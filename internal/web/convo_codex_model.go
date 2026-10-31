package web

import (
	"errors"
	"regexp"
	"strings"

	"github.com/ihainan/corral/internal/sshx"
)

// Switching a Codex's model and reasoning effort from the agents page,
// through its own /model picker, for the session only
// (plans/5d3c-codex-model.md). Codex 0.164's picker has two steps: "Select
// Model and Effort" (a numbered list, Enter goes on to the next step), then
// "Select Reasoning Level for <model>", where Enter saves the choice as the
// default of every new session ("enter default") and "s" takes it for this
// session only — so Enter is never pressed there, only "s". No digit is
// pressed (a digit picks at once). Each step is read before the next;
// anything not as expected is left with Escape.

// codexPicker is one step of Codex's picker as its screen shows it.
type codexPicker struct {
	ok      bool
	effort  bool   // (the second step: "Select Reasoning Level for …")
	of      string // (the second step's model, as its head names it)
	options []modelOpt
}

var (
	codexPickerHead   = regexp.MustCompile(`^\s*Select (?:Model and Effort|Reasoning Level for (.+?))\s*$`)
	codexPickerOption = regexp.MustCompile(`^\s*(›)?\s*(\d{1,2})\.\s+(\S.*?)\s*$`)
	codexPickerFoot   = regexp.MustCompile(`^\s*enter (?:select|default)\b.*\besc back\b`)
)

// parseCodexPicker reads the picker at the end of the screen: its head, its
// numbered choices (each label apart from its description: two spaces or
// more between them), one with the cursor (›), then its footer.
func parseCodexPicker(screen string) codexPicker {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	foot := -1
	for i := len(lines) - 1; i >= 0 && len(lines)-i <= 4; i-- {
		if codexPickerFoot.MatchString(lines[i]) {
			foot = i
			break
		}
	}
	if foot < 0 {
		return codexPicker{}
	}
	head := -1
	var p codexPicker
	for i := foot - 1; i >= 0 && foot-i <= 30; i-- {
		if m := codexPickerHead.FindStringSubmatch(lines[i]); m != nil {
			head, p.effort, p.of = i, strings.Contains(lines[i], "Reasoning Level"), m[1]
			break
		}
	}
	if head < 0 {
		return codexPicker{}
	}
	cursors := 0
	for _, l := range lines[head+1 : foot] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		m := codexPickerOption.FindStringSubmatch(l)
		if m == nil {
			// (A description wrapped in a narrow pane: indented under it, its words go on.)
			if n := len(p.options); n > 0 && strings.HasPrefix(l, "    ") {
				p.options[n-1].Desc = strings.TrimSpace(p.options[n-1].Desc + " " + strings.TrimSpace(l))
				continue
			}
			return codexPicker{} // (another line among its choices: not the picker as known)
		}
		o := codexOptions([]string{m[3]})[0]
		n := 0
		for _, c := range m[2] {
			n = n*10 + int(c-'0')
		}
		if n != len(p.options)+1 {
			return codexPicker{}
		}
		opt := modelOpt{N: n, Label: o.label, Desc: o.desc, at: m[1] != ""}
		if opt.at {
			cursors++
		}
		p.options = append(p.options, opt)
	}
	p.ok = len(p.options) > 0 && cursors == 1
	return p
}

// codexOptions splits a picker line's words into its label and description
// (as the chat page's card does: see web/src/terminal/menu.ts).
func codexOptions(labels []string) []struct{ label, desc string } {
	out := make([]struct{ label, desc string }, len(labels))
	for i, s := range labels {
		if j := strings.Index(s, "  "); j > 0 {
			out[i].label, out[i].desc = strings.TrimSpace(s[:j]), strings.TrimSpace(s[j:])
		} else {
			out[i].label = s
		}
	}
	return out
}

func (p codexPicker) cursor() (modelOpt, bool) {
	for _, o := range p.options {
		if o.at {
			return o, true
		}
	}
	return modelOpt{}, false
}

// codexEffortName: an effort choice's label without what it says of itself
// ("Medium (default)" is Medium).
func codexEffortName(label string) string {
	if i := strings.Index(label, " ("); i > 0 {
		return label[:i]
	}
	return label
}

// codexModelSet: how many times the screen says model with effort was
// taken for this session ("Model changed to gpt-6-sol high for this session
// only": the model's label in lower case, its effort's id).
func codexModelSet(screen, model, effort string) int {
	n := 0
	for _, m := range codexModelSaid.FindAllStringSubmatch(screen, -1) {
		if strings.EqualFold(m[1], strings.ReplaceAll(model, " ", "-")) && strings.EqualFold(m[2], codexEffortID(effort)) {
			n++
		}
	}
	return n
}

var codexModelSaid = regexp.MustCompile(`Model changed to (\S+) (\S+) for this session only`)

// codexEffortID: an effort as Codex says it ("Extra high" is xhigh).
func codexEffortID(effort string) string {
	if strings.EqualFold(effort, "Extra high") {
		return "xhigh"
	}
	return strings.ToLower(effort)
}

