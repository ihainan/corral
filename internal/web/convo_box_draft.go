package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// screenBlocker describes a terminal condition currently blocking normal chat
// messages from the web composer, as read off a pane's ANSI screen.
// Code == "" means the main input box is visible and empty (clear to send).
type screenBlocker struct {
	Code   string `json:"code"`
	Draft  string `json:"draft,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// POST /api/sessions/{id}/conversation/box-draft
// {"pane","pid","start","session","action":"send"|"clear","digest":"<64-hex>"}
//
// Acts on unsent text detected in Claude Code's or Codex's terminal input box:
// "send" presses Enter, "clear" presses Ctrl+C. Both verify the screen twice
// (gateway pre-check against digest, then atomic host-side box:<digest> or
// cscreen:<digest> gate) so if the screen changes meanwhile, nothing is pressed.
func (s *Server) apiConversationBoxDraft(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "box_draft")
}

// herdrAgentKind maps a herdr pane's agent label to the canonical agent kind
// used by screen inspection ("claude", "codex", "opencode", "agy").
func herdrAgentKind(agent string, agyPane bool) string {
	switch {
	case agyPane || isAgyAgent(agent):
		return "agy"
	case isCodexAgent(agent):
		return "codex"
	case isOpenCodeAgent(agent):
		return "opencode"
	default:
		return "claude"
	}
}

// codexDraftText extracts recognizable unsent draft text from Codex's colored
// input box. Returns ok=false when the box is empty, holds only a dim
// placeholder, holds a folded paste ("[Pasted Content N chars]"), or cannot be
// cleanly distinguished from a menu.
func codexDraftText(ansiScreen string) (string, bool) {
	norm := strings.ReplaceAll(strings.ReplaceAll(ansiScreen, "\x00", ""), "\r", "")
	if !codexHasDraft(norm) {
		return "", false
	}
	plain := sgr.ReplaceAllString(norm, "")
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	at, foot, _, ok := codexInputSpan(lines)
	if !ok {
		return "", false
	}
	parts := make([]string, 0, foot-at)
	for i := at; i < foot; i++ {
		l := lines[i]
		if i == at {
			l = strings.TrimPrefix(strings.TrimPrefix(l, "›"), " ")
		}
		if t := strings.TrimSpace(l); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	joined := strings.Join(parts, " ")
	if codexEmptyInput(joined) || codexPasted.MatchString(squeeze(joined)) {
		return "", false
	}
	return joined, true
}

var claudeAskHintRe = regexp.MustCompile(`(?i)\b(?:Esc to cancel|Enter to (?:select|confirm|continue|submit)|Do you want to proceed\?|Review your answers)\b`)

func claudeScreenAsks(plain string) bool {
	if lineAsks(plain, "") {
		return true
	}
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	start := max(0, len(lines)-25)
	for _, l := range lines[start:] {
		if askOptLine.MatchString(l) || claudeAskHintRe.MatchString(l) {
			return true
		}
	}
	return false
}

// inspectScreenBlocker classifies whether a pane's ANSI screen currently has a
// terminal blocker (unsent draft, subagent/other view, picker, waiting dialog,
// or missing input box) or is clear for chat sending (Code == "").
func inspectScreenBlocker(agent, ansiScreen string, waiting bool) screenBlocker {
	if agent == "" {
		agent = "claude"
	}
	norm := strings.ReplaceAll(strings.ReplaceAll(ansiScreen, "\x00", ""), "\r", "")
	plain := sgr.ReplaceAllString(norm, "")

	switch agent {
	case "claude":
		switch claudeOwner(plain, false) {
		case "subagent":
			return screenBlocker{Code: "subagent_view"}
		case "list", "sessions":
			return screenBlocker{Code: "other_view"}
		case "main":
			if ls, ok := boxDraftLines(norm); ok && len(ls) > 0 {
				return screenBlocker{
					Code:   "box_draft",
					Draft:  clip(strings.Join(ls, " "), 200),
					Digest: boxDigest(plain),
				}
			}
			if waiting {
				return screenBlocker{Code: "agent_waiting"}
			}
			return screenBlocker{}
		default: // "none"
			if pickerShows(plain) {
				return screenBlocker{Code: "picker"}
			}
			if waiting || claudeScreenAsks(plain) {
				return screenBlocker{Code: "agent_waiting"}
			}
			return screenBlocker{Code: "no_input"}
		}

	case "codex":
		if codexHasDraft(norm) {
			if d, ok := codexDraftText(norm); ok {
				_, dig, _ := screenOf([]byte(plain))
				return screenBlocker{Code: "draft", Draft: clip(d, 200), Digest: dig}
			}
			return screenBlocker{Code: "draft"}
		}
		lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
		if _, _, _, ok := codexInputSpan(lines); ok {
			if waiting {
				return screenBlocker{Code: "agent_waiting"}
			}
			return screenBlocker{}
		}
		if parseCodexPicker(plain).ok {
			return screenBlocker{Code: "picker"}
		}
		if waiting || codexPlanPrompt(plain) {
			return screenBlocker{Code: "agent_waiting"}
		}
		return screenBlocker{Code: "no_input"}

	case "agy":
		switch err := agyIdleInputColor(norm); {
		case err == nil:
			if waiting {
				return screenBlocker{Code: "agent_waiting"}
			}
			return screenBlocker{}
		case errors.Is(err, errDraft):
			return screenBlocker{Code: "draft"}
		case errors.Is(err, errAgyAsks):
			return screenBlocker{Code: "agent_waiting"}
		default:
			return screenBlocker{Code: "no_input"}
		}

	case "opencode":
		switch err := ocIdleInput(plain); {
		case err == nil:
			if waiting {
				return screenBlocker{Code: "agent_waiting"}
			}
			return screenBlocker{}
		case errors.Is(err, errDraft):
			return screenBlocker{Code: "draft"}
		case errors.Is(err, errOcAsks):
			return screenBlocker{Code: "agent_waiting"}
		default:
			return screenBlocker{Code: "no_input"}
		}

	default:
		return screenBlocker{}
	}
}

// boxDraftAction safely sends (Enter) or clears (Ctrl+C) an unsent terminal
// draft after verifying the pane screen matches wantDigest both on the gateway
// and atomically on the host before pressing any key.
func (s *Server) boxDraftAction(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket, agent string, pid, start int64, sid, action, wantDigest string, outcome, reason *string, release func()) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var held sync.WaitGroup
	held.Add(1)
	defer func() {
		go func() {
			held.Wait()
			release()
		}()
	}()
	res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: held.Done, Interactive: true, AwaitSlot: 2 * time.Second})
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			*outcome, *reason = "refused", "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*outcome, *reason = "unknown", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	scr := strings.ReplaceAll(strings.ReplaceAll(string(res.Out), "\x00", ""), "\r", "")
	plain := sgr.ReplaceAllString(scr, "")

	var gate string
	switch agent {
	case "claude":
		if claudeOwner(plain, false) != "main" {
			*outcome, *reason = "refused", "screen_changed"
			writeError(w, http.StatusConflict, "screen_changed", "the screen changed; look again before acting")
			return
		}
		ls, ok := boxDraftLines(scr)
		if !ok {
			*outcome, *reason = "refused", "screen_changed"
			writeError(w, http.StatusConflict, "screen_changed", "the screen changed; look again before acting")
			return
		}
		if len(ls) == 0 {
			*outcome, *reason = "refused", "draft_gone"
			writeError(w, http.StatusConflict, "draft_gone", "the terminal input box is already empty")
			return
		}
		curDigest := boxDigest(plain)
		if curDigest == "" || curDigest != wantDigest {
			*outcome, *reason = "refused", "screen_changed"
			extra := map[string]any{"draft": clip(strings.Join(ls, " "), 200)}
			if curDigest != "" {
				extra["digest"] = curDigest
			}
			writeErrorExtra(w, http.StatusConflict, "screen_changed", "the terminal input changed; review it before acting", extra)
			return
		}
		gate = "box:" + curDigest

	case "codex":
		d, ok := codexDraftText(scr)
		if !ok {
			if codexIdleInput(plain) == nil {
				*outcome, *reason = "refused", "draft_gone"
				writeError(w, http.StatusConflict, "draft_gone", "the terminal input box is already empty")
				return
			}
			*outcome, *reason = "refused", "screen_changed"
			writeError(w, http.StatusConflict, "screen_changed", "the screen changed; look again before acting")
			return
		}
		_, curDigest, _ := screenOf([]byte(plain))
		if curDigest == "" || curDigest != wantDigest {
			*outcome, *reason = "refused", "screen_changed"
			extra := map[string]any{"draft": clip(d, 200)}
			if curDigest != "" {
				extra["digest"] = curDigest
			}
			writeErrorExtra(w, http.StatusConflict, "screen_changed", "the terminal input changed; review it before acting", extra)
			return
		}
		gate = "cscreen:" + curDigest

	default:
		*outcome, *reason = "refused", "unsupported"
		writeError(w, http.StatusConflict, "unsupported", "not for this agent")
		return
	}

	keys := []string{"enter"}
	if action == "clear" {
		keys = []string{"ctrl+c"}
	}
	if sess.Persistence == "tmux" {
		if action == "clear" {
			keys = []string{"C-c"}
		} else {
			keys = []string{"Enter"}
		}
	}

	held.Add(1)
	done := held.Done
	if agent == "codex" {
		s.codexPressed(sess, pane)
		defer s.codexPressed(sess, pane)
		done = func() {
			s.codexPressed(sess, pane)
			held.Done()
		}
	}
	// AwaitSlot: 0 so a busy connection refuses immediately rather than pressing
	// Enter or Ctrl+C after the user may have edited the input box.
	_, err = s.runGated(ctx, sess, gatedReq{
		Gate:      gate,
		Mode:      sess.Persistence,
		Session:   sess.TmuxSession,
		Pane:      pane,
		Socket:    socket,
		PID:       pid,
		Start:     start,
		SID:       sid,
		LenSpec:   "keys",
		Keys:      keys,
		Read:      sendLines,
		MaxOut:    sendOut,
		Release:   done,
		AwaitSlot: 0,
		What:      "box_draft",
	})
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
	case errors.Is(err, gw.ErrUnknownResult):
		*outcome, *reason = "unknown", "corrald_unknown_result"
		writeCorraldUnknownResult(w)
		return
	case errors.Is(err, sshx.ErrBusy):
		*outcome, *reason = "refused", "ssh_channels_busy"
		writeSSHChannelsBusy(w)
		return
	case errors.As(err, &exitErr) && exitErr.Status == 10:
		*outcome, *reason = "refused", "screen_changed"
		writeError(w, http.StatusConflict, "screen_changed", "the screen changed; look again before acting")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 9:
		*outcome, *reason = "refused", "agent_waiting"
		writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 11:
		*outcome, *reason = "refused", "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "the host cannot verify the screen: handle it in the terminal")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 7:
		*outcome, *reason = "refused", "busy"
		writeError(w, http.StatusConflict, "busy", "the pane is busy with another request: try again")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 4:
		*outcome, *reason = "refused", "send_failed"
		writeError(w, http.StatusBadGateway, "send_failed", "nothing could be sent to the pane")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*outcome, *reason = "refused", "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	case errors.As(err, &exitErr) && exitErr.Status == exitOwnerUnread:
		*outcome, *reason = "refused", "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	case errors.As(err, &exitErr) && exitErr.Status >= exitOwnerSubagent && exitErr.Status <= exitOwnerLast:
		*outcome = "refused"
		*reason, _, _ = writeOwnerRefusal(w, err)
		return
	default:
		*outcome, *reason = "unknown", "no_answer"
		writeError(w, http.StatusBadGateway, "no_answer", "the host did not answer")
		return
	}
	*outcome = "typed"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action})
}
