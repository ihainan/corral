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
	pickerMore = regexp.MustCompile(`^(…|\.{3})\s*\+\d+\s+models?\b`)
	// (A numbered choice of a dialog's, by which one is told from the words
	// a narrow pane pushed under the one before it.)
	choiceRow = regexp.MustCompile(`^(❯|›)?\s*\d{1,2}\.\s`)
)

// parseModelPicker reads the picker off the screen. Its first entry
// ("Default (recommended)": the default of new sessions, whichever that is)
// is left out; so are those it does not show (scrolled away). A pane too
// narrow for the picker breaks its lines: its title and its keys' line are
// found all the same (see convo_screen.go). Description line breaks are
// preserved: a text capture cannot distinguish word wrap from hard splits.
func parseModelPicker(screen string) modelPicker {
	var p modelPicker
	// Only the active block, including a picker with either end clipped.
	lines := strings.Split(screen, "\n")
	from, to, ok := pickerBlock(lines)
	if !ok {
		return modelPicker{}
	}
	type row struct {
		opt   modelOpt
		col   int // (the column its description begins at; -1: it has none)
		parts []string
	}
	var rows []row
	cursors, currents := 0, 0
	footer := false
	for _, line := range lines[from:to] {
		// The viewport's arrow is decoration, even when it lands on a row.
		line = strings.TrimRight(line, " \r")
		if strings.HasSuffix(line, " ↓") {
			line = strings.TrimRight(strings.TrimSuffix(line, "↓"), " ")
		}
		if pickerMore.MatchString(strings.TrimSpace(line)) || screenSays(line, "←/→ to adjust") {
			footer = true
		}
		if footer {
			if pickerRow.MatchString(line) {
				return modelPicker{}
			}
			continue
		}
		if m := pickerRow.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[2])
			if n < 1 || len(rows) > 0 && n != rows[len(rows)-1].opt.N+1 {
				return modelPicker{}
			}
			f := pickerGaps.Split(m[3], 2)
			label := strings.TrimSpace(f[0])
			o := modelOpt{N: n, at: m[1] == "❯"}
			if l, ok := strings.CutSuffix(label, "✔"); ok {
				label, o.Current = strings.TrimSpace(l), true
			}
			if o.at {
				cursors++
			}
			if o.Current {
				currents++
			}
			o.Label = label
			r := row{opt: o, col: -1}
			if len(f) > 1 {
				if d := strings.TrimSpace(f[1]); d != "" {
					if at := strings.LastIndex(line, d); at >= 0 {
						r.col = len([]rune(line[:at]))
						r.parts = []string{d}
					}
				}
			}
			rows = append(rows, r)
			continue
		}
		// (What a narrow pane pushed off the end of a description: the lines
		// under it, indented to where it begins — nothing else stands there.)
		if n := len(rows); n > 0 && rows[n-1].col > 0 {
			if lead, text := indentOf(line); text != "" && lead == rows[n-1].col {
				rows[n-1].parts = append(rows[n-1].parts, text)
				continue
			}
			// A blank line, summary or effort footer ends the description.
			rows[n-1].col = -1
		}
	}
	// More marks than there should be is odd, not a reason to show the
	// person nothing: the list is still the list. A second tick says only
	// which is in use, so both are dropped; a second cursor is where the
	// switch would move from, so the place is called unknown instead —
	// listing goes on, switching refuses rather than move somewhere wrong.
	for i := range rows {
		if currents > 1 {
			rows[i].opt.Current = false
		}
		if cursors > 1 {
			rows[i].opt.at = false
		}
	}
	for _, r := range rows {
		o := r.opt
		// A plain-text read has lost whether the pane broke a line within
		// a word or after one, and no reading of it can tell: both come
		// back the same. The words are put together with a space, because
		// the menu they end up in is not that pane and wraps them again
		// itself; the cost is that a word the pane cut in half keeps the
		// space, which is rare and reads as what it is.
		o.Desc = strings.Join(r.parts, " ")
		if strings.HasPrefix(o.Label, "Default") || o.Label == "More models" ||
			strings.ContainsAny(o.Label, "…") || o.Label == "" || len(o.Label) > 64 {
			p.opts = append(p.opts, modelOpt{N: o.N, at: o.at})
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
	"down":  {"down", "Down"},
	"up":    {"up", "Up"},
	"left":  {"left", "Left"}, // (the effort slider: see convo_effort.go)
	"right": {"right", "Right"},
	"this":  {"s", "s"},
	"esc":   {"esc", "Escape"},
	"yes":   {"enter", "Enter"},
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
	// (Its box not the conversation's: a subagent's conversation shown in
	// front, or a list holding the keys — its placeholder is no draft.)
	errViewSubagent = errors.New("the terminal shows a subagent's conversation")
	errViewOther    = errors.New("the terminal's keys go to the list of agents or of sessions")
	errScreen       = errors.New("its screen is not as expected")
	errUnsure       = errors.New("it is not known whether the model was taken")
	pickerRule      = regexp.MustCompile(`^\s*[─━]{8,}`)
)

const (
	pickerLines   = 40 // (of the screen read after a step: the picker and what is under it)
	maxPickerMove = 20
	pickerLooks   = 4 // (times the screen is looked at again for a picker not drawn yet)
)

// notePickerUnread says what the screen held where no picker could be read
// off it, so that the next report of this needs no further asking: how much
// was read, which of the picker's marks were there, how many of its rows,
// and the foot of the screen, where the dialog would have been. The words
// of the thread above are not written down.
func notePickerUnread(screen string) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	title, rows := false, 0
	for _, l := range lines {
		if strings.TrimSpace(l) == pickerTitle {
			title = true
		}
		if pickerRow.MatchString(l) {
			rows++
		}
	}
	tail := lines
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	wide := 0
	for _, l := range lines {
		if n := len([]rune(l)); n > wide {
			wide = n
		}
	}
	slog.Warn("model picker not read off the screen",
		"lines", len(lines), "columns", wide, "title", title,
		"keys", wrapAt(lines, pickerKeys, 0) >= 0, "rows", rows,
		"foot", strings.Join(tail, " ⏎ "))
}

