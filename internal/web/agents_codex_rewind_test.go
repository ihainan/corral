package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Words back in Codex's input after a rewind, as real screens of several
// status-line settings draw them — one with no footer under its input at
// all (review fork3-9) — and not its placeholder, nor while it works.
func TestCodexInputHolds(t *testing.T) {
	ansi := regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	want := []rune(squeeze("half typed"))
	for _, name := range []string{"default.draft", "limits.draft", "gitbranch.draft", "nocolor.draft", "threadid.draft"} {
		b, err := os.ReadFile("testdata/codex-shapes/" + name + ".ansi")
		if err != nil {
			t.Fatal(err)
		}
		if !codexInputHolds(ansi.ReplaceAllString(string(b), ""), want) {
			t.Errorf("%s: its draft not seen", name)
		}
	}
	b, err := os.ReadFile("testdata/codex-shapes/default.empty.ansi")
	if err != nil {
		t.Fatal(err)
	}
	empty := ansi.ReplaceAllString(string(b), "")
	if codexInputHolds(empty, []rune("Ask")) {
		t.Error("its placeholder taken for words")
	}
}

// "/rename <name>" seen alone in Codex's input, under it a footer of any
// status line the shapes have (one with a thread's id alone, one of model,
// version and state — review fork5-2), not with a draft's lines under it.
func TestCodexHoldsOnly(t *testing.T) {
	ansi := regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	typed := "/rename proj fork 1a2b"
	for _, name := range []string{"default.draft", "limits.draft", "gitbranch.draft", "nocolor.draft", "threadid.draft", "misc.draft", "user.draft", "fast.draft"} {
		b, err := os.ReadFile("testdata/codex-shapes/" + name + ".ansi")
		if err != nil {
			t.Fatal(err)
		}
		scr := strings.Replace(ansi.ReplaceAllString(string(b), ""), "half typed draft", typed, 1)
		if !codexHoldsOnly(scr, typed) {
			t.Errorf("%s: not seen alone", name)
		}
		if codexHoldsOnly(scr, typed+" more") {
			t.Errorf("%s: taken for other words", name)
		}
	}
	two := "› " + typed + "\n\n  a draft line\n  and another\n"
	if codexHoldsOnly(two, typed) {
		t.Error("a draft's lines under it taken for its footer")
	}
}