// codexPickerOpen: a picker of Codex's is open — its footer at the end of
// the screen — whether the rest of it reads as known or not.
func codexPickerOpen(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && len(lines)-i <= 4; i-- {
		if codexPickerFoot.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

const maxCodexPickerMove = 12

// codexModelFlow opens the picker; with model "" it lists the models and
// closes it, else it takes model with effort for this session. Each press
// is of the screen it was decided on: look reads the screen whole (and its
// digest), press presses only while the screen is still that one (the
// host checks: a key typed in the terminal meanwhile, or anything drawn
// since, and nothing is pressed). As modelFlow, it acts only on Codex at
// rest, its input empty (a picker of its left open is closed with Escape
// first); on a failure after the picker opened, it is left (leave: Escape
// while its picker is seen open).
func codexModelFlow(look func() (string, string, error), press func(keys []string, text, digest string) error, leave modelStep, model, effort string) (opts []modelOpt, err error) {
	screen, digest, err := look()
	if err != nil {
		return nil, err
	}
	for i := 0; i < 2 && codexPickerOpen(screen); i++ {
		if _, err := leave([]string{"esc"}, "", pickerLines); err != nil {
			return nil, err
		}
		if screen, digest, err = look(); err != nil {
			return nil, err
		}
	}
	if codexPickerOpen(screen) {
		return nil, errScreen
	}
	if err := codexIdleInput(screen); err != nil {
		return nil, err
	}
	before := codexModelSet(screen, model, effort)
	out := func(e error) ([]modelOpt, error) {
		for i := 0; i < 2; i++ {
			scr, err := leave(nil, "", pickerLines) // (looked at: Escape only on its picker)
			if err != nil || !codexPickerOpen(scr) {
				break
			}
			if _, err := leave([]string{"esc"}, "", pickerLines); err != nil {
				break
			}
		}
		return nil, e
	}
	err = press(nil, "/model", digest)
	if gateRefused(err) || screenRefused(err) {
		return nil, err // (nothing typed)
	}
	if err != nil {
		return out(err)
	}
	// (The picker as now read, and its digest: what the next press is of.)
	read := func() (codexPicker, error) {
		scr, d, err := look()
		if err != nil {
			return codexPicker{}, err
		}
		digest = d
		return parseCodexPicker(scr), nil
	}
	pk, err := read()
	if err != nil {
		return out(err)
	}
	if !pk.ok || pk.effort {
		return out(errNoPicker)
	}
	if model == "" {
		if err := press([]string{"esc"}, "", digest); err != nil {
			return out(err)
		}
		if scr, _, err := look(); err != nil || codexPickerOpen(scr) {
			return out(errScreen)
		}
		return pk.options, nil
	}
	// (Moved to the choice match finds on picker p, read again: the cursor
	// there, on the step of the same model.)
	moveTo := func(p codexPicker, match func(modelOpt) bool, same func(codexPicker) bool) (codexPicker, error) {
		var to modelOpt
		found := false
		for _, o := range p.options {
			if match(o) {
				to, found = o, true
			}
		}
		at, known := p.cursor()
		if !found || !known {
			return p, errNoModel
		}
		if d := to.N - at.N; d != 0 {
			key := "down"
			if d < 0 {
				key, d = "up", -d
			}
			if d > maxCodexPickerMove {
				return p, errNoModel
			}
			keys := make([]string, d)
			for i := range keys {
				keys[i] = key
			}
			if err := press(keys, "", digest); err != nil {
				return p, err
			}
			var err error
			if p, err = read(); err != nil {
				return p, err
			}
			if c, ok := p.cursor(); !p.ok || !ok || !match(c) || !same(p) {
				return p, errNotThere
			}
		}
		return p, nil
	}
	isModels := func(p codexPicker) bool { return !p.effort }
	if _, err := moveTo(pk, func(o modelOpt) bool { return o.Label == model }, isModels); err != nil {
		return out(err)
	}
	if err := press([]string{"yes"}, "", digest); err != nil {
		return out(err)
	}
	ofModel := func(p codexPicker) bool { return p.effort && strings.EqualFold(p.of, model) }
	ef, err := read()
	if err != nil {
		return out(err)
	}
	if !ef.ok || !ofModel(ef) {
		return out(errNotTaken)
	}
	if _, err := moveTo(ef, func(o modelOpt) bool { return strings.EqualFold(codexEffortName(o.Label), effort) }, ofModel); err != nil {
		return out(err)
	}
	if err := press([]string{"this"}, "", digest); err != nil { // ("s": this session only)
		return out(err)
	}
	scr, _, err := look()
	switch {
	case err != nil:
		return out(err)
	case codexModelSet(scr, model, effort) > before:
		return []modelOpt{{Label: model, Desc: effort}}, nil
	case codexPickerOpen(scr):
		return out(errNotTaken)
	}
	return nil, errUnsure
}

// screenRefused: the press not made, the screen not the one it was of
// (exit 10), or not to be checked there (exit 11).
func screenRefused(err error) bool {
	var e *sshx.ExitError
	return errors.As(err, &e) && (e.Status == 10 || e.Status == 11)
}
