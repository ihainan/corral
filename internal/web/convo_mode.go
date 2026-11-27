package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// codexLooks: Codex's commands that only show something in its terminal
// (its status, its background terminals, its changes, its MCP servers),
// none of it in its log: run from the page, its terminal shown there.
var codexLooks = map[string]bool{"/status": true, "/ps": true, "/diff": true, "/mcp": true}

// convoModes: the permission modes a Claude Code may be switched to from
// the agents page (the ones Shift+Tab goes through; which a session has
// depends on how it was started).
var convoModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "auto": true, "bypassPermissions": true, "build": true}

// convoModeMost: the presses at most on the way to a mode (there are not
// that many to go through).
const convoModeMost = 6

// modeOnScreen is the permission mode the screen shows, by its last lines
// only (its mode line and input box are at its foot; the thread above may
// quote either): its mode line's, else "default" where its input box is
// there and no mode line (an older Claude Code says nothing of that one);
// "" where it cannot be told (a question in front of it, say).
func modeOnScreen(screen string) string {
	return modeOnScreenOf("claude", screen)
}

// modeOnScreenOf: modeOnScreen for agent's screen — a Codex's, its
// collaboration mode: "plan" where its footer says "Plan mode" (at the
// right of its status line, "(shift+tab to cycle)" after it where there is
// room: 0.167, 11-15), "default" where its input is there without it; ""
// where its input is not told apart (a menu, a dialog in front of it).
func modeOnScreenOf(agent, screen string) string {
	if agent == "agy" {
		return agyModeOnScreen(screen)
	}
	if agent == "codex" {
		lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
		_, foot, footed, ok := codexInputSpan(lines)
		if !ok || !footed {
			return ""
		}
		for _, l := range lines[foot:] {
			if codexPlanMode.MatchString(l) {
				return "plan"
			}
		}
		return "default"
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	foot := strings.Join(lines[max(0, len(lines)-modeFoot):], "\n")
	if m := screenMode(foot); m != "" {
		return m
	}
	if inputArea(foot) != "" {
		return "default"
	}
	return ""
}

// codexPlanMode: Codex's footer saying it is in plan mode, at the right.
// (0.169: at the right of its hints' line, notes after it: "· ⚠ 1 warning · f2 to view".)
var codexPlanMode = regexp.MustCompile(`(?:^|\s{2,}|… )Plan mode(?: \(shift\+tab to cycle\))?(?:\s*$| · )`)

// codexModes: the collaboration modes a Codex may be switched to (Shift+Tab
// goes from one to the other).
var codexModes = map[string]bool{"default": true, "plan": true}

// agyModes: the execution modes Antigravity CLI cycles through on Shift+Tab.
var agyModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "auto": true}

// modeFoot: the screen's last lines its mode and input box are looked for
// in (the input's box, its mode line and a status line of a few lines).
const modeFoot = 12