// pickerTitle is the picker's first line, pickerKeys a part of its last
// ("Enter to set as default · s to use this session only · Esc to cancel")
// that stands in nothing else. A pane too narrow for that line breaks it
// over two — "… s to use this" and "session only · …" — so the keys are
// looked for with the line ends taken out (see convo_screen.go).
//
// A short pane may lose its title or its keys; a clipped foot instead
// carries a trailing ↓. A title alone is not evidence of an active picker.
const (
	pickerTitle = "Select model"
	pickerKeys  = "s to use this session only"
)

// pickerOwns: from line at down, the screen holds no input box of Claude
// Code's — the picker has the screen, as an open one does, taking the
// box's place. The mark alone is not the box: the picker's own cursor row
// begins with it too. A rule above it identifies even a numbered draft;
// an unnumbered prompt is rejected with or without that rule in view.
func pickerOwns(lines []string, at, end int) bool {
	cursors := 0
	for i := max(at, 0); i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "❯") {
			cursors++
			if i >= end || cursors > 1 || i > 0 && boxTop(lines[i-1]) || !pickerRow.MatchString(lines[i]) {
				return false
			}
		}
	}
	return true
}

// pickerBlock: the lines the picker's rows stand on, [from, to).
//
// A pane too short for the whole picker loses an end of it, and which end
// is not ours to choose: of a short list Claude Code scrolls the title off
// the top, of a long one it cuts the foot — the keys' line with it — and
// marks what is left with an arrow. So whichever end is on the screen is
// the one it is read from, and what keeps a thread quoting the picker from
// being taken for one is that the input box is nowhere under it.
func pickerBlock(lines []string) (from, to int, ok bool) {
	title := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == pickerTitle {
			title = i
		}
	}
	keys := -1
	for i := 0; ; {
		t := wrapAt(lines, pickerKeys, i)
		if t < 0 {
			break
		}
		keys, i = t, t+1
	}
	switch {
	case title >= 0 && keys > title:
		from, to = title+1, keys
	case title >= 0 && strings.HasSuffix(strings.TrimSpace(strings.Join(lines, "\n")), "↓"):
		from, to = title+1, len(lines)
	case title < 0 && keys >= 0: // (its head scrolled away)
		from, to = 0, keys
	default:
		return 0, 0, false
	}
	if !pickerOwns(lines, from, to) {
		return 0, 0, false
	}
	return from, to, true
}

// Recognition and parsing must agree, including invalid row sequences.
func pickerShows(screen string) bool {
	return parseModelPicker(screen).ok
}

