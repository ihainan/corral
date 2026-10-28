package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Setting how hard an agent is asked to think, through Claude Code's own
// /effort, for the session only.
//
// Its screen is a slider: "low medium high xhigh max" under a rule with a
// ▲ on the one chosen. Enter there would make that the default of every
// new session too and is never pressed; "s" takes it for this session
// alone, which is all this does. The log says what it is on every answer
// (Meta.Effort), so nothing is pressed to read it.

// effortLevels: the slider's, left to right, as the log and Claude Code
// name them.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

func effortLevel(s string) int { return slices.Index(effortLevels, s) }

const (
	effortLines = 24 // (of the screen read after a step: the slider and what is under it)
	// effortKeys is in the slider's line of keys, as in nothing else (the
	// model picker's says "s to use this session only").
	effortKeys = "s for this session only"
)

// effortShows: the slider is on the screen, by its title and its line of
// keys. A narrow pane wraps both of those mid-word, so the screen is read
// with every space and line break taken out: what is left holds each of
// them whole however it was broken. (The model picker's line of keys
// reads "s to use this session only", so it is no slider.)
func effortShows(screen string) bool {
	flat := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == 0xa0 {
			return -1
		}
		return r
	}, screen)
	return strings.Contains(flat, "Effort") && strings.Contains(flat, strings.ReplaceAll(effortKeys, " ", ""))
}

// effortRow matches the slider's line of names, however it is indented.
var effortRow = regexp.MustCompile(`low\s+medium\s+high\s+xhigh\s+max`)

// effortAt reads which level the slider's ▲ stands on: the mark's
// column against the columns the names are centred on (the rule above the
// names is drawn in ─, one column each, as the mark is).
//
// A narrow pane wraps both lines, so the mark is looked for a few lines up
// and only where that line is indented as the names are: the tail of a
// wrapped line begins at the left and its columns would say nothing. ""
// when the screen shows no slider, or none that can be read this way.
func effortAt(screen string) string {
	lines := strings.Split(screen, "\n")
	indent := func(l string) int { return len([]rune(l)) - len([]rune(strings.TrimLeft(l, " "))) }
	for i, l := range lines {
		if !effortRow.MatchString(l) {
			continue
		}
		mark := -1
		for j := i - 1; j >= 0 && j >= i-3; j-- {
			if at := strings.IndexRune(lines[j], '▲'); at >= 0 && indent(lines[j]) == indent(l) {
				mark = len([]rune(lines[j][:at]))
				break
			}
		}
		if mark < 0 {
			continue
		}
		best, how, from := "", 0, 0
		for _, name := range effortLevels {
			// (Each name looked for after the one before it: "xhigh" holds
			// "high", and the names come in this order.)
			j := strings.Index(l[from:], name)
			if j < 0 {
				continue
			}
			j += from
			from = j + len(name)
			mid := len([]rune(l[:j])) + len([]rune(name))/2
			d := mid - mark
			if d < 0 {
				d = -d
			}
			if best == "" || d < how {
				best, how = name, d
			}
		}
		return best
	}
	return ""
}

// effortTook: the level the slider says it took, as it is said over the
// input after "s" ("◐ medium · /effort"); "" when it says none.
//
// The last of them, not the first: what it said of earlier switches stays
// in the thread above, and only the latest is of this one.
func effortTook(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		t := strings.TrimSpace(l)
		name, ok := strings.CutSuffix(t, "· /effort")
		if !ok {
			continue
		}
		f := strings.Fields(name)
		if len(f) > 0 && effortLevel(f[len(f)-1]) >= 0 {
			return f[len(f)-1]
		}
	}
	return ""
}

var (
	errNoEffort    = errors.New("the effort slider did not show")
	errEffortWhere = errors.New("the slider did not go where it was moved to")
	errEffortTaken = errors.New("the slider did not take the choice")
)

