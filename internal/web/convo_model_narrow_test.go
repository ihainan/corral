package web

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The model picker as Claude Code 2.1.319 really draws it, caught off four
// panes of different widths (testdata/model-picker-<columns>.txt). A pane
// of 55 columns or fewer breaks the picker's keys' line in two, which is
// how the picker went unread on a 47-column pane on 2026-11-09; narrower
// still, the models' descriptions are pushed onto the lines under them.

// The descriptions as the widest pane, which wraps nothing, has them.
var pickerDescs = map[string]string{
	"Opus":   "Opus 5.5 · Best for everyday, complex tasks",
	"Fable":  "Fable 5.1 · Most capable for your hardest and longest-running tasks",
	"Sonnet": "Sonnet 5.5 · Efficient for routine tasks",
	"Haiku":  "Haiku 5.5 · Fastest for quick answers",
}

func realPicker(t *testing.T, cols int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "model-picker-"+strconv.Itoa(cols)+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every width retains the same model names and description characters.
// The layout is preserved: plain text cannot recover lost word boundaries.
func TestRealPickerEveryWidth(t *testing.T) {
	for _, cols := range []int{47, 80, 100, 120} {
		screen := realPicker(t, cols)
		if !pickerShows(screen) {
			t.Errorf("%d columns: the picker was not seen on the screen", cols)
		}
		p := parseModelPicker(screen)
		if !p.ok {
			t.Fatalf("%d columns: nothing read off the picker", cols)
		}
		var got []string
		for _, o := range p.offered() {
			got = append(got, o.Label)
			if want := pickerDescs[o.Label]; flatScreen(o.Desc) != flatScreen(want) {
				t.Errorf("%d columns: %s says\n\t%q\nwant\t%q", cols, o.Label, o.Desc, want)
			}
		}
		if want := "Opus Fable Sonnet Haiku"; strings.Join(got, " ") != want {
			t.Errorf("%d columns: models %q, want %q", cols, strings.Join(got, " "), want)
		}
		// Its cursor sits on the first entry, the one left out of the list.
		c, ok := p.cursor()
		if !ok || c.N != 1 || c.Label != "" {
			t.Errorf("%d columns: cursor %+v, ok %v; want the first entry", cols, c, ok)
		}
	}
}

// The line that went unread: "Enter to set as default · s to use this
// session only · Esc to cancel" broken where a narrow pane breaks it.
func TestPickerKeysBrokenOverTwoLines(t *testing.T) {
	screen := strings.Join([]string{
		"  Select model",
		"  ❯ 1. Default (recommended) ✔  Opus 5.5 · Best for everyday",
		"    2. Haiku                   Haiku 5.5 · Fastest",
		"  ◐ Medium effort (default) ←/→ to adjust",
		"  Enter to set as default · s to use this",
		"  session only · Esc to cancel",
	}, "\n")
	if !pickerShows(screen) {
		t.Error("the picker was not seen with its keys' line broken in two")
	}
	if p := parseModelPicker(screen); !p.ok || len(p.offered()) != 1 || p.offered()[0].Label != "Haiku" {
		t.Errorf("read %+v; want the one model it offers", p.opts)
	}
}

// Nothing of the picker on the screen is still nothing.
func TestPickerNotShown(t *testing.T) {
	for _, screen := range []string{
		"",
		"  ❯ some words typed\n  ────────────────",
		// (Its keys without its title: another dialog's.)
		"  Enter to set as default · s to use this session only · Esc to cancel",
		// (Its title without its keys: the words of a thread quoting it.)
		"  ● Select model\n  I opened Select model for you",
	} {
		if pickerShows(screen) {
			t.Errorf("the picker was seen where there is none: %q", screen)
		}
		if p := parseModelPicker(screen); p.ok {
			t.Errorf("read %+v off a screen with no picker: %q", p.opts, screen)
		}
	}
}

// "Set model to X for this session only" is longer than a narrow pane, so
// it arrives broken; the switch must still count as taken.
func TestTakenAcrossLines(t *testing.T) {
	whole := "  ⎿  Set model to Haiku 5.5 for this session only"
	broken := "  ⎿  Set model to Haiku 5.5 for this\n     session only"
	for _, screen := range []string{whole, broken, whole + "\n" + broken} {
		want := strings.Count(screen, "Set model to")
		if got := taken(screen, "Haiku 5.5"); got != want {
			t.Errorf("taken = %d, want %d, on:\n%s", got, want, screen)
		}
	}
	if got := taken(broken, "Opus 5.5"); got != 0 {
		t.Errorf("taken = %d for a model the screen does not name", got)
	}
}

// The question after "s", its choice broken over two lines, is still read —
// and still only for the model it names.
func TestAskShowsAcrossLines(t *testing.T) {
	screen := strings.Join([]string{
		"  Switch model?",
		"  ❯ 1. Yes, switch to Haiku",
		"       5.5",
		"    2. No, keep the model",
	}, "\n")
	if !askShows(screen, "Haiku 5.5") {
		t.Error("the question went unread with its choice broken in two")
	}
	if askShows(screen, "Opus 5.5") {
		t.Error("the question was read as naming a model it does not name")
	}
	// A thread quoting it is behind a mark and is no question.
	if askShows("  ● Switch model?\n  ❯ 1. Yes, switch to Haiku 5.5", "Haiku 5.5") {
		t.Error("words of the thread were taken for the question itself")
	}
}

// Both hard-split words and breaks between words are kept as captured.
// Their original boundaries cannot be inferred from column widths.
func TestDescriptionPutBackTogether(t *testing.T) {
	p := parseModelPicker(realPicker(t, 47))
	for _, o := range p.offered() {
		if strings.Contains(o.Desc, "  ") {
			t.Errorf("%s: two spaces run together in %q", o.Label, o.Desc)
		}
	}
	got := map[string]string{}
	for _, o := range p.offered() {
		got[o.Label] = o.Desc
	}
	for _, o := range p.offered() {
		if strings.Contains(o.Desc, "\n") {
			t.Errorf("%s: a line of the pane came through in %q", o.Label, o.Desc)
		}
	}
	// A line the pane broke after a word reads as it was written.
	if d := got["Sonnet"]; d != "Sonnet 5.5 · Efficient for routine tasks" {
		t.Errorf("word-wrapped description changed: %q", d)
	}
	// A word the pane cut in half keeps the space, and is meant to: no
	// reading of the text can tell that break from the one above it, so
	// the rarer case is the one left showing.
	if d := got["Fable"]; d != "Fable 5.1 · Most capable for your hardest and longest-runni ng tasks" {
		t.Errorf("hard-split description changed: %q", d)
	}
}

// Words about Claude Code itself are no picker. A session talking this very
// change through has the picker's words on its screen; what tells them from
// the dialog is that Claude Code's input box still stands under them, which
// an open picker takes the place of.
func TestWordsAboutThePickerAreNoPicker(t *testing.T) {
	screen := strings.Join([]string{
		"  ❯ 1. 认菜单靠两行：一行正好是 Select model，另一行要含",
		"       s to use this session only 这句话。",
		"  ──────────────────────────────────────",
		"  ❯",
		"  ──────────────────────────────────────",
		"    Opus 5·H │ Ctx 305.5K",
	}, "\n")
	if pickerShows(screen) {
		t.Error("a thread quoting the picker was taken for the picker itself")
	}
	if p := parseModelPicker(screen); p.ok {
		t.Errorf("read %+v off a thread that only talks about the picker", p.opts)
	}
}

// A pane too short for the whole picker scrolls its title away: of 47
// columns the picker runs to 33 lines, and a session whose status line
// takes two of 34 rows has no room left for it. Read off the keys' line
// upwards, the models are all still there.
func TestPickerTitleScrolledAway(t *testing.T) {
	lines := strings.Split(strings.TrimRight(realPicker(t, 47), "\n"), "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "Select model" {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("the fixture has no title to scroll away")
	}
	screen := strings.Join(append(append([]string{}, lines[at+1:]...),
		"  Opus 5·H │ Ctx 305.5K (30.5% / υ 38.2%) │ …",
		"  ⏵⏵ auto mode on (shift+tab to cycle) · ⧉ 6"), "\n")
	if !pickerShows(screen) {
		t.Fatal("the picker went unseen with its title scrolled off the top")
	}
	p := parseModelPicker(screen)
	var got []string
	for _, o := range p.offered() {
		got = append(got, o.Label)
		if want := pickerDescs[o.Label]; flatScreen(o.Desc) != flatScreen(want) {
			t.Errorf("%s says %q, want %q", o.Label, o.Desc, want)
		}
	}
	if want := "Opus Fable Sonnet Haiku"; strings.Join(got, " ") != want {
		t.Errorf("models %q, want %q", strings.Join(got, " "), want)
	}
}

// The picker as it was served a few hours later the same day: the models
// named in full, two spaces after the number, a first entry cut short with
// an ellipsis, and a row that opens more of them. Caught off a pane of the
// very shape this went wrong on (47 columns, 34 rows).
func TestPickerOtherShape(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "model-picker-47-six.txt"))
	if err != nil {
		t.Fatal(err)
	}
	screen := string(b)
	if !pickerShows(screen) {
		t.Fatal("the picker was not seen in its other shape")
	}
	p := parseModelPicker(screen)
	var got []string
	for _, o := range p.offered() {
		got = append(got, o.Label)
	}
	if want := "Opus 5.5 Fable 5.1 Sonnet 5.5 Haiku 5.5"; strings.Join(got, " ") != want {
		t.Errorf("models %q, want %q", strings.Join(got, " "), want)
	}
	if d := p.offered()[0].Desc; d != "For complex work and everyday tasks" {
		t.Errorf("the first model says %q", d)
	}
	if c, ok := p.cursor(); !ok || c.N != 1 || c.Label != "" {
		t.Errorf("cursor %+v ok %v; want the entry cut short, left out", c, ok)
	}
}