// convoModeTo switches a Claude Code to the permission mode target: the
// screen read first (there already: nothing pressed), then Shift+Tab, the
// screen read after each, until it shows target. Back at the mode it was in
// without having shown target, it has no such mode, and is left as it was
// ({"ok":true,"mode":…,"missing":true}); a screen that does not say stops it
// where it is ({"ok":true,"mode":""}); presses enough to go round without
// either, it stops where it is ({…,"incomplete":true}). "pressed" says
// whether any key was. Each press is checked on the host as
// any other (see convoSendCommand): another process there, nothing more is
// pressed. The pane's turn is given back once the last command has ended on
// the host.
func (s *Server) convoModeTo(w http.ResponseWriter, ctx context.Context, sess *term.Session, agent, pane, socket string, pid, start int64, sid string, press []string, target string, outcome, reason *string, release func()) {
	gate := "free"
	if agent == "codex" {
		gate = "cfree" // (whether Codex waits, its title says)
	} else if agent == "agy" {
		gate = "amode" // (agy's input box must be visible; Shift+Tab cycles mode)
	}
	var ended sync.WaitGroup
	defer func() {
		go func() {
			ended.Wait()
			release()
		}()
	}()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	ended.Add(1)
	scrCmd := convoScreenCommand(sess.Persistence, pane, socket)
	if agent == "agy" {
		scrCmd = agyScreenCommand(sess.Persistence, pane, socket)
	}
	res, err := sshx.Exec(ctx, sess.Client(), scrCmd, 256<<10, sshx.ExecOptions{Release: ended.Done, Interactive: true, AwaitSlot: 2 * time.Second})
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			*reason = "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*reason = "no_screen"
		writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
		return
	}
	from := modeOnScreenOf(agent, string(res.Out))
	if from == "" {
		*outcome, *reason = "looked", "unsure"
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "", "pressed": false})
		return
	}
	if from == target {
		*outcome = "looked"
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": from, "pressed": false})
		return
	}
	now := from
	for i := 0; i < convoModeMost; i++ {
		ended.Add(1)
		// (Not while it waits: Shift+Tab is a question's then. Exit 9.)
		// Terminal keypress after checking screen mode: intentionally AwaitSlot: 0
		// so a busy connection refuses immediately rather than pressing against a stale screen.
		res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, press, convoModeLines), 16<<10,
			sshx.ExecOptions{Stdin: strings.NewReader(""), Release: ended.Done, Interactive: true})
		var exitErr *sshx.ExitError
		switch {
		case err == nil:
			*outcome = "typed"
		case i > 0:
			// (Some pressed already: where it is now is not known.)
			*outcome, *reason = "unknown", "stopped"
			writeError(w, http.StatusBadGateway, "unknown", "the switch stopped on the way")
			return
		case errors.Is(err, sshx.ErrBusy):
			*reason = "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		case errors.As(err, &exitErr) && exitErr.Status == 7:
			*reason = "busy"
			writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
			return
		case errors.As(err, &exitErr) && exitErr.Status == 3:
			*reason = "agent_changed"
			writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
			return
		case errors.As(err, &exitErr) && exitErr.Status == 9:
			*reason = "agent_waiting"
			writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
			return
		case errors.As(err, &exitErr) && exitErr.Status == 4:
			*reason = "send_failed"
			writeError(w, http.StatusConflict, "refused", "the pane did not take the key")
			return
		case errors.As(err, &exitErr) && exitErr.Status == exitOwnerUnread:
			*reason = "no_screen"
			writeError(w, http.StatusBadGateway, "no_screen", "the pane's screen could not be read")
			return
		case errors.As(err, &exitErr) && exitErr.Status >= exitOwnerSubagent && exitErr.Status <= exitOwnerLast:
			*reason, _, _ = writeOwnerRefusal(w, err) // (Claude Code's input box not the conversation's: nothing pressed)
			return
		default:
			*outcome = "unknown"
			writeError(w, http.StatusBadGateway, "unknown", "it is not known whether the key arrived")
			return
		}
		now = modeOnScreenOf(agent, string(res.Out))
		// (A Codex's: one press goes to its other mode, its footer drawn a while
		// after — longer in 0.169 —: read again for it before taking it for
		// missing, or pressing again, back where it began.)
		for j := 0; (agent == "codex" || agent == "agy") && now == from && j < 6; j++ {
			time.Sleep(400 * time.Millisecond)
			// Fast 400ms screen-redraw poll loop: intentionally AwaitSlot: 0 so
			// a busy tick yields immediately to the next poll iteration.
			if r, err := sshx.Exec(ctx, sess.Client(), scrCmd, 256<<10, sshx.ExecOptions{Interactive: true}); err == nil {
				now = modeOnScreenOf(agent, string(r.Out))
			}
		}
		switch now {
		case target:
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": now, "pressed": true})
			return
		case "":
			*reason = "unsure"
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "", "pressed": true})
			return
		case from:
			*reason = "missing"
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": now, "pressed": true, "missing": true})
			return
		}
	}
	// (Neither there nor back where it began: switched meanwhile from
	// elsewhere, say. Where it is, as the screen last said.)
	*reason = "incomplete"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": now, "pressed": true, "incomplete": true})
}

// convoModeLines: the screen's last lines read after each press, enough
// for its input box under a status line of several lines (see modeOnScreen).
const convoModeLines = 24