// askShows: Claude Code's question after "s" is on the screen, for target
// ("" any): a line "Switch model?" of its own, then its first choice, where
// its cursor is, "Yes, switch to <target>". (Words of the thread quoting it
// are behind a mark: "● Switch model?".)
func askShows(screen, target string) bool { return askWithin(screen, target, 6) }

// askWithin is askShows with its choice looked for that many lines under
// its title: a narrow pane breaks the words between them into more (at 39
// columns the choice is nine lines under, measured 2026-11-10).
func askWithin(screen, target string, span int) bool {
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "Switch model?" {
			continue
		}
		if !pickerOwns(lines, i+1, len(lines)) {
			continue
		}
		for j := i + 1; j < min(len(lines), i+span); j++ {
			t := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(t, "❯") {
				continue
			}
			// (The choice with what a narrow pane pushed under it: those
			// lines are indented past its mark, and nothing else is. Read
			// whole, it is held to the name it names, as ever.)
			lead, _ := indentOf(lines[j])
			c := strings.TrimSpace(strings.TrimPrefix(t, "❯"))
			for k := j + 1; k < min(len(lines), j+wrapSpan); k++ {
				n, text := indentOf(lines[k])
				// (A choice of its own stands as deep as the words pushed
				// under this one, so it is told by its number, not by that.)
				if text == "" || n <= lead || choiceRow.MatchString(text) {
					break
				}
				c += " " + text
			}
			if f := flatScreen(c); f == flatScreen("1. Yes, switch to "+target) ||
				target == "" && strings.HasPrefix(f, flatScreen("1. Yes, switch to ")) {
				return true
			}
		}
	}
	return false
}

// idleInput: the screen shows Claude Code at rest with its input empty: the
// input's lines (from "❯" between two rules, a few at most) hold nothing but
// its placeholder, and nothing says it works ("esc to interrupt"). Words in
// it are a draft, unless greyed (given: a read of the screen with its
// colours, made only then, right after this one) finds the box holding only
// the suggestion Claude Code greys in there, the same words: the plain
// screen cannot tell a suggestion from words typed. (A suggestion is no
// draft: typing a command over it replaces it. Decided on the read with
// colours itself, so that words typed, or the suggestion taken with Tab,
// since the plain one are not let through — review of 9eedda3.)
func idleInput(screen string, greyed ...func() string) error {
	if strings.Contains(screen, "esc to interrupt") {
		return errBusyAgent
	}
	switch claudeOwner(screen, false) {
	case "subagent":
		return errViewSubagent
	case "list", "sessions":
		return errViewOther
	}
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 2; i >= 1; i-- {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "❯") || !boxTop(lines[i-1]) {
			continue
		}
		// (Its lines down to the rule under it: a suggestion wraps in a narrow
		// pane. That rule is the one over it again, the same to the letter —
		// a line of dashes in a draft is indented and of its own length, and
		// taken for the box's foot it would have the draft read as empty:
		// review of 4b4dddd.)
		top := strings.TrimRight(lines[i-1], " \r")
		end := -1
		for j := i + 1; j < len(lines) && j <= i+inputLinesMost; j++ {
			if boxFoot(top, strings.TrimRight(lines[j], " \r")) {
				end = j
				break
			}
		}
		if end < 0 {
			continue
		}
		words := []string{strings.TrimSpace(strings.TrimPrefix(t, "❯"))}
		for j := i + 1; j < end; j++ {
			words = append(words, strings.TrimSpace(lines[j]))
		}
		in := oneSpaced(strings.Join(words, " "))
		// (Empty: its one line, the mark alone. Lines of nothing over more
		// than one are words typed — new lines — not an empty box.)
		if in == "" && end == i+1 {
			return nil
		}
		// (Its placeholder, "Try \"…\"": of one line — Claude Code cuts it
		// short with "…" in a narrow pane, it does not wrap it — and greyed,
		// told so from words typed beginning so by the read with colours,
		// where there is one (review batch4).)
		if strings.HasPrefix(in, `Try "`) && end == i+1 && len(greyed) == 0 {
			return nil
		}
		if in == "" {
			return errDraft
		}
		for _, g := range greyed {
			if g != nil && in == oneSpaced(g()) {
				return nil
			}
		}
		return errDraft
	}
	return errScreen
}

