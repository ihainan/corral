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

// Every width reads as the same four models, each with the description the
// widest pane shows: a pane the person made narrow takes nothing away.
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
			if want := pickerDescs[o.Label]; o.Desc != want {
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

// A word the pane cut in half goes back together; one it broke after does
// not lose its space. Both happen at the same column, so only the length of
// the word tells them apart (47 columns: "Efficient for" and
// "longest-runni" both end there).
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
	if d := got["Fable"]; !strings.Contains(d, "longest-running tasks") {
		t.Errorf("a word cut in half was not put back: %q", d)
	}
	if d := got["Sonnet"]; !strings.Contains(d, "Efficient for routine") {
		t.Errorf("a line broken after a word lost its space: %q", d)
	}
}

// Words about Claude Code itself are no picker. A session talking this very
// change through has both of the picker's marks on its screen; only the
// title standing on a line of its own tells the dialog from the talk.
func TestWordsAboutThePickerAreNoPicker(t *testing.T) {
	screen := strings.Join([]string{
		"  ❯ 1. 认菜单靠两行：一行正好是 Select model，另一行要含",
		"       s to use this session only 这句话。",
		"  ──────────────────────────────────────",
	}, "\n")
	if pickerShows(screen) {
		t.Error("a thread quoting the picker was taken for the picker itself")
	}
	if p := parseModelPicker(screen); p.ok {
		t.Errorf("read %+v off a thread that only talks about the picker", p.opts)
	}
}
