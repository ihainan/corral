package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	var p modelPicker
	// (Its rows only: between its title and its keys' line, the last such.)
	lines := strings.Split(screen, "\n")
	from, to := -1, -1
	for i, l := range lines {
		switch {
		case strings.TrimSpace(l) == "Select model":
			from, to = i, -1
		case from >= 0 && to < 0 && strings.Contains(l, pickerKeys):
			to = i
		}
	}
	if from < 0 || to < 0 {
		return modelPicker{}
	}
	for _, line := range lines[from+1 : to] {
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
	"down": {"down", "Down"},
	"up":   {"up", "Up"},
	"this": {"s", "s"},
	"esc":  {"esc", "Escape"},
	"yes":  {"enter", "Enter"},
}

// modelStep runs one step on the host: keys (names of modelKeys; none:
// only the screen read) or a text typed and Enter, then the last read lines
// of the screen. ctx is the step's own (a clean-up after a request was
// given up has one of its own).
type modelStep func(keys []string, text string, read int) (string, error)

var (
	errNoPicker  = errors.New("the model picker did not show")
	errNoModel   = errors.New("no such model in the picker")
	errNotThere  = errors.New("the picker's choice is not where it was moved to")
	errNotTaken  = errors.New("the picker did not take the choice")
	errBusyAgent = errors.New("the agent is working")
	errDraft     = errors.New("its input holds text not sent")
	errScreen    = errors.New("its screen is not as expected")
	errUnsure    = errors.New("it is not known whether the model was taken")
	pickerRule   = regexp.MustCompile(`^\s*[─━]{8,}`)
)

const (
	pickerLines   = 40 // (of the screen read after a step: the picker and what is under it)
	maxPickerMove = 20
)

// pickerKeys is in the picker's line of keys ("Enter to set as default ·
// s to use this session only · Esc to cancel"), as in nothing else.
const pickerKeys = "s to use this session only"

// pickerShows: the picker is on the screen (its title, a line of its own,
// then its line of keys).
func pickerShows(screen string) bool {
	title := false
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) == "Select model" {
			title = true
		}
		if title && strings.Contains(l, pickerKeys) {
			return true
		}
	}
	return false
}

// askShows: Claude Code's question after "s" is on the screen, for target
// ("" any): a line "Switch model?" of its own, then its first choice, where
// its cursor is, "Yes, switch to <target>". (Words of the thread quoting it
// are behind a mark: "● Switch model?".)
func askShows(screen, target string) bool {
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "Switch model?" {
			continue
		}
		for _, n := range lines[i+1 : min(len(lines), i+6)] {
			t := strings.TrimSpace(n)
			if !strings.HasPrefix(t, "❯") {
				continue
			}
			if c := strings.TrimSpace(strings.TrimPrefix(t, "❯")); c == "1. Yes, switch to "+target || target == "" && strings.HasPrefix(c, "1. Yes, switch to ") {
				return true
			}
		}
	}
	return false
}

// idleInput: the screen shows Claude Code at rest with its input empty: the
// input's line ("❯" between two rules) holds nothing but its placeholder,
// and nothing says it works ("esc to interrupt").
func idleInput(screen string) error {
	if strings.Contains(screen, "esc to interrupt") {
		return errBusyAgent
	}
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 2; i >= 1; i-- {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "❯") || !pickerRule.MatchString(lines[i-1]) || !pickerRule.MatchString(lines[i+1]) {
			continue
		}
		if in := strings.TrimSpace(strings.TrimPrefix(t, "❯")); in != "" && !strings.HasPrefix(in, `Try "`) {
			return errDraft
		}
		return nil
	}
	return errScreen
}

// taken: how many times the screen says target was taken for the session
// ("Set model to Haiku 5.5 for this session only").
func taken(screen, target string) int {
	return strings.Count(screen, "Set model to "+target+" for this session only")
}

