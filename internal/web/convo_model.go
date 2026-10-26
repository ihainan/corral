package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Switching an agent's model from the agents page, through Claude Code's
// own picker (/model), for the session only.
//
// /model with a name, Enter on the picker, or a digit there each save the
// model as the default of every new session too: none of them is ever
// pressed. The picker is opened, read off the screen, the choice moved to
// with the arrows, and taken with "s" (this session only); the question
// Claude Code may ask then ("Switch model?", the history to be read again)
// is the only place Enter is pressed. Each step is read before the next;
// anything not as expected is left with Escape.

// modelOpt is one of the picker's models, as it shows them.
type modelOpt struct {
	N       int    `json:"-"` // its number in the picker
	Label   string `json:"label"`
	Desc    string `json:"desc,omitempty"`
	Current bool   `json:"current,omitempty"` // (the session's now: ✔)
	at      bool   // the picker's cursor is on it
}

// modelPicker is what the screen shows of the picker; ok false when it
// shows none.
type modelPicker struct {
	opts []modelOpt
	ok   bool
}

var (
	pickerRow  = regexp.MustCompile(`^\s*(❯|↓|↑)?\s*(\d{1,2})\.\s+(.+?)\s*$`)
	pickerGaps = regexp.MustCompile(`\s{2,}`)
)

// parseModelPicker reads the picker off the screen. Its first entry
// ("Default (recommended)": the default of new sessions, whichever that is)
// is left out; so are those it does not show (scrolled away).
func parseModelPicker(screen string) modelPicker {
	if !strings.Contains(screen, "Select model") || !strings.Contains(screen, "this session only") {
		return modelPicker{}
	}
	var p modelPicker
	for _, line := range strings.Split(screen, "\n") {
		m := pickerRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		f := pickerGaps.Split(m[3], 2)
		label := strings.TrimSpace(f[0])
		o := modelOpt{N: n, at: m[1] == "❯"}
		if l, ok := strings.CutSuffix(label, "✔"); ok {
			label, o.Current = strings.TrimSpace(l), true
		}
		o.Label = label
		if len(f) > 1 {
			o.Desc = strings.TrimSpace(f[1])
		}
		if strings.HasPrefix(label, "Default") || label == "" || len(label) > 64 {
			if o.at { // (the cursor is there: known, though not offered)
				p.opts = append(p.opts, modelOpt{N: n, at: true})
			}
			continue
		}
		p.opts = append(p.opts, o)
	}
	p.ok = len(p.opts) > 0
	return p
}

func (p modelPicker) cursor() (modelOpt, bool) {
	for _, o := range p.opts {
		if o.at {
			return o, true
		}
	}
	return modelOpt{}, false
}

// offered: the models to pick from (not the cursor's place holder).
func (p modelPicker) offered() []modelOpt {
	out := []modelOpt{}
	for _, o := range p.opts {
		if o.Label != "" {
			out = append(out, o)
		}
	}
	return out
}

// The keys the model flow may press, by what they are for (herdr's name,
// tmux's). No digit, and Enter only for the question after "s".
var modelKeys = map[string][2]string{
	"clear": {"ctrl+u", "C-u"}, // (the input's line: Ctrl+Y gives it back in Claude Code)
	"down":  {"down", "Down"},
	"up":    {"up", "Up"},
	"this":  {"s", "s"},
	"esc":   {"esc", "Escape"},
	"yes":   {"enter", "Enter"},
}

// modelStep runs one step on the host: keys (names of modelKeys) or a text
// typed and Enter, then the last lines of the screen.
type modelStep func(keys []string, text string, read int) (string, error)

var (
	errNoPicker = errors.New("the model picker did not show")
	errNoModel  = errors.New("no such model in the picker")
	errNotThere = errors.New("the picker's choice is not where it was moved to")
	errNotTaken = errors.New("the picker did not take the choice")
	pickerYes   = regexp.MustCompile(`❯\s*1\.\s*Yes`)
)

