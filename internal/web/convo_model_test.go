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
}

func (f *fakePicker) screen() string {
	var b strings.Builder
	b.WriteString("❯ \n  Opus 5.5·H │ Ctx 0\n")
	if f.open {
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
			}
		case f.open && k == "yes":
			f.saved = true
		case f.open && k == "esc":
			f.open = false
		}
	}
	if text != "" {
		f.pressed = append(f.pressed, "text:"+text)
		if strings.TrimSpace(text) == "/model" {
			f.open, f.cursor = true, f.current
		} else if strings.HasPrefix(text, "/model ") || strings.ContainsAny(text, "0123456789") && f.open {
			f.saved = true
		}
	}
	return f.screen(), nil
}

func newFake(history bool) *fakePicker {
	return &fakePicker{models: []string{"Default (recommended)", "Opus 5.5", "Fable 5.1", "Sonnet 5.5", "Haiku 5.5"}, current: 1, history: history}
}

func TestModelFlowLists(t *testing.T) {
	f := newFake(false)
	opts, err := modelFlow(f.step, "")
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
			if _, err := modelFlow(f.step, target); err != nil {
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
		if _, err := modelFlow(f.step, target); !errors.Is(err, errNoModel) {
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
	if _, err := modelFlow(none, "Haiku 5.5"); !errors.Is(err, errNoPicker) {
		t.Fatalf("no picker: %v", err)
	}
	if got := strings.Join(f.pressed, " "); got != "clear| |/model esc|" {
		t.Fatalf("pressed %q", got)
	}
}