// boxFoot: line is the rule under the input whose top rule is top. Of a
// plain top, the same to the letter; of one with the session's name in it
// (a renamed session: "──── name ─"), a rule alone from the first column.
// (A line of dashes in a draft is indented past the mark.)
func boxFoot(top, line string) bool {
	if line == top {
		return true
	}
	if strings.Trim(strings.TrimSpace(top), "─━") == "" {
		return false // (a plain top: its foot is the same)
	}
	return len([]rune(line)) >= 8 && strings.Trim(line, "─━") == ""
}

// boxTop: line is the rule over Claude Code's input: a rule, or a renamed
// session's with its name on it, ending " ─" — in a narrow pane with no
// rule before the name at all (" a long name… ─": seen so at 30 columns).
func boxTop(line string) bool {
	if pickerRule.MatchString(line) {
		return true
	}
	t := strings.TrimRight(line, " \r")
	return (strings.HasSuffix(t, " ─") || strings.HasSuffix(t, " ━")) && strings.Trim(strings.TrimSpace(t), "─━ ") != ""
}

// (How many lines the input's words may take, wrapped: a suggestion is short.)
const inputLinesMost = 10

func oneSpaced(s string) string { return strings.Join(strings.Fields(s), " ") }

// taken: how many times the screen says target was taken for the session
// ("Set model to Haiku 5.5 for this session only"). That line is longer
// than a narrow pane, which breaks it: counted with the line ends taken
// out. Only a command result (⎿) can count, not ordinary quoted prose.
func taken(screen, target string) int {
	want := flatScreen("Set model to " + target + " for this session only")
	lines := strings.Split(screen, "\n")
	count := 0
	for i, line := range lines {
		text, result := strings.CutPrefix(strings.TrimSpace(line), "⎿")
		if !result {
			continue
		}
		acc := flatScreen(text)
		for j := i; j < min(len(lines), i+wrapSpan); j++ {
			if j > i {
				acc += flatScreen(lines[j])
			}
			if strings.HasPrefix(acc, want) {
				count++ // It may also say "with xhigh effort" after the model.
				break
			}
			if !strings.HasPrefix(want, acc) {
				break
			}
		}
	}
	return count
}

