package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/sshx"
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
	setTo   string // (what its "Model changed to" says: the model chosen, or another — a Codex that took another)
	pressed []string
	drawn   int // (screens drawn: a digest's)
	cut     int // (a key typed in its terminal before the press of that number: the screen changed)
	presses int
	lag     bool   // (what a press brings drawn a read late)
	shown   string // (the screen the last press left, while lagging)
	late    bool
	wrap    bool // (its "Model changed…" wrapped, as in a narrow pane)
	queue   bool // (keys taken in a read late, the screen as it was meanwhile — its digest too)
	pending [][]string
}

var fakeModels = []string{"GPT-6.1-Sol", "GPT-6-Astra", "GPT-6-Sol", "GPT-6-Luna"}
var fakeEfforts = []string{"Low", "Medium (default)", "High", "Extra high"}

func (f *fakeCodexPicker) screen() string {
	var b strings.Builder
	b.WriteString("  >_ OpenAI Codex (v0.164.0)\n\n")
	for i := 0; i < f.set; i++ {
		if f.wrap {
			b.WriteString("• Model changed to " + f.setTo + "\n  for this session only\n")
		} else {
			b.WriteString("• Model changed to " + f.setTo + " for this session only\n")
		}
	}
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

func (f *fakeCodexPicker) look() (string, string, error) {
	if f.queue && len(f.pending) > 0 {
		if f.late {
			f.late = false // (not yet: this read sees the screen before them)
			return f.screen(), fmt.Sprint(f.drawn), nil
		}
		for _, k := range f.pending {
			f.run(k, "", 0)
		}
		f.pending = nil
	}
	if f.late {
		f.late = false
		return f.shown, "stale", nil // (the screen before: its digest no longer the screen's)
	}
	return f.screen(), fmt.Sprint(f.drawn), nil
}

// press: as the host does, only on the screen read last (its digest).
func (f *fakeCodexPicker) press(keys []string, text, digest string) error {
	if keys == nil && text == "" {
		f.t.Fatalf("an empty paste, and Enter (review498)")
	}
	f.presses++
	if f.lag {
		f.shown, f.late = f.screen(), true
	}
	if f.presses == f.cut {
		f.drawn++ // (the person pressed a key in the terminal: another screen)
	}
	if digest != fmt.Sprint(f.drawn) {
		return &sshx.ExitError{Status: 10}
	}
	if f.queue && text == "" {
		f.pending, f.late = append(f.pending, keys), true
		return nil
	}
	f.run(keys, text, 0)
	return nil
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
			if f.setTo == "" {
				f.setTo = strings.ToLower(f.of) + " " + codexEffortID(codexEffortName(fakeEfforts[f.at]))
			}
			f.step, f.set = 0, f.set+1
		}
	}
	if text != "" || len(keys) > 0 {
		f.drawn++
	}
	return f.screen(), nil
}

func TestCodexModelFlow(t *testing.T) {
	codexSettleGap = 0
	flow := func(f *fakeCodexPicker, model, effort string) ([]modelOpt, error) {
		io := codexIO{f.look, f.press}
		return codexModelFlow(io, io, model, effort)
	}
	f := &fakeCodexPicker{t: t}
	opts, err := flow(f, "", "")
	if err != nil || len(opts) != 4 || opts[2].Label != "GPT-6-Sol" || f.step != 0 {
		t.Fatalf("list: %v %+v step %d", err, opts, f.step)
	}
	f = &fakeCodexPicker{t: t}
	opts, err = flow(f, "GPT-6-Sol", "High")
	if err != nil || len(opts) != 1 || f.set != 1 || f.of != "GPT-6-Sol" {
		t.Fatalf("switch: %v %+v %+v", err, opts, f)
	}
	if got := strings.Join(f.pressed, " "); got != "1:down 1:down 1:yes 2:down 2:this" {
		t.Fatalf("keys: %s", got)
	}
	f = &fakeCodexPicker{t: t}
	if _, err = flow(f, "GPT-9", "High"); err != errNoModel || f.step != 0 {
		t.Fatalf("no such model: %v, left at step %d", err, f.step)
	}
	f = &fakeCodexPicker{t: t}
	if _, err = flow(f, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Fatalf("no such effort: %v, left at step %d", err, f.step)
	}
	// (review493: a key typed in the terminal between a read and a press —
	// at each press in turn: that press not made, the picker left, never
	// Enter on the effort step; nothing said to be done.)
	for cut := 1; cut <= 5; cut++ {
		f = &fakeCodexPicker{t: t, cut: cut}
		_, err = flow(f, "GPT-6-Sol", "High")
		if !screenRefused(err) || f.step != 0 || f.set != 0 {
			t.Errorf("screen changed before press %d: %v, step %d, set %d (%v)", cut, err, f.step, f.set, f.pressed)
		}
	}
	// (review493: what it says it took is another model's: not done.)
	f = &fakeCodexPicker{t: t, setTo: "gpt-6-luna high"}
	if _, err = flow(f, "GPT-6-Sol", "High"); err == nil {
		t.Errorf("another model said taken: ok")
	}
	// (review498: Codex draws what a press brought a read late: read again.)
	f = &fakeCodexPicker{t: t, lag: true}
	if _, err = flow(f, "GPT-6-Sol", "High"); err != nil || f.set != 1 {
		t.Errorf("drawn late: %v, set %d (%v)", err, f.set, f.pressed)
	}
	// (review498: its "Model changed…" wrapped: done all the same.)
	f = &fakeCodexPicker{t: t, wrap: true}
	if _, err = flow(f, "GPT-6-Sol", "High"); err != nil {
		t.Errorf("wrapped: %v", err)
	}
	// (review498: left after a failure on the effort step — Escape only,
	// never Enter (the fake fails on Enter there, and on an empty paste).)
	f = &fakeCodexPicker{t: t}
	if _, err = flow(f, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Errorf("no such effort, left: %v step %d (%v)", err, f.step, f.pressed)
	}
	// (review501: leaving, the Escape taken in a read late: not pressed again.)
	f = &fakeCodexPicker{t: t, queue: true}
	if _, err = flow(f, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Errorf("left, drawn late: %v step %d", err, f.step)
	}
	if n := strings.Count(strings.Join(f.pressed, " "), ":esc"); n != 1 {
		t.Errorf("left, drawn late: %d Escapes (%v)", n, f.pressed)
	}
	// (A picker left open that does not read as known (its footer there):
	// closed first, then on as at rest.)
	f = &fakeCodexPicker{t: t, step: 1}
	scr := f.screen()
	if !codexPickerOpen(strings.Replace(scr, "1. ", "1) ", 1)) || parseCodexPicker(strings.Replace(scr, "1. ", "1) ", 1)).ok {
		t.Fatalf("a picker not read as known, its footer there")
	}
}
