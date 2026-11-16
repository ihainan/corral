package web

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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
	for _, shape := range []string{"default", "limits", "gitbranch", "nocolor", "threadid", "misc", "user", "fast", "empty", "narrow"} {
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
	// (A line like a footer, with no status line at rest: a draft's — review fork7-1.)
	if codexHoldsOnly("› "+typed+"\n\n  Context 50% left\n", typed, "") {
		t.Error("a draft's line like a footer taken for one")
	}
	// (Other digits not taken as moving: another model, another folder — review fork8-4.)
	if codexHoldsOnly("› "+typed+"\n\n  gpt-7.2 · ~/proj2\n", typed, "gpt-6.1 · ~/proj1") {
		t.Error("another status line taken for its own")
	}
	// (Its figures moved since: the same status line — review fork7-3.)
	if !codexHoldsOnly("› "+typed+"\n\n  gpt · Context 48% left · 5h 12% used\n", typed, "gpt · Context 50% left · 5h 11% used") {
		t.Error("its status line, its figures moved, not taken")
	}
}

// The copy's Codex in its tmux pane: the pane's process itself, or — npm's
// node keeping itself on a Node without execve — its one child named codex
// (review fork7-2); its fork argument read.
func TestTmuxCopyCodexSh(t *testing.T) {
	if _, err := os.Stat("/proc/self/task"); err != nil {
		t.Skip("no /proc")
	}
	dir := t.TempDir()
	codex := dir + "/codex"
	// (A program named codex: sh copied, so its comm says codex.)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	b, err := os.ReadFile(sh)
	if err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(codex, b, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "01a1e301-0b3e-7b40-8617-af7d350a7eb7"
	run := func(pp int) string {
		out, _ := exec.Command("sh", "-c", tmuxCopyCodexSh(strconv.Itoa(pp))).Output()
		return strings.TrimSpace(string(out))
	}
	// Itself: codex -c 'sleep 5' fork <id> (sh takes "fork" and id as $0, $1).
	self := exec.Command(codex, "-c", "sleep 5", "fork", id)
	if err := self.Start(); err != nil {
		t.Fatal(err)
	}
	defer self.Process.Kill()
	time.Sleep(200 * time.Millisecond)
	if got := strings.Fields(run(self.Process.Pid)); len(got) != 3 || got[0] != strconv.Itoa(self.Process.Pid) || got[2] != id {
		t.Errorf("the pane's own Codex: %q", got)
	}
	// Under a parent of another name (as npm's node): its one child named codex.
	parent := exec.Command("sh", "-c", `"$0" -c 'sleep 5' fork `+id+`; true`, codex)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer parent.Process.Kill()
	time.Sleep(300 * time.Millisecond)
	if got := strings.Fields(run(parent.Process.Pid)); len(got) != 3 || got[0] == strconv.Itoa(parent.Process.Pid) || got[2] != id {
		t.Errorf("a child of the pane's process: %q", got)
	}
	// Two children named codex: not guessed between.
	two := exec.Command("sh", "-c", `"$0" -c 'sleep 5' fork `+id+` & "$0" -c 'sleep 5' fork `+id+`; wait`, codex)
	if err := two.Start(); err != nil {
		t.Fatal(err)
	}
	defer two.Process.Kill()
	time.Sleep(300 * time.Millisecond)
	if got := run(two.Process.Pid); got != "" {
		t.Errorf("two children taken: %q", got)
	}
}

// A status line's moving figures set aside, alone or in a row, whatever
// their unit; digits within a word kept (review fork8-4, fork9-1, fork9-2).
func TestCodexDigitless(t *testing.T) {
	same := [][2]string{
		{"gpt · Context 50% left · 5h 11% used", "gpt · Context 48% left · 5h 12% used"},
		{"123 tokens", "124 tokens"},
		{"999 tokens", "1K tokens"},
		{"elapsed 59s", "elapsed 1m 0s"},
		{"141K / 200K", "142K / 200K"},
		{"at 12:30", "at 12:31"},
		{"(50%)", "(49%)"},
	}
	for _, c := range same {
		if codexDigitless(c[0]) != codexDigitless(c[1]) {
			t.Errorf("%q and %q not taken alike: %q, %q", c[0], c[1], codexDigitless(c[0]), codexDigitless(c[1]))
		}
	}
	other := [][2]string{
		{"gpt-6.1 · ~/proj1", "gpt-7.2 · ~/proj2"},
		{"~/ctx-32k", "~/ctx-128k"},
		{"~/job-1m", "~/job-2m"},
		{"main", "main2"},
	}
	for _, c := range other {
		if codexDigitless(c[0]) == codexDigitless(c[1]) {
			t.Errorf("%q and %q taken alike: %q", c[0], c[1], codexDigitless(c[0]))
		}
	}
}