// modelFlow opens the picker; with target "" it lists its models and closes
// it, else it switches to target for this session. It acts only on Claude
// Code at rest, its input empty, no picker or question of it open (one
// left open is closed with Escape first). On a failure after the picker
// opened, Escape is pressed (best effort, by leave).
func modelFlow(step, leave modelStep, target string, greyed ...func() string) (opts []modelOpt, err error) {
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
	if err := idleInput(screen, greyed...); err != nil {
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
	if gateRefused(err) {
		return nil, err // (it came to wait: no picker was opened, nor anything to close; an Escape would answer it)
	}
	if err != nil {
		return out(err)
	}
	screen = opened
	pk := parseModelPicker(screen)
	// (Claude Code may not have finished drawing the picker when the screen
	// was read — a heavy session takes its time — so it is looked at again
	// before it is given up on. One glance was all a picker on its way had.)
	for i := 0; i < pickerLooks && !pk.ok; i++ {
		if screen, err = step([]string{}, "", pickerLines); err != nil {
			return out(err)
		}
		pk = parseModelPicker(screen)
	}
	if !pk.ok {
		notePickerUnread(screen)
		return out(errNoPicker)
	}
	if target == "" {
		if screen, err = step([]string{"esc"}, "", pickerLines); err != nil || pickerShows(screen) || askShows(screen, "") {
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
	if !found {
		return out(errNoModel)
	}
	// Bring a clipped cursor into view before computing a move. A window
	// starting at row 1 lost its bottom; one starting later may have lost
	// the cursor above it. Never guess its position from the current model.
	seek := "up"
	if len(pk.opts) > 0 && pk.opts[0].N > 1 {
		seek = "down"
	}
	for i := 0; !known && i < maxPickerMove; i++ {
		if screen, err = step([]string{seek}, "", pickerLines); err != nil {
			return out(err)
		}
		pk = parseModelPicker(screen)
		if !pk.ok {
			return out(errNoPicker)
		}
		at, known = pk.cursor()
	}
	if !known {
		return out(errNotThere)
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
	} else if at.Label != target {
		return out(errNotThere)
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
	case pickerShows(screen) || askShows(screen, ""):
		return out(errNotTaken)
	case taken(screen, target) > before:
		return []modelOpt{to}, nil
	}
	return nil, errUnsure // (the picker gone, nothing said of it: as it may have been taken, no Escape)
}

// convoModel runs modelFlow in the agent's pane, each step with the checks
// of convoSendCommand (the same process in front there). It holds the
// pane's turn (release gives it back): until the last step's command has
// ended on the host, if one was given up before. target "": the list.
// A Codex's (codex): codexModelFlow, target with effort; its config.toml,
// which "s" leaves alone, read before and after only to say if it changed.
func (s *Server) convoModel(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target string, outcome, reason *string, release func(), codex bool, effort string, log string, via, auth string, own bool) {
	var procKey, kinKey string // (Claude Code's: what is kept of its picker's list, see lineListFor)
	if !codex {
		// (Claude Code's settings are one account's: a switch at a time, what
		// an earlier one left settled first — convo_line_settle.go.)
		account, aerr := settingsAccount(ctx, sess)
		if aerr != nil {
			release()
			*reason = "settings"
			writeError(w, http.StatusBadGateway, "settings", "Claude Code's settings could not be read: nothing done")
			return
		}
		procKey, kinKey = lineListKey(account, pid, start), lineKinKey(account, via, auth)
		if target == "" && !own {
			// (Its list as its picker last gave it, where that is kept: at
			// once, nothing typed, its picker not opened. Which of them it
			// is on is not said with it — a /model typed in its terminal
			// since would not be known here: the page marks that by the
			// conversation's own log, which says each switch as it is made.)
			now := time.Now()
			opts, mine := offered(procKey, now)
			kin := false
			if !mine {
				opts, kin = s.kinOffered(ctx, kinKey, now)
			}
			for i := range opts {
				opts[i].Current = false
			}
			if mine || kin {
				release()
				*outcome, *reason = "kept", "list_kept"
				writeJSON(w, http.StatusOK, map[string]any{"models": opts, "kept": true, "kin": !mine})
				// (What an earlier switch left to settle is looked at all the
				// same — after the answer, not before it: the list waits for
				// nothing. A switch asked for meanwhile waits its moment for it,
				// as for any other.)
				// (Whether there is any, only read here: what is kept of it is
				// changed with the account's settings in hand, in settingsTurn.)
				_, inMem := pendingMem.Load(account)
				if p, perr := s.store.SettingsPendingOf(ctx, account); inMem || perr != nil || p != nil {
					go func() {
						bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
						defer cancel()
						if done, _, _, ok := s.settingsTurn(bctx, sess, account); ok {
							done()
						}
					}()
				}
				return
			}
		}
		if _, line := lineModels[target]; line && log != "" && !own {
			if _, _, ok := lineModelArg(procKey, target, time.Now()); !ok {
				// (To be typed on one line, and this process's own picker not
				// yet seen to offer it — the list the page chose from may be
				// its kin's: its own is read first, as for a list asked for,
				// with the pane's turn kept here meanwhile.)
				o, why := "refused", "" // (as the request's own begin: a refusal sets only why)
				read := make(chan struct{})
				quiet := &quietWriter{}
				s.convoModel(quiet, ctx, sess, pane, socket, pid, start, sid, "", &o, &why, func() { close(read) }, false, "", log, via, auth, true)
				<-read
				if quiet.status >= 400 {
					release()
					*outcome, *reason = o, why
					quiet.to(w)
					return
				}
				// (Its picker was opened for this request: said of it, however
				// what follows ends — a refusal after it sets only why.)
				*outcome = o
			}
		}
		done, code, why, ok := s.settingsTurn(ctx, sess, account)
		if !ok {
			release()
			*reason = code
			writeError(w, http.StatusConflict, code, why)
			return
		}
		if target != "" && log != "" {
			// (Only by what this process's own picker was seen to offer.)
			if arg, want, ok := lineModelArg(procKey, target, time.Now()); ok {
				s.convoModelLine(w, ctx, sess, pane, socket, pid, start, sid, target, arg, want, log, account, outcome, reason, release, done, procKey, kinKey)
				return
			}
		}
		turn := release
		release = func() { done(); turn() }
	}
	var running sync.WaitGroup // (steps whose command may still run on the host)
	// The default model as it is now, put back after if changed (see withModel).
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	saved, none, err := readSettings(sctx, sess)
	if codex {
		saved, none, err = readCodexConfig(sctx, sess)
	}
	scancel()
	was, had, perr := settingsModel(saved, none)
	if codex {
		perr = nil
	}
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
		if codex {
			// (Codex's config: said if changed, not written back — Codex writes it itself too.)
			now, gone, err := readCodexConfig(rctx, sess)
			if err != nil {
				return true, err
			}
			if gone != none || !bytes.Equal(now, saved) {
				return true, errors.New("Codex's config.toml changed")
			}
			return false, nil
		}
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
					defaultChanged.Store(sess.ID, time.Now()) // (said in the conversation's next answers)
				}
				running.Wait()
			}
			release()
		}()
	}()
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	typed := *outcome == "typed" // (its picker opened already by this request, for its own list: see above)
	run := func(ctx context.Context, keys []string, text string, read int, at string) (string, error) {
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
			if codex {
				gate = "cfree"
			}
		}
		if at != "" {
			gate = "cscreen:" + at // (Codex's: pressed only on the screen it was decided on)
		}
		running.Add(1)
		res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,
			sshx.ExecOptions{Stdin: strings.NewReader(text), Release: running.Done})
		if err == nil && (len(press) > 0 || text != "") {
			typed = true
		}
		return string(res.Out), err
	}
	step := func(keys []string, text string, read int) (string, error) { return run(ctx, keys, text, read, "") }
	// (Leaving the picker after a failure: once a step given up has ended,
	// on its own short while, the request's may be over.)
	leave := func(keys []string, text string, read int) (string, error) {
		running.Wait()
		lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer lcancel()
		return run(lctx, keys, text, read, "")
	}
	// (Codex's: the screen whole and its digest; keys pressed only on it.)
	look := func() (string, string, error) {
		running.Add(1)
		res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: running.Done})
		if err != nil {
			return "", "", err
		}
		shown, digest, _ := screenOf(res.Out)
		return shown, digest, nil
	}
	// (Leaving it after a failure, as leave: in a while of its own.)
	undo := codexIO{
		look: func() (string, string, error) {
			running.Wait()
			lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
			defer lcancel()
			running.Add(1)
			res, err := sshx.Exec(lctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: running.Done})
			if err != nil {
				return "", "", err
			}
			shown, digest, _ := screenOf(res.Out)
			return shown, digest, nil
		},
		press: func(keys []string, text, digest string) error {
			running.Wait()
			lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
			defer lcancel()
			_, err := run(lctx, keys, text, pickerLines, digest)
			return err
		},
	}
	press := func(keys []string, text, digest string) error {
		_, err := run(ctx, keys, text, pickerLines, digest) // (its read: a moment after, as Codex draws what the keys did)
		return err
	}
	var opts []modelOpt
	if codex {
		opts, err = codexModelFlow(codexIO{look, press}, undo, target, effort)
	} else {
		opts, err = modelFlow(step, leave, target, func() string { return s.greyedSuggestion(ctx, sess, pane, socket, &running) })
		// (What is kept of its list, changed while its pane and the account's
		// settings are still in hand.)
		switch {
		case err == nil && target != "" && len(opts) == 1:
			confirmOffered(procKey, kinKey, opts[0], time.Now())
		case errors.Is(err, errNoModel), errors.Is(err, errUnsure), errors.Is(err, errElsewhere):
			// (Not as the lists kept said, or not known how it ended: read again.)
			s.forgetLists(context.WithoutCancel(ctx), procKey, kinKey)
		}
		if err == nil && target == "" {
			// (With the lists' lock, as kinOffered's read of the store and
			// its keeping of that are: an older list read from the store
			// meanwhile is not kept over this one.)
			lineListMu.Lock()
			noteOffered(procKey, opts, time.Now())
			noteOffered(kinKey, opts, time.Now())
			if rows, jerr := json.Marshal(opts); jerr == nil {
				if perr := s.store.PutModelList(context.WithoutCancel(ctx), kinKey, time.Now().Unix(), rows); perr != nil {
					slog.Warn("model list: not kept in the store", "session", sess.ID, "err", perr)
				}
			}
			lineListMu.Unlock()
		}
	}
	restored, restoreFailed := false, false
	if ctx.Err() == nil {
		running.Wait()
		changed, rerr := restore()
		running.Wait()
		checked = true
		restored, restoreFailed = changed && rerr == nil, changed && rerr != nil
		if restoreFailed {
			slog.Error("model switch: the default model changed and could not be put back", "session", sess.ID, "err", rerr)
			defaultChanged.Store(sess.ID, time.Now())
		}
	}
	switch {
	case restoreFailed:
		*reason = "default_not_restored"
	case restored:
		*reason = "default_restored"
	}
	switch {
	case restoreFailed:
		// (Above all else: the person is to know, and put it back.)
		*outcome = "unknown"
		if codex {
			writeError(w, http.StatusInternalServerError, "default_changed", "Codex's config.toml (the default of new sessions) changed meanwhile")
			break
		}
		writeError(w, http.StatusInternalServerError, "default_changed", "the default model of new sessions changed and could not be put back")
	case err == nil:
		*outcome = "typed"
		if target == "" {
			writeJSON(w, http.StatusOK, map[string]any{"models": opts, "restored": restored, "restore_failed": restoreFailed})
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": target, "restored": restored, "restore_failed": restoreFailed})
		}
	default:
		writeSwitchError(w, err, typed, outcome, reason)
	}
}

