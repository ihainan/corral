package web

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakePicker stands in for Claude Code's /model picker: what each step does
// to it, and the screen after.
type fakePicker struct {
	models  []string
	current int  // the session's model (index in models; 0 is Default)
	cursor  int  // (index)
	open    bool // the picker shows
	asking  bool // "Switch model?" shows
	history bool // the session has turns: "s" asks first
	pressed []string
	saved   bool // the default of new sessions was changed (a digit, Enter on the picker, /model NAME)
	session string
	said    []string // the thread above the input ("Set model to …", words quoted)
	draft   string   // in its input
	busy    bool
}

func (f *fakePicker) screen() string {
	var b strings.Builder
	for _, l := range f.said {
		b.WriteString(l + "\n")
	}
	if f.busy {
		b.WriteString("✻ Crunching… (12s · esc to interrupt)\n")
	}
	b.WriteString("────────────────────────\n❯ " + f.draft + "\n────────────────────────\n  Opus 5.5·H │ Ctx 0\n")
	if f.open && !f.asking { // (its question takes the picker's place)
		b.WriteString("   Select model\n   Switch between Claude models. Your pick becomes the default for new sessions.\n")
		for i, m := range f.models {
			mark, check := "  ", ""
			if i == f.cursor {
				mark = "❯ "
			}
			if i == f.current {
				check = " ✔"
			}
			fmt.Fprintf(&b, "   %s%d.  %s%s             For some tasks\n", mark, i+1, m, check)
		}
		b.WriteString("   Enter to set as default · s to use this session only · Esc to cancel\n")
	}
	if f.asking {
		b.WriteString("   Switch model?\n   ❯ 1. Yes, switch to " + f.models[f.cursor] + "\n     2. No, go back\n")
	}
	return b.String()
}

func (f *fakePicker) step(keys []string, text string, read int) (string, error) {
	for _, k := range keys {
		if _, ok := modelKeys[k]; !ok {
			return "", fmt.Errorf("key not of the set: %q", k)
		}
		f.pressed = append(f.pressed, k)
		switch {
		case f.asking && k == "yes":
			f.asking, f.open, f.current, f.session = false, false, f.cursor, f.models[f.cursor]
			f.said = append(f.said, "  ⎿  Set model to "+f.session+" for this session only")
		case f.asking && k == "esc":
			f.asking = false
		case f.open && k == "down" && f.cursor < len(f.models)-1:
			f.cursor++
		case f.open && k == "up" && f.cursor > 0:
			f.cursor--
		case f.open && k == "this":
			if f.history {
				f.asking = true
			} else {
				f.open, f.current, f.session = false, f.cursor, f.models[f.cursor]
				f.said = append(f.said, "  ⎿  Set model to "+f.session+" for this session only")
			}
		case f.open && k == "yes":
			f.saved = true
		case f.open && k == "esc":
			f.open = false
		}
	}
	if text != "" {
		f.pressed = append(f.pressed, "text:"+text)
		switch {
		case f.open:
			f.saved = true // (Enter on the picker, after the text: the default saved)
		case strings.TrimSpace(f.draft+text) == "/model":
			f.open, f.cursor, f.draft = true, f.current, ""
		default:
			f.saved = f.saved || strings.HasPrefix(strings.TrimSpace(f.draft+text), "/model ")
			f.draft = ""
		}
	}
	return f.screen(), nil
}

func newFake(history bool) *fakePicker {
	return &fakePicker{models: []string{"Default (recommended)", "Opus 5.5", "Fable 5.1", "Sonnet 5.5", "Haiku 5.5"}, current: 1, history: history}
}

func TestModelFlowLists(t *testing.T) {
	f := newFake(false)
	opts, err := modelFlow(f.step, f.step, "")
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, o := range opts {
		labels = append(labels, o.Label)
	}
	if strings.Join(labels, ",") != "Opus 5.5,Fable 5.1,Sonnet 5.5,Haiku 5.5" || !opts[0].Current {
		t.Fatalf("listed %v (%+v)", labels, opts)
	}
	if f.open || f.saved {
		t.Fatalf("left open %v, saved %v", f.open, f.saved)
	}
}

