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
	read := func(name string) string {
		b, err := os.ReadFile("testdata/codex-shapes/" + name + ".ansi")
		if err != nil {
			t.Fatal(err)
		}
		return ansi.ReplaceAllString(string(b), "")
	}
	for _, shape := range []string{"default", "limits", "gitbranch", "nocolor", "threadid", "misc", "user", "fast"} {
		status := codexRestStatus(read(shape + ".empty"))
		scr := strings.Replace(read(shape+".draft"), "half typed draft", typed, 1)
		if !codexHoldsOnly(scr, typed, status) {
			t.Errorf("%s: not seen alone (status %q)", shape, status)
		}
		if codexHoldsOnly(scr, typed+" more", status) {
			t.Errorf("%s: taken for other words", shape)
		}
	}
	for _, scr := range []string{
		"› " + typed + "\n\n  a draft line\n  and another\n",
		"› " + typed + "\n\n  extra draft\n", // (one line of a draft's, no status line at rest: fork6-3)
	} {
		if codexHoldsOnly(scr, typed, "") {
			t.Errorf("a draft's line under it taken for its footer: %q", scr)
		}
	}
	if codexHoldsOnly("› "+typed+"\n\n  extra draft\n", typed, "GPT-6.1-Sol high · ~/cxreal/proj") {
		t.Error("a draft's line taken for the status line it was not")
	}
}