// quietWriter keeps an answer instead of giving it: a step of a request
// that is answered by what comes after it, unless the step failed.
type quietWriter struct {
	head   http.Header
	status int
	body   bytes.Buffer
}

func (q *quietWriter) Header() http.Header {
	if q.head == nil {
		q.head = http.Header{}
	}
	return q.head
}

func (q *quietWriter) WriteHeader(status int) {
	if q.status == 0 {
		q.status = status
	}
}

func (q *quietWriter) Write(b []byte) (int, error) {
	if q.status == 0 {
		q.status = http.StatusOK
	}
	return q.body.Write(b)
}

// to gives w the answer kept.
func (q *quietWriter) to(w http.ResponseWriter) {
	for k, v := range q.head {
		w.Header()[k] = v
	}
	w.WriteHeader(q.status)
	w.Write(q.body.Bytes())
}

// writeSwitchError answers a switch that failed with err (typed: something
// was typed into the pane before it did).
func writeSwitchError(w http.ResponseWriter, err error, typed bool, outcome, reason *string) {
	var exitErr *sshx.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
	case errors.As(err, &exitErr) && (exitErr.Status == 9 || exitErr.Status == 5):
		// (Its picker's command not sent, or typed and not sent: it waits for an answer.)
		*outcome, *reason = said(typed || exitErr.Status == 5), "agent_waiting"
		writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
	case errors.As(err, &exitErr) && exitErr.Status >= exitOwnerSubagent && exitErr.Status <= exitOwnerLast:
		// (Claude Code's input box not the conversation's: its picker's command not sent.)
		code, typedNow, _ := writeOwnerRefusal(w, err)
		*outcome, *reason = said(typed || typedNow), code
	case errors.As(err, &exitErr) && exitErr.Status == 10:
		// (A press not made: the screen changed after it was read — a key
		// typed in the terminal, say. Its picker, if open, left.)
		*outcome, *reason = said(typed), "screen_changed"
		writeError(w, http.StatusConflict, "screen_changed", "its screen changed meanwhile: nothing more was pressed")
	case errors.As(err, &exitErr) && exitErr.Status == 11:
		*outcome, *reason = said(typed), "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "its screen cannot be checked on the host: switch in the terminal")
	case errors.Is(err, sshx.ErrBusy) || errors.As(err, &exitErr) && exitErr.Status == 7:
		*reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case errors.Is(err, errBusyAgent):
		*reason = "working"
		writeError(w, http.StatusConflict, "working", err.Error())
	case errors.Is(err, errDraft):
		*reason = "draft"
		writeError(w, http.StatusConflict, "draft", err.Error())
	case errors.Is(err, errViewSubagent):
		*reason = "subagent_view"
		writeError(w, http.StatusConflict, "subagent_view", err.Error())
	case errors.Is(err, errViewOther):
		*reason = "other_view"
		writeError(w, http.StatusConflict, "other_view", err.Error())
	case errors.Is(err, errNoPicker), errors.Is(err, errNoModel), errors.Is(err, errNotThere), errors.Is(err, errNotTaken), errors.Is(err, errScreen):
		if typed {
			*outcome = "unknown"
		}
		// (The record says which of them it was. One word for all five left
		// the log saying no more than the person on the page could see:
		// a picker read and a picker left open are not the same trouble.)
		*reason = pickerReason(err)
		writeError(w, http.StatusConflict, "picker", err.Error())
	default:
		if typed {
			*outcome = "unknown"
		}
		writeError(w, http.StatusBadGateway, "unknown", "it is not known what the picker took")
	}
}