// modelFlow opens the picker; with target "" it lists its models and closes
// it, else it switches to target for this session. It acts only on Claude
// Code at rest, its input empty, no picker or question of it open (one
// left open is closed with Escape first). On a failure after the picker
// opened, Escape is pressed (best effort, by leave).
func modelFlow(step, leave modelStep, target string) (opts []modelOpt, err error) {
	screen, err := step([]string{}, "", pickerLines)
	if err != nil {
		return nil, err
	}
	for i := 0; i < 2 && (pickerShows(screen) || askShows(screen, "")); i++ {
		if screen, err = step([]string{"esc"}, "", pickerLines); err != nil {
			return nil, err
		}
	}
	if pickerShows(screen) || askShows(screen, "") {
		return nil, errScreen
	}
	if err := idleInput(screen); err != nil {
		return nil, err
	}
	before := taken(screen, target)
	opened, err := step(nil, "/model", pickerLines)
	// (Escape, read, again while the picker or its question still shows: twice at most.)
	out := func(e error) ([]modelOpt, error) {
		for i := 0; i < 2; i++ {
			if scr, err := leave([]string{"esc"}, "", pickerLines); err != nil || !pickerShows(scr) && !askShows(scr, "") {
				break
			}
		}
		return nil, e
	}
	if err != nil {
		return out(err)
	}
	screen = opened
	pk := parseModelPicker(screen)
	if !pk.ok {
		return out(errNoPicker)
	}
	if target == "" {
		if screen, err = step([]string{"esc"}, "", pickerLines); err != nil || pickerShows(screen) {
			return out(errScreen)
		}
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
		return out(errNoModel)
	}
	if d := to.N - at.N; d != 0 {
		key := "down"
		if d < 0 {
			key, d = "up", -d
		}
		if d > maxPickerMove {
			return out(errNoModel)
		}
		keys := make([]string, d)
		for i := range keys {
			keys[i] = key
		}
		if screen, err = step(keys, "", pickerLines); err != nil {
			return out(err)
		}
		now := parseModelPicker(screen)
		if c, ok := now.cursor(); !now.ok || !ok || c.Label != target {
			return out(errNotThere)
		}
	}
	if screen, err = step([]string{"this"}, "", pickerLines); err != nil {
		return out(err)
	}
	if !pickerShows(screen) && askShows(screen, target) {
		if screen, err = step([]string{"yes"}, "", pickerLines); err != nil {
			return out(err)
		}
	}
	switch {
	case taken(screen, target) > before:
		return []modelOpt{to}, nil
	case pickerShows(screen) || askShows(screen, ""):
		return out(errNotTaken)
	}
	return nil, errUnsure // (the picker gone, nothing said of it: as it may have been taken, no Escape)
}