const (
	pickerLines   = 40 // (of the screen read after a step: the picker and what is under it)
	maxPickerMove = 20
)

// modelFlow opens the picker; with target "" it lists its models and closes
// it, else it switches to target for this session. On a failure after the
// picker opened, Escape is pressed (best effort).
func modelFlow(step modelStep, target string) (opts []modelOpt, err error) {
	if _, err := step([]string{"clear"}, "", 0); err != nil {
		return nil, err
	}
	screen, err := step(nil, "/model", pickerLines)
	if err != nil {
		return nil, err
	}
	pk := parseModelPicker(screen)
	leave := func(e error) ([]modelOpt, error) {
		step([]string{"esc"}, "", 0)
		return nil, e
	}
	if !pk.ok {
		return leave(errNoPicker)
	}
	if target == "" {
		step([]string{"esc"}, "", 0)
		return pk.offered(), nil
	}
	var to modelOpt
	found := false
	for _, o := range pk.offered() {
		if o.Label == target {
			to, found = o, true
		}
	}
	at, known := pk.cursor()
	if !found || !known {
		return leave(errNoModel)
	}
	if d := to.N - at.N; d != 0 {
		key := "down"
		if d < 0 {
			key, d = "up", -d
		}
		if d > maxPickerMove {
			return leave(errNoModel)
		}
		keys := make([]string, d)
		for i := range keys {
			keys[i] = key
		}
		screen, err = step(keys, "", pickerLines)
		if err != nil {
			return leave(err)
		}
		now := parseModelPicker(screen)
		if c, ok := now.cursor(); !now.ok || !ok || c.Label != target {
			return leave(errNotThere)
		}
	}
	screen, err = step([]string{"this"}, "", pickerLines)
	if err != nil {
		return leave(err)
	}
	if strings.Contains(screen, "Switch model?") {
		if !pickerYes.MatchString(screen) {
			return leave(errNotTaken)
		}
		if screen, err = step([]string{"yes"}, "", pickerLines); err != nil {
			return nil, err
		}
	}
	if strings.Contains(screen, "Switch model?") || parseModelPicker(screen).ok {
		return leave(errNotTaken)
	}
	return []modelOpt{to}, nil
}

// convoModel runs modelFlow in the agent's pane, each step with the checks
// of convoSendCommand (the same process in front there), within the pane's
// turn the caller holds. target "": the list.
func (s *Server) convoModel(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target string, outcome, reason *string) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	typed := false
	step := func(keys []string, text string, read int) (string, error) {
		var press []string
		for _, k := range keys {
			if sess.Persistence == "herdr" {
				press = append(press, modelKeys[k][0])
			} else {
				press = append(press, modelKeys[k][1])
			}
		}
		length := len(text)
		if press == nil && text == "" {
			return "", errors.New("nothing to press")
		}
		res, err := sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, length, press, read), 64<<10,
			sshx.ExecOptions{Stdin: strings.NewReader(text)})
		if err == nil {
			typed = true
		}
		return string(res.Out), err
	}
	opts, err := modelFlow(step, target)
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
		*outcome = "typed"
		if target == "" {
			writeJSON(w, http.StatusOK, map[string]any{"models": opts})
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": target})
		}
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
	case errors.Is(err, sshx.ErrBusy) || errors.As(err, &exitErr) && exitErr.Status == 7:
		*reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case errors.Is(err, errNoPicker), errors.Is(err, errNoModel), errors.Is(err, errNotThere), errors.Is(err, errNotTaken):
		if typed {
			*outcome = "unknown"
		}
		*reason = "picker"
		writeError(w, http.StatusConflict, "picker", err.Error())
	default:
		if typed {
			*outcome = "unknown"
		}
		writeError(w, http.StatusBadGateway, "unknown", "it is not known what the picker took")
	}
}