// pickerReason names, for the record, which of the picker's troubles it
// was. The page is told "picker" for all of them all the same: there is
// nothing the person could do differently.
func pickerReason(err error) string {
	switch {
	case errors.Is(err, errNoPicker):
		return "picker_unread"
	case errors.Is(err, errNoModel):
		return "picker_no_model"
	case errors.Is(err, errNotThere):
		return "picker_not_there"
	case errors.Is(err, errNotTaken):
		return "picker_not_taken"
	case errors.Is(err, errScreen):
		return "picker_screen"
	}
	return "picker"
}

// defaultChanged: the sessions (by id) where a switch left the default model
// changed, not put back, and when: said in their conversation's answers
// for a while, also where the request that did it was given up.
var defaultChanged sync.Map

func defaultChangedOf(id string) bool {
	v, ok := defaultChanged.Load(id)
	if !ok {
		return false
	}
	if time.Since(v.(time.Time)) > 10*time.Minute {
		defaultChanged.Delete(id)
		return false
	}
	return true
}

// The default model of new sessions is the top-level "model" of Claude
// Code's settings. Whatever a switch did (a key taken by the picker it was
// not meant for, the person's own keys in between), it is put back as it
// was before: read before the flow, again after, and if it changed, that
// one value restored, the rest of the file as it is then.

