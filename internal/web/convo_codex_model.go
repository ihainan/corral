package web

import (
	"regexp"
	"strings"
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

// codexModelSet: the screen says the model and effort were taken for this
// session ("Model changed to gpt-6-sol high for this session only"), as
// many times as it says so.
func codexModelSet(screen string) int {
	return strings.Count(screen, "for this session only")
}

const maxCodexPickerMove = 12

// codexModelFlow opens the picker; with model "" it lists the models and
// closes it, else it takes model with effort for this session. As
// modelFlow, it acts only on Codex at rest, its input empty, no picker of
// its open (one left open is closed with Escape first); on a failure after
// the picker opened, Escape is pressed (by leave).
func codexModelFlow(step, leave modelStep, model, effort string) (opts []modelOpt, err error) {
	screen, err := step([]string{}, "", pickerLines)
	if err != nil {
		return nil, err
	}
	for i := 0; i < 2 && parseCodexPicker(screen).ok; i++ {
		if screen, err = step([]string{"esc"}, "", pickerLines); err != nil {
			return nil, err
		}
	}
	if parseCodexPicker(screen).ok {
		return nil, errScreen
	}
	if err := codexIdleInput(screen); err != nil {
		return nil, err
	}
	before := codexModelSet(screen)
	opened, err := step(nil, "/model", pickerLines)
	out := func(e error) ([]modelOpt, error) {
		for i := 0; i < 2; i++ {
			if scr, err := leave([]string{"esc"}, "", pickerLines); err != nil || !parseCodexPicker(scr).ok {
				break
			}
		}
		return nil, e
	}
	if gateRefused(err) {
		return nil, err // (it came to wait: nothing opened)
	}
	if err != nil {
		return out(err)
	}
	pk := parseCodexPicker(opened)
	if !pk.ok || pk.effort {
		return out(errNoPicker)
	}
	if model == "" {
		if screen, err = step([]string{"esc"}, "", pickerLines); err != nil || parseCodexPicker(screen).ok {
			return out(errScreen)
		}
		return pk.options, nil
	}
	// (Moved to the choice of label on picker p, read again: the cursor there.)
	moveTo := func(p codexPicker, match func(modelOpt) bool) (codexPicker, error) {
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
			scr, err := step(keys, "", pickerLines)
			if err != nil {
				return p, err
			}
			p = parseCodexPicker(scr)
			if c, ok := p.cursor(); !p.ok || !ok || !match(c) {
				return p, errNotThere
			}
		}
		return p, nil
	}
	if _, err := moveTo(pk, func(o modelOpt) bool { return o.Label == model }); err != nil {
		return out(err)
	}
	if screen, err = step([]string{"yes"}, "", pickerLines); err != nil {
		return out(err)
	}
	ef := parseCodexPicker(screen)
	if !ef.ok || !ef.effort || !strings.EqualFold(ef.of, model) {
		return out(errNotTaken)
	}
	if _, err := moveTo(ef, func(o modelOpt) bool { return strings.EqualFold(codexEffortName(o.Label), effort) }); err != nil {
		return out(err)
	}
	if screen, err = step([]string{"this"}, "", pickerLines); err != nil { // ("s": this session only)
		return out(err)
	}
	switch {
	case codexModelSet(screen) > before:
		return []modelOpt{{Label: model, Desc: effort}}, nil
	case parseCodexPicker(screen).ok:
		return out(errNotTaken)
	}
	return nil, errUnsure
}