func TestModelFlowSwitches(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, target := range []string{"Haiku 5.5", "Opus 5.5", "Fable 5.1"} {
			f := newFake(history)
			f.current, f.cursor = 3, 3 // (Sonnet now: up as well as down)
			if _, err := modelFlow(f.step, f.step, target); err != nil {
				t.Fatalf("%v %s: %v", history, target, err)
			}
			if f.session != target || f.saved || f.open || f.asking {
				t.Errorf("%v %s: session %q saved %v open %v asking %v (%v)", history, target, f.session, f.saved, f.open, f.asking, f.pressed)
			}
			for _, k := range f.pressed {
				if k == "yes" && !history {
					t.Errorf("Enter pressed with no question: %v", f.pressed)
				}
			}
		}
	}
}

// Not in the picker (or Default): nothing taken, the picker closed.
func TestModelFlowRefuses(t *testing.T) {
	for _, target := range []string{"Default (recommended)", "GPT-6", ""} {
		if target == "" {
			continue
		}
		f := newFake(true)
		if _, err := modelFlow(f.step, f.step, target); !errors.Is(err, errNoModel) {
			t.Errorf("%q: %v", target, err)
		}
		if f.open || f.saved || f.session != "" {
			t.Errorf("%q: open %v saved %v session %q", target, f.open, f.saved, f.session)
		}
	}
	// The picker not showing (busy, another program): Escape, nothing more.
	f := newFake(false)
	none := func(keys []string, text string, read int) (string, error) {
		f.pressed = append(f.pressed, strings.Join(keys, "+")+"|"+text)
		return "$ ", nil
	}
	if _, err := modelFlow(none, none, "Haiku 5.5"); !errors.Is(err, errScreen) {
		t.Fatalf("no input on the screen: %v", err)
	}
	if got := strings.Join(f.pressed, " "); got != "|" {
		t.Fatalf("pressed %q", got)
	}
}

// What review 234 found: none of these presses a key that could save the
// default, nor says it switched when it is not known.
func TestModelFlowCareful(t *testing.T) {
	// The picker left open: closed with Escape first, then opened anew.
	f := newFake(true)
	f.open, f.cursor = true, 3
	if _, err := modelFlow(f.step, f.step, "Haiku 5.5"); err != nil || f.saved || f.session != "Haiku 5.5" {
		t.Errorf("picker left open: %v saved %v session %q %v", err, f.saved, f.session, f.pressed)
	}
	// Its question left open too.
	f = newFake(true)
	f.open, f.asking, f.cursor = true, true, 2
	if _, err := modelFlow(f.step, f.step, "Haiku 5.5"); err != nil || f.saved || f.session != "Haiku 5.5" {
		t.Errorf("question left open: %v saved %v session %q %v", err, f.saved, f.session, f.pressed)
	}
	// Words of the thread like its question, the picker still up after "s": no Enter.
	f = newFake(false)
	f.said = []string{"● Switch model?", "  ❯ 1. Yes, switch to Haiku 5.5"}
	stuck := func(keys []string, text string, read int) (string, error) {
		scr, err := f.step(keys, text, read)
		if len(keys) == 1 && keys[0] == "this" { // (it does not take "s")
			f.open, f.session, f.current = true, "", 1
			f.said = f.said[:2]
			return f.screen(), err
		}
		return scr, err
	}
	if _, err := modelFlow(stuck, f.step, "Haiku 5.5"); !errors.Is(err, errNotTaken) || f.saved {
		t.Errorf("quoted question: %v saved %v %v", err, f.saved, f.pressed)
	}
	for _, k := range f.pressed {
		if k == "yes" {
			t.Errorf("Enter pressed: %v", f.pressed)
		}
	}
	// Text in its input, or at work: nothing pressed at all.
	for name, set := range map[string]func(*fakePicker){"draft": func(f *fakePicker) { f.draft = "half a thought" }, "busy": func(f *fakePicker) { f.busy = true }} {
		f := newFake(false)
		set(f)
		_, err := modelFlow(f.step, f.step, "Haiku 5.5")
		if err == nil || len(f.pressed) != 0 {
			t.Errorf("%s: %v, pressed %v", name, err, f.pressed)
		}
	}
	// The picker gone, nothing said: not known (not "switched").
	f = newFake(false)
	silent := func(keys []string, text string, read int) (string, error) {
		f.step(keys, text, read)
		f.said = nil
		return f.screen(), nil
	}
	if _, err := modelFlow(silent, f.step, "Haiku 5.5"); !errors.Is(err, errUnsure) {
		t.Errorf("nothing said: %v", err)
	}
}