// settingsReadCommand prints Claude Code's settings (through a link, the
// file it names); exits 3 when there are none (its folder readable, the
// file not there), 5 when they could not be read (or it cannot be told).
const settingsReadCommand = `f="$HOME/.claude/settings.json"
if [ -L "$f" ]; then f=$(readlink -f "$f") && [ -n "$f" ] || exit 5; fi
if [ ! -e "$f" ]; then d=${f%/*}; { [ ! -e "$d" ] || { [ -r "$d" ] && [ -x "$d" ]; }; } && exit 3; exit 5; fi
head -c 1048577 "$f" || exit 5`

// settingsWriteCommand replaces them with its input, in one step (its mode
// kept), only if they are still those read ($1: their SHA-256; else exit
// 9) and the input came whole ($2 its length, $3 its SHA-256; else 4).
// Fixed; its arguments are hex digests and a number.
func settingsWriteCommand(was []byte, now []byte) string {
	script := `f="$HOME/.claude/settings.json"
if [ -L "$f" ]; then f=$(readlink -f "$f") && [ -n "$f" ] || exit 4; fi
[ -f "$f" ] || exit 4
sum() { sha256sum <"$1" | cut -d' ' -f1; }
[ "$(sum "$f")" = "$1" ] || exit 9
t=$(mktemp "$f.corral.XXXXXX") || exit 4
cp -p "$f" "$t" || { rm -f "$t"; exit 4; }
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
	var exitErr *sshx.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		return nil, true, nil
	case err != nil:
		return nil, false, err
	case len(out) > maxSettings:
		return nil, false, errors.New("settings too long")
	}
	return out, false, nil
}

// settingsModel: the settings' top-level "model" (had false: none), or an
// error when they are not one JSON object with at most one.
func settingsModel(doc []byte, none bool) (json.RawMessage, bool, error) {
	if none {
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

// readCodexConfig reads ~/.codex/config.toml (none: there is none), as
// readSettings reads Claude Code's settings.
func readCodexConfig(ctx context.Context, sess *term.Session) (doc []byte, none bool, err error) {
	out, err := sshx.Output(ctx, sess.Client(), codexConfigReadCommand, maxSettings+16)
	var exitErr *sshx.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		return nil, true, nil
	case err != nil:
		return nil, false, err
	case len(out) > maxSettings:
		return nil, false, errors.New("config too long")
	}
	return out, false, nil
}

// codexConfigReadCommand: fixed; exit 3 where there is no config.
const codexConfigReadCommand = `f="${CODEX_HOME:-$HOME/.codex}/config.toml"; [ -f "$f" ] || exit 3; head -c 262160 "$f"`