// effortFlow opens the slider and takes target for this session. It acts
// only on Claude Code at rest, its input empty, nothing of its own open
// (what is open is closed with Escape first). The slider is moved to the
// far left and then right to target: it stops at either end, so where it
// stood need not be guessed at. On a failure after it opened, Escape is
// pressed (best effort, by leave).
func effortFlow(step, leave modelStep, target string) (string, error) {
	want := effortLevel(target)
	if want < 0 {
		return "", errNoEffort
	}
	out := func(e error) (string, error) {
		for i := 0; i < 2; i++ {
			if scr, err := leave([]string{"esc"}, "", effortLines); err != nil || !effortShows(scr) {
				break
			}
		}
		return "", e
	}
	screen, err := step([]string{}, "", effortLines)
	if err != nil {
		return "", err
	}
	open := func(s string) bool { return effortShows(s) || pickerShows(s) || askShows(s, "") }
	for i := 0; i < 2 && open(screen); i++ {
		// (Escape on its own while where the request's is spent: what was
		// open before this began is not left open behind it.)
		if screen, err = step([]string{"esc"}, "", effortLines); err != nil {
			return out(err)
		}
	}
	if open(screen) {
		return out(errScreen)
	}
	if err := idleInput(screen); err != nil {
		return "", err
	}
	if screen, err = step(nil, "/effort", effortLines); err != nil {
		if gateRefused(err) {
			return "", err // (it came to wait: no picker opened; an Escape would answer it)
		}
		return out(err)
	}
	if !effortShows(screen) {
		return out(errNoEffort)
	}
	keys := make([]string, 0, len(effortLevels)-1+want)
	for i := 0; i < len(effortLevels)-1; i++ { // (to the far left, wherever it stood)
		keys = append(keys, "left")
	}
	for i := 0; i < want; i++ {
		keys = append(keys, "right")
	}
	if len(keys) > 0 {
		if screen, err = step(keys, "", effortLines); err != nil {
			return out(err)
		}
	}
	if !effortShows(screen) {
		return out(errNoEffort)
	}
	// (Where the mark can be read, it must be where it was moved to; a
	// screen too narrow to read it is left to what the slider says it took.)
	if at := effortAt(screen); at != "" && at != target {
		return out(errEffortWhere)
	}
	if screen, err = step([]string{"this"}, "", effortLines); err != nil {
		return out(err)
	}
	if effortShows(screen) {
		return out(errEffortTaken)
	}
	switch took := effortTook(screen); {
	case took == target:
		return took, nil
	case took != "":
		return took, errEffortWhere // (it took one, but another: said as it is)
	}
	return "", errUnsure // (the slider gone, nothing said of it: as it may have been taken, no Escape)
}

// convoEffort runs effortFlow in the agent's pane, each step with the
// checks of convoSendCommand (the same process in front there). It holds
// the pane's turn (release gives it back) until the last step's command
// has ended on the host, even where the request was given up before.
func (s *Server) convoEffort(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target string, outcome, reason *string, release func()) {
	var running sync.WaitGroup // (steps whose command may still run on the host)
	defer func() {
		go func() {
			running.Wait()
			release()
		}()
	}()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	typed := false
	run := func(ctx context.Context, keys []string, text string, read int) (string, error) {
		var press []string
		if keys != nil {
			press = []string{} // (none: the screen only)
		}
		for _, k := range keys {
			if sess.Persistence == "herdr" {
				press = append(press, modelKeys[k][0])
			} else {
				press = append(press, modelKeys[k][1])
			}
		}
		// (A text — the command that opens its picker — not while it waits:
		// its Enter would be a question's. Keys within the picker go: it is
		// itself a dialog Claude Code waits on.)
		gate := ""
		if text != "" {
			gate = "free"
		}
		running.Add(1)
		res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,
			sshx.ExecOptions{Stdin: strings.NewReader(text), Release: running.Done})
		if err == nil && (len(press) > 0 || text != "") {
			typed = true
		}
		return string(res.Out), err
	}
	step := func(keys []string, text string, read int) (string, error) { return run(ctx, keys, text, read) }
	// (Leaving the slider after a failure: once a step given up has ended,
	// on its own short while, the request's may be over.)
	leave := func(keys []string, text string, read int) (string, error) {
		running.Wait()
		lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer lcancel()
		return run(lctx, keys, text, read)
	}
	took, err := effortFlow(step, leave, target)
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
		*outcome = "typed"
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "effort": took})
	case errors.Is(err, sshx.ErrBusy) || errors.As(err, &exitErr) && exitErr.Status == 7:
		*outcome, *reason = "refused", "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case errors.Is(err, errBusyAgent), errors.Is(err, errDraft):
		*outcome, *reason = "refused", "busy"
		writeError(w, http.StatusConflict, "busy", "the agent is working, or its input holds text not sent")
	case errors.Is(err, errEffortWhere) && took != "":
		*outcome, *reason = "unknown", "elsewhere"
		writeErrorExtra(w, http.StatusBadGateway, "elsewhere", "the slider took another level", map[string]any{"effort": took})
	case errors.Is(err, errNoEffort):
		*outcome, *reason = said(typed), "no_effort"
		writeError(w, http.StatusConflict, "no_effort", "this Claude Code has no effort to set")
	case errors.Is(err, errUnsure):
		*outcome, *reason = "unknown", "unsure"
		writeError(w, http.StatusBadGateway, "unsure", "it is not known whether the effort was taken")
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*outcome, *reason = "refused", "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
	case errors.As(err, &exitErr) && (exitErr.Status == 9 || exitErr.Status == 5):
		*outcome, *reason = said(typed || exitErr.Status == 5), "agent_waiting"
		writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
	case ctx.Err() != nil:
		*outcome, *reason = said(typed), "timeout"
		writeError(w, http.StatusGatewayTimeout, "timeout", "the agent did not answer in time")
	default:
		*outcome, *reason = said(typed), "screen"
		writeError(w, http.StatusBadGateway, "screen", "its screen is not as expected: nothing was set")
	}
}

// said: what became of an attempt that failed after something was typed.
func said(typed bool) string {
	if typed {
		return "unknown"
	}
	return "refused"
}

// POST /api/sessions/{id}/conversation/effort (the same as interrupt, and
// "effort": one of effortLevels)
//
// The session asked to think that hard, for this session only
// ({"ok":true,"effort":…}): see effortFlow. Its log then says so on every
// answer that follows.
func (s *Server) apiConversationEffort(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "effort")
}