// Claude Code drawing the picker a moment late: the first looks at the
// screen find nothing of it. The flow must look again rather than give up
// on one glance — a heavy session does not repaint within the first read.
func TestModelFlowWaitsForASlowPicker(t *testing.T) {
	f := newFake(false)
	late := 2 // (reads that find the screen as it was before the picker)
	slow := func(keys []string, text string, read int) (string, error) {
		s, err := f.step(keys, text, read)
		if err != nil || !f.open || late == 0 {
			return s, err
		}
		late--
		return "  ──────────────\n  ❯\n  ──────────────\n", nil
	}
	opts, err := modelFlow(slow, f.step, "")
	if err != nil {
		t.Fatalf("gave up on a picker drawn late: %v", err)
	}
	var labels []string
	for _, o := range opts {
		labels = append(labels, o.Label)
	}
	if strings.Join(labels, ",") != "Opus 5.5,Fable 5.1,Sonnet 5.5,Haiku 5.5" {
		t.Fatalf("listed %v", labels)
	}
	if late != 0 {
		t.Fatalf("the screen was not looked at again (%d looks left)", late)
	}
}

// A pane too short for a long list: Claude Code keeps the title and cuts
// the foot away — the keys' line with it — marking what is left with an
// arrow. Seen on 2026-11-09 on a pane of 47 columns and 34 rows holding
// fourteen models: title there, keys gone, eight rows read.
func TestPickerFootCutAway(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "model-picker-47-six.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	cut := -1
	for i, l := range lines {
		if strings.Contains(l, "Enter to set as default") {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatal("the fixture has no keys line to cut")
	}
	lines = lines[:cut]
	for i := len(lines) - 1; i >= 0; i-- { // (the arrow sits at the end of the last line left)
		if strings.TrimSpace(lines[i]) != "" {
			lines[i] += "     ↓"
			break
		}
	}
	screen := strings.Join(lines, "\n")
	if wrapAt(lines, pickerKeys, 0) >= 0 {
		t.Fatal("the keys line was not cut away")
	}
	if !pickerShows(screen) {
		t.Fatal("the picker went unseen with its foot cut away")
	}
	p := parseModelPicker(screen)
	var got []string
	for _, o := range p.offered() {
		got = append(got, o.Label)
	}
	if want := "Opus 5.5 Fable 5.1 Sonnet 5.5 Haiku 5.5"; strings.Join(got, " ") != want {
		t.Errorf("models %q, want %q", strings.Join(got, " "), want)
	}
}

// With neither end of the picker on the screen there is nothing to read,
// and a thread quoting its words is no picker either: the input box under
// them says so.
func TestPickerNeitherEnd(t *testing.T) {
	rows := "  ❯ 1. Opus                     Opus 5.5 · Best for everyday\n    2. Haiku                   Haiku 5.5 · Fastest"
	if p := parseModelPicker(rows); p.ok {
		t.Errorf("read %+v off rows with neither title nor keys", p.opts)
	}
	quoted := "  Select model\n  ❯ 1. Opus    Opus 5.5\n  ──────────────\n  ❯\n  ──────────────\n    Opus 5·H │ Ctx 0"
	if pickerShows(quoted) {
		t.Error("a thread quoting the picker was taken for one, its input box in plain sight")
	}
}
