package web

import (
	"context"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What a Codex at work says only on its screen (its log says nothing of it
// till it is over, 11-15 parity audit): its request failing and tried again
// ("• Reconnecting... 2/5 (7s • esc to interrupt)", the reason on the line
// under it: "└ Exceeded retry limit, last status: 429 Too Many Requests"),
// or compacting the conversation ("• Compacting context (4s • esc to
// interrupt)"). Only its status line now counts: the last such line, over
// its input box (the same words higher up are the conversation's).

// codexStatusLine: Codex's line of what it does now, its mark blinking
// ("•"/"◦"), its time, and what else goes on after it (" · 1 background
// terminal running · …").
var codexStatusLine = regexp.MustCompile(`^[•◦] (\S.*?) \((?:\d+[hm] )*\d+s • esc to interrupt\)(?: · .*)?$`)

var codexRetrying = regexp.MustCompile(`^Reconnecting\.\.\. (\d{1,3})/(\d{1,3})$`)

type codexRetry struct {
	N   int    `json:"n"`
	Of  int    `json:"of"`
	Why string `json:"why,omitempty"`
}

// codexWorkNote: what a Codex's screen (without its colours) says it does
// that its log does not: compacting, or a request tried again (n of m, and
// why); neither where its status line says other.
func codexWorkNote(screen string) (compacting bool, retry *codexRetry) {
	compacting, retry, _ = codexWorkNoteAll(screen)
	return
}

// codexWorkNoteAll: codexWorkNote, and the command its approvals reviewer
// looks at now ("• Reviewing approval request (5s …)", the command on the
// line under it: approvals_reviewer = auto_review), "" none.
func codexWorkNoteAll(screen string) (compacting bool, retry *codexRetry, review string) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	// (Above its input box only — its last line beginning "›": what is in
	// the box, typed or pasted, is no word of what it does.)
	box := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "›") {
			box = i
			break
		}
	}
	for i := box - 1; i >= 0; i-- {
		m := codexStatusLine.FindStringSubmatch(strings.TrimRight(lines[i], " "))
		if m == nil {
			continue
		}
		what := m[1]
		under := ""
		if i+1 < len(lines) {
			if w, ok := strings.CutPrefix(strings.TrimSpace(lines[i+1]), "└ "); ok {
				under = strings.TrimSpace(w)
			}
		}
		if strings.HasPrefix(what, "Compacting context") {
			return true, nil, ""
		}
		if what == "Reviewing approval request" {
			return false, nil, clipRunes(under, 200)
		}
		if r := codexRetrying.FindStringSubmatch(what); r != nil {
			n, _ := strconv.Atoi(r[1])
			of, _ := strconv.Atoi(r[2])
			if n < 1 || of < n {
				return false, nil, ""
			}
			return false, &codexRetry{N: n, Of: of, Why: clipRunes(under, 200)}, ""
		}
		return false, nil, ""
	}
	return false, nil, ""
}

// codexPlanPrompt: Codex's screen asks whether to carry out the plan it
// proposed (plan mode, 0.167): "Implement this plan?", its menu under it
// ("› 1. Yes, implement this plan", …, "enter select · esc back") the
// screen's last lines.
func codexPlanPrompt(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	for i := end - 1; i >= 0 && end-i <= 12; i-- {
		if !codexMenuChoice.MatchString(lines[i]) {
			continue
		}
		top := i // (the menu's first choice: the marked one may be any)
		for top > 0 && codexMenuNext.MatchString(lines[top-1]) {
			top--
		}
		for j := top - 1; j >= 0 && top-j <= 3; j-- {
			if strings.TrimSpace(lines[j]) == "Implement this plan?" {
				return codexMenuKeysUnder(lines, i, end)
			}
		}
		return false
	}
	return false
}

// codexMenuKeysUnder: Codex's menu keys under line at (its last choice may
// be the marked one: no next choice under it).
func codexMenuKeysUnder(lines []string, at, end int) bool {
	for _, l := range lines[at+1 : end] {
		if codexMenuKeys.MatchString(l) {
			return true
		}
	}
	return false
}

// codexPlanAsked: the pane's screen now asks "Implement this plan?" and is
// the one the page answered (digest): its answer goes by that screen alone,
// checked on the host again right before the key — Codex's title does not
// say it waits for this one (0.167, 11-15).
func (s *Server) codexPlanAsked(ctx context.Context, sess *term.Session, pane, socket, digest string) bool {
	if digest == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{})
	if err != nil {
		return false
	}
	text, d, _ := screenOf(res.Out)
	return d == digest && codexPlanPrompt(text)
}
