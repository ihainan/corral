package web

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func cxScreen(t *testing.T, name string) string {
	b, err := os.ReadFile(os.ExpandEnv("$HOME/tmp/corral-devtools/cxscreens/" + name + ".txt"))
	if err != nil {
		t.Skip("no screen sample " + name)
	}
	return string(b)
}

// Codex 0.164's picker, as a real one drew it (cxscreens/): its two steps.
func TestParseCodexPicker(t *testing.T) {
	m := parseCodexPicker(cxScreen(t, "model"))
	c, _ := m.cursor()
	if !m.ok || m.effort || len(m.options) != 8 || c.N != 3 || m.options[0].Label != "GPT-6.1-Sol" || !strings.HasPrefix(m.options[0].Desc, "Latest") {
		t.Fatalf("models: %+v", m)
	}
	e := parseCodexPicker(cxScreen(t, "effort"))
	c, _ = e.cursor()
	if !e.ok || !e.effort || e.of != "GPT-6-Sol" || len(e.options) != 5 || c.N != 2 || codexEffortName(c.Label) != "Medium" {
		t.Fatalf("efforts: %+v", e)
	}
	// (In a narrow pane a description wraps: its line under it, indented.)
	w := parseCodexPicker(cxScreen(t, "effort-at07"))
	c, _ = w.cursor()
	if !w.ok || !w.effort || len(w.options) != 5 || c.N != 2 || !strings.HasSuffix(c.Desc, "everyday tasks") {
		t.Fatalf("efforts, wrapped: %+v", w)
	}
	if p := parseCodexPicker(cxScreen(t, "modelset")); p.ok {
		t.Fatalf("no picker: %+v", p)
	}
	if err := codexIdleInput(cxScreen(t, "modelset")); err != nil {
		t.Fatalf("idle: %v", err)
	}
	if err := codexIdleInput(strings.Replace(cxScreen(t, "modelset"), "› Ask Codex to do anything", "› half a message", 1)); err != errDraft {
		t.Fatalf("draft: %v", err)
	}
}

// A stand-in Codex for the flow: its picker drawn as a real one draws it,
// the keys pressed noted. Enter on the effort step (which saves the default)
// and digits are never to be pressed.
type fakeCodexPicker struct {
	t       *testing.T
	step    int // 0 at rest, 1 models, 2 efforts
	at      int
	of      string
	set     int
	pressed []string
}

var fakeModels = []string{"GPT-6.1-Sol", "GPT-6-Astra", "GPT-6-Sol", "GPT-6-Luna"}
var fakeEfforts = []string{"Low", "Medium (default)", "High", "Extra high"}

func (f *fakeCodexPicker) screen() string {
	var b strings.Builder
	b.WriteString("  >_ OpenAI Codex (v0.164.0)\n\n")
	b.WriteString(strings.Repeat("• Model changed to x high for this session only\n", f.set))
	switch f.step {
	case 0:
		b.WriteString("\n› Ask Codex to do anything\n\n  gpt-mock default · ~/p\n")
	default:
		list := fakeModels
		if f.step == 1 {
			b.WriteString("  Select Model and Effort\n\n")
		} else {
			list = fakeEfforts
			b.WriteString("  Select Reasoning Level for " + f.of + "\n\n")
		}
		for i, l := range list {
			mark := " "
			if i == f.at {
				mark = "›"
			}
			fmt.Fprintf(&b, "%s %d. %-18s description %d\n", mark, i+1, l, i)
		}
		if f.step == 1 {
			b.WriteString("\n  enter select · esc back\n")
		} else {
			b.WriteString("\n  enter default · s session · esc back\n")
		}
	}
	return b.String()
}

func (f *fakeCodexPicker) run(keys []string, text string, read int) (string, error) {
	if text == "/model" {
		f.step, f.at = 1, 0
	}
	for _, k := range keys {
		f.pressed = append(f.pressed, fmt.Sprint(f.step)+":"+k)
		if _, ok := modelKeys[k]; !ok {
			f.t.Fatalf("a key not of modelKeys: %q", k)
		}
		switch {
		case k == "down":
			f.at++
		case k == "up":
			f.at--
		case k == "esc":
			f.step = 0
		case k == "yes" && f.step == 1:
			f.of, f.step, f.at = fakeModels[f.at], 2, 1
		case k == "yes" && f.step == 2:
			f.t.Fatalf("Enter on the effort step: the default saved")
		case k == "this" && f.step == 2:
			f.step, f.set = 0, f.set+1
		}
	}
	return f.screen(), nil
}

func TestCodexModelFlow(t *testing.T) {
	f := &fakeCodexPicker{t: t}
	opts, err := codexModelFlow(f.run, f.run, "", "")
	if err != nil || len(opts) != 4 || opts[2].Label != "GPT-6-Sol" || f.step != 0 {
		t.Fatalf("list: %v %+v step %d", err, opts, f.step)
	}
	f = &fakeCodexPicker{t: t}
	opts, err = codexModelFlow(f.run, f.run, "GPT-6-Sol", "High")
	if err != nil || len(opts) != 1 || f.set != 1 || f.of != "GPT-6-Sol" {
		t.Fatalf("switch: %v %+v %+v", err, opts, f)
	}
	if got := strings.Join(f.pressed, " "); got != "1:down 1:down 1:yes 2:down 2:this" {
		t.Fatalf("keys: %s", got)
	}
	f = &fakeCodexPicker{t: t}
	if _, err = codexModelFlow(f.run, f.run, "GPT-9", "High"); err != errNoModel || f.step != 0 {
		t.Fatalf("no such model: %v, left at step %d", err, f.step)
	}
	f = &fakeCodexPicker{t: t}
	if _, err = codexModelFlow(f.run, f.run, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Fatalf("no such effort: %v, left at step %d", err, f.step)
	}
}
