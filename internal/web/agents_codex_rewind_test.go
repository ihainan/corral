package web

import (
	"os"
	"regexp"
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