// convoModel runs modelFlow in the agent's pane, each step with the checks
// of convoSendCommand (the same process in front there). It holds the
// pane's turn (release gives it back): until the last step's command has
// ended on the host, if one was given up before. target "": the list.
func (s *Server) convoModel(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target string, outcome, reason *string, release func()) {
	var running sync.WaitGroup // (steps whose command may still run on the host)
	// The default model as it is now, put back after if changed (see withModel).
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	saved, none, err := readSettings(sctx, sess)
	scancel()
	was, had, perr := settingsModel(saved, none)
	if err != nil || perr != nil {
		release()
		*reason = "settings"
		writeError(w, http.StatusBadGateway, "settings", "Claude Code's settings could not be read: nothing done")
		return
	}
	checked := false
	// restore: changed says it had changed; err that it could not be put back.
	restore := func() (changed bool, err error) {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer rcancel()
		for try := 0; try < 2; try++ { // (changed again meanwhile: read once more)
			now, none, err := readSettings(rctx, sess)
			if err != nil {
				return true, err // (not known: said so, not taken for unchanged)
			}
			cur, has, err := settingsModel(now, none)
			if err != nil {
				return true, err
			}
			if has == had && (!has || jsonSame(cur, was)) {
				return changed, nil
			}
			changed = true
			if none {
				return true, errors.New("the settings are gone")
			}
			fixed, err := withModel(now, was, had)
			if err != nil {
				return true, err
			}
			running.Add(1)
			_, err = sshx.Exec(rctx, sess.Client(), settingsWriteCommand(now, fixed), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(fixed), Release: running.Done})
			var exitErr *sshx.ExitError
			if errors.As(err, &exitErr) && exitErr.Status == 9 {
				continue
			}
			if err != nil {
				return true, err
			}
			slog.Warn("model switch: the default model had changed; put back", "session", sess.ID)
			return true, nil
		}
		return true, errors.New("the settings kept changing")
	}
	defer func() {
		go func() {
			running.Wait()
			if !checked {
				if changed, err := restore(); changed && err != nil { // (given up: once its steps have ended)
					slog.Error("model switch: the default model changed and could not be put back", "session", sess.ID, "err", err)
				}
				running.Wait()
			}
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
		running.Add(1)
		res, err := sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,
			sshx.ExecOptions{Stdin: strings.NewReader(text), Release: running.Done})
		if err == nil && (len(press) > 0 || text != "") {
			typed = true
		}
		return string(res.Out), err
	}
	step := func(keys []string, text string, read int) (string, error) { return run(ctx, keys, text, read) }
	// (Leaving the picker after a failure: once a step given up has ended,
	// on its own short while, the request's may be over.)
	leave := func(keys []string, text string, read int) (string, error) {
		running.Wait()
		lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer lcancel()
		return run(lctx, keys, text, read)
	}
	opts, err := modelFlow(step, leave, target)
	restored, restoreFailed := false, false
	if ctx.Err() == nil {
		running.Wait()
		changed, rerr := restore()
		running.Wait()
		checked = true
		restored, restoreFailed = changed && rerr == nil, changed && rerr != nil
		if restoreFailed {
			slog.Error("model switch: the default model changed and could not be put back", "session", sess.ID, "err", rerr)
		}
	}
	switch {
	case restoreFailed:
		*reason = "default_not_restored"
	case restored:
		*reason = "default_restored"
	}
	var exitErr *sshx.ExitError
	switch {
	case restoreFailed:
		// (Above all else: the person is to know, and put it back.)
		*outcome = "unknown"
		writeError(w, http.StatusInternalServerError, "default_changed", "the default model of new sessions changed and could not be put back")
	case err == nil:
		*outcome = "typed"
		if target == "" {
			writeJSON(w, http.StatusOK, map[string]any{"models": opts, "restored": restored, "restore_failed": restoreFailed})
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": target, "restored": restored, "restore_failed": restoreFailed})
		}
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
	case errors.Is(err, sshx.ErrBusy) || errors.As(err, &exitErr) && exitErr.Status == 7:
		*reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case errors.Is(err, errBusyAgent):
		*reason = "working"
		writeError(w, http.StatusConflict, "working", err.Error())
	case errors.Is(err, errDraft):
		*reason = "draft"
		writeError(w, http.StatusConflict, "draft", err.Error())
	case errors.Is(err, errNoPicker), errors.Is(err, errNoModel), errors.Is(err, errNotThere), errors.Is(err, errNotTaken), errors.Is(err, errScreen):
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

// The default model of new sessions is the top-level "model" of Claude
// Code's settings. Whatever a switch did (a key taken by the picker it was
// not meant for, the person's own keys in between), it is put back as it
// was before: read before the flow, again after, and if it changed, that
// one value restored, the rest of the file as it is then.

// settingsReadCommand prints Claude Code's settings ("none" when there are
// none; through a link, the file it names); not read, it exits 5.
const settingsReadCommand = `f="$HOME/.claude/settings.json"; [ -L "$f" ] && f=$(readlink -f "$f"); [ -e "$f" ] || { printf none; exit 0; }; head -c 1048577 "$f" || exit 5`

// settingsWriteCommand replaces them with its input, in one step (its mode
// kept), only if they are still those read ($1: their SHA-256; else exit
// 9) and the input came whole ($2 its length, $3 its SHA-256; else 4).
// Fixed; its arguments are hex digests and a number.
func settingsWriteCommand(was []byte, now []byte) string {
	script := `f="$HOME/.claude/settings.json"; [ -L "$f" ] && f=$(readlink -f "$f"); [ -f "$f" ] || exit 4
sum() { sha256sum <"$1" | cut -d' ' -f1; }
[ "$(sum "$f")" = "$1" ] || exit 9
t="$f.corral.$$"
cp -p "$f" "$t" || exit 4
cat >"$t" || { rm -f "$t"; exit 4; }
[ "$(wc -c <"$t" | tr -d ' ')" = "$2" ] && [ "$(sum "$t")" = "$3" ] || { rm -f "$t"; exit 4; }
[ "$(sum "$f")" = "$1" ] || { rm -f "$t"; exit 9; }
mv -f "$t" "$f" || { rm -f "$t"; exit 4; }`
	a, b := sha256.Sum256(was), sha256.Sum256(now)
	return "sh -c " + sshx.QuoteShell(script) + " corral " + hex.EncodeToString(a[:]) + " " + strconv.Itoa(len(now)) + " " + hex.EncodeToString(b[:])
}

const maxSettings = 1 << 20

// readSettings: Claude Code's settings as they are (none: there are none).
func readSettings(ctx context.Context, sess *term.Session) (doc []byte, none bool, err error) {
	out, err := sshx.Output(ctx, sess.Client(), settingsReadCommand, maxSettings+16)
	switch {
	case err != nil:
		return nil, false, err
	case string(out) == "none":
		return nil, true, nil
	case len(out) > maxSettings:
		return nil, false, errors.New("settings too long")
	}
	return out, false, nil
}

// settingsModel: the settings' top-level "model" (had false: none), or an
// error when they are not one JSON object with at most one.
func settingsModel(doc []byte, none bool) (json.RawMessage, bool, error) {
	if none || len(bytes.TrimSpace(doc)) == 0 {
		return nil, false, nil
	}
	v, _, _, had, err := topValue(doc, "model")
	return v, had, err
}

// topValue finds a top-level key of a JSON object: its value as written,
// and the span of the key and its value (from just after what comes before
// it), or found false.
//
// doc must be one whole JSON object, the key in it at most once.
func topValue(doc []byte, key string) (val json.RawMessage, from, to int, found bool, err error) {
	if !json.Valid(doc) {
		return nil, 0, 0, false, errors.New("not JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, 0, 0, false, errors.New("not an object")
	}
	for dec.More() {
		start := int(dec.InputOffset())
		t, err := dec.Token()
		if err != nil {
			return nil, 0, 0, false, err
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, 0, 0, false, err
		}
		if k == key {
			if found {
				return nil, 0, 0, false, errors.New("the key twice")
			}
			val, from, to, found = v, start, int(dec.InputOffset()), true
		}
	}
	return val, from, to, found, nil
}

// withModel is doc with its top-level "model" as before (absent when absent
// before), the rest as in doc; checked to be so.
func withModel(doc []byte, before json.RawMessage, had bool) ([]byte, error) {
	_, from, to, found, err := topValue(doc, "model")
	if err != nil {
		return nil, err
	}
	var out []byte
	switch {
	case found && had:
		span := doc[from:to]
		i := bytes.IndexByte(span, ':')
		if i < 0 {
			return nil, errors.New("no colon")
		}
		out = append(append(append([]byte{}, doc[:from+i+1]...), append([]byte(" "), before...)...), doc[to:]...)
	case found:
		// (Its key and value out, with the comma before it (in its span, from
		// just after the member before), or the one after when it is first.)
		if bytes.HasPrefix(bytes.TrimLeft(doc[from:to], " \t\r\n"), []byte(",")) {
			out = append(append([]byte{}, doc[:from]...), doc[to:]...)
		} else {
			rest := bytes.TrimLeft(doc[to:], " \t\r\n")
			rest = bytes.TrimPrefix(rest, []byte(","))
			out = append(append([]byte{}, doc[:from]...), rest...)
		}
	case had:
		i := bytes.IndexByte(doc, '{')
		member := "\n  \"model\": " + string(before)
		if !bytes.HasPrefix(bytes.TrimLeft(doc[i+1:], " \t\r\n"), []byte("}")) {
			member += ","
		}
		out = append(append(append([]byte{}, doc[:i+1]...), member...), doc[i+1:]...)
	default:
		return doc, nil
	}
	// (Checked: all else as in doc, the model as before.)
	var a, b map[string]json.RawMessage
	if json.Unmarshal(doc, &a) != nil || json.Unmarshal(out, &b) != nil {
		return nil, errors.New("not JSON once changed")
	}
	delete(a, "model")
	m, ok := b["model"]
	delete(b, "model")
	if ok != had || (had && !jsonSame(m, before)) || len(a) != len(b) {
		return nil, errors.New("the model not as before")
	}
	for k, v := range a {
		if !jsonSame(v, b[k]) {
			return nil, errors.New("another setting changed: " + k)
		}
	}
	return out, nil
}

func jsonSame(a, b json.RawMessage) bool {
	var x, y bytes.Buffer
	return json.Compact(&x, a) == nil && json.Compact(&y, b) == nil && bytes.Equal(x.Bytes(), y.Bytes())
}
