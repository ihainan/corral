package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real Codex 0.167.2 screens (the screen's last lines, colours and all;
// a mock model, the test container, 11-15) under status lines of several
// configs — its default, none ([]: "100% context left" at the right of its
// hints), a thread's id, limits with no value yet (no status line drawn at
// all), items with no folder, the service tier Fast, a narrow pane — each
// with its input empty, holding "half typed draft", and that and a second
// line; and its dialogs and menus: an approval, "/" and "@" menus open.
// Its footer is to be told by where it stands, whatever its words.
func TestCodexShapes(t *testing.T) {
	files, _ := filepath.Glob("testdata/codex-shapes/*.ansi")
	if len(files) < 30 {
		t.Fatalf("%d screens", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		colour := string(b)
		plain := sgr.ReplaceAllString(colour, "")
		name := strings.TrimSuffix(filepath.Base(f), ".ansi")
		kind := name[strings.LastIndex(name, ".")+1:]
		switch {
		case kind == "empty":
			if err := codexIdleInput(plain); err != nil {
				t.Errorf("%s: idle: %v", name, err)
			}
			if codexHasDraft(colour) {
				t.Errorf("%s: a draft", name)
			}
		case kind == "draft" || kind == "draft2":
			if err := codexIdleInput(plain); err != errDraft && err != errScreen {
				t.Errorf("%s: idle: %v", name, err)
			}
			if !codexHasDraft(colour) {
				t.Errorf("%s: its draft not told", name)
			}
			want := "half typed draft"
			if kind == "draft2" {
				want += "\nsecond line"
			}
			if !codexTypedIn(plain, want, true) {
				area, rest, _, ok := codexInputArea(plain)
				t.Errorf("%s: not whole: %q / %q %v", name, area, rest, ok)
			}
			if w, k := codexInInput(plain, want); !w || !k {
				t.Errorf("%s: in input %v %v", name, w, k)
			}
		case name == "dialog-approve":
			if err := codexIdleInput(plain); err != errScreen {
				t.Errorf("%s: idle: %v", name, err)
			}
			if _, _, _, ok := codexInputArea(plain); ok {
				t.Errorf("%s: an input told on a dialog", name)
			}
		case name == "dialog-slash" || name == "dialog-at":
			if err := codexIdleInput(plain); err != errDraft {
				t.Errorf("%s: idle: %v", name, err)
			}
		case name == "dialog-history":
			if err := codexIdleInput(plain); err != nil {
				t.Errorf("%s: idle: %v", name, err)
			}
		default:
			t.Errorf("%s: not looked at", name)
		}
	}
}

// Not Codex's input: the person's message above and the agent's words under
// it (no input drawn: a pager, a closed Codex); a shell whose prompt is
// "› ", with nothing under it.
func TestCodexInputSpanNotIt(t *testing.T) {
	for _, screen := range []string{
		"› go\n\n• did it\n  more\n",
		"• done\n\n› Ask Codex to do anything\n",
		"$ ls\nfoo\n› \n",
	} {
		if err := codexIdleInput(screen); err != errScreen {
			t.Errorf("%q: %v", screen, err)
		}
	}
	if _, _, _, ok := codexInputArea("› go\n\n• did it\n"); ok {
		t.Error("an input told above the agent's words")
	}
}

// Codex's collaboration mode as its screen shows it (0.167, real lines).
func TestCodexModeOnScreen(t *testing.T) {
	for screen, want := range map[string]string{
		"› Ask Codex to do anything\n\n  GPT-6.1-Sol medium · ~/cxpl/proj · Ready · Context 100% left · 01a1def5-5907-7353-a783-c6c95fc9ca34       Plan mode (shift+tab to cycle)\n  ← for agents · ? for shortcuts\n": "plan",
		"› Ask Codex to do anything\n\n  GPT-6.1-Sol medium · ~/cxpl/proj · Ready · Co… Plan mode\n  ← for agents · ? for shortcuts\n":                                                                                 "plan",
		"› Ask Codex to do anything\n\n  GPT-6.1-Sol high · ~/cxpl/proj · Ready · Context 100% left\n  ← for agents · ? for shortcuts\n":                                                                               "default",
		"› make a Plan mode\n\n  GPT-6.1-Sol high · ~/p\n": "default", // (words of the input's)
		"  Implement this plan?\n› 1. Yes, implement this plan\n  2. Yes, clear context and implement\n  3. No, stay in Plan mode\n\n  enter select · esc back\n": "",
	} {
		if got := modeOnScreenOf("codex", screen); got != want {
			t.Errorf("%q: %q, want %q", screen[:30], got, want)
		}
	}
}

// "Implement this plan?" with its last choice marked (the cursor moved
// there): a menu, not the input — no mode read off it (review p4-2).
func TestCodexMenuLastMarked(t *testing.T) {
	screen := "  Implement this plan?\n\n  1. Yes, implement this plan\n  2. Yes, clear context and implement\n› 3. No, stay in Plan mode\n\n  enter select · esc back\n"
	if got := modeOnScreenOf("codex", screen); got != "" {
		t.Errorf("mode %q off a menu", got)
	}
	if err := codexIdleInput(screen); err != errScreen {
		t.Errorf("idle: %v", err)
	}
}

// Codex 0.169: its plan mode at the right of its hints' line, a note after
// it (real, test container, a config with a warning).
func TestCodexModeOnScreen169(t *testing.T) {
	screen := "› Ask Codex to do anything\n\n  GPT-6.1-Sol medium · ~/cxreal/proj · Ready · Context 100% left · 01a1df8f-b38e-7ed1-8b81-9b9a3db874a2\n  ← for agents · ? for shortcuts                                            Plan mode (shift+tab to cycle) · ⚠ 1 warning · f2 to view\n"
	if got := modeOnScreenOf("codex", screen); got != "plan" {
		t.Errorf("mode %q", got)
	}
	draft := "› draft words\n\n  GPT-6.1-Sol medium · ~/cxpl9/proj · Ready\n                                                                   Plan mode (shift+tab to cycle)\n"
	if got := modeOnScreenOf("codex", draft); got != "plan" {
		t.Errorf("with a draft: mode %q", got)
	}
}
