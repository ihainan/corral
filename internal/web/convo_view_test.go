package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Who has Claude Code's input box, on real screens (testdata/view: Claude
// Code 2.1.314 against a stand-in API, and real panes cut to their input box
// and what is under it), by claudeOwner and by the host's awk alike.
var ownerCases = []struct {
	file          string
	before, after string
}{
	{"main-plain.txt", "main", "main"},
	{"main-renamed.txt", "main", "main"},       // (a renamed session's name on the box's top rule)
	{"main-draft-nbsp.txt", "main", "main"},    // (a no-break space after ❯, a draft in the box)
	{"main-headsup.txt", "main", "main"},       // (a card of Claude Code's above the box)
	{"main-busy-queued.txt", "main", "main"},   // (at work, a message queued)
	{"main-agents-footer.txt", "main", "main"}, // (subagents listed under it, "● main" the one shown)
	{"subagent-list-cursor.txt", "subagent", "subagent"},
	{"subagent-typed.txt", "subagent", "subagent"},
	{"subagent-live-fullscreen.txt", "subagent", "subagent"},
	{"list-focus-main.txt", "list", "list"},       // (the list of agents holds the keys)
	{"sessions-view.txt", "sessions", "sessions"}, // (← on an empty input: a message there starts a session)
	{"permission-dialog.txt", "none", "none"},     // (a dialog in the box's place)
}

// Screens made up for what the real ones above have not shown.
var ownerMade = []struct {
	name, screen  string
	before, after string
}{
	{"placeholder alone, no list", "──────────\n❯ Message @general-purpose…\n──────────\n  ⏵⏵ auto mode on\n", "subagent", "main"},
	{"our text begins with the placeholder's words", "──────────\n❯ Message @team about it\n──────────\n  ⏵⏵ auto mode on\n", "subagent", "main"},
	{"the thread quotes a footer and a box", "❯ ◯ main\n  enter to return · space to reply\n──────────\n❯ old\n──────────\nsome answer\n──────────\n❯ \n──────────\n  ⏵⏵ auto mode on\n", "main", "main"},
	{"a box with no rule under it", "──────────\n❯ \n  ⏵⏵ auto mode on\n", "none", "none"},
	{"blank lines at the end, CRs", "──────────\r\n❯ \r\n──────────\r\n  ◯ main\r\n  ● Explore  x\r\n\r\n \r\n", "subagent", "subagent"},
	{"nothing", "", "none", "none"},
	{"a multi-line draft", "──────────\n❯ first line\n  second line\n──────────\n  ● main\n  ◯ Plan  thinking\n", "main", "main"},
	{"heavy rules", "━━━━━━━━━━\n❯ \n━━━━━━━━━━\n", "main", "main"},
	{"wrapped: the sessions view's words", "──────────\n❯ describe a\n  task for a\n  new session\n──────────\n  enter to\n  return\n", "sessions", "sessions"},
	{"wrapped: the list's keys", "──────────\n❯ \n──────────\n  ↑/↓ to\n  select\n", "list", "list"},
	{"wrapped placeholder", "──────────\n❯ Message\n  @general-purpose…\n──────────\n", "subagent", "main"},
	{"a picker under an old box", "──────────\n❯ \n──────────\n Select model\n ❯ 1. Default\n   2. Opus\n Enter to select · Esc to cancel\n", "none", "none"},
	{"ideographic spaces are no blank lines", "──────────\n❯ \n──────────\n" + strings.Repeat("\u3000\n", 13), "none", "none"},
	{"a CR inside a line", "──────────\n❯ Message \r@general-purpose…\n──────────\n", "main", "main"},
	// (A subagent's description holding a hint's words: refused, as unsure — review556.)
	{"an agent's description with a hint's words: unsure", "──────────\n❯ \n──────────\n  ● main\n  ◯ Explore  inspect Enter to confirm handler\n", "none", "none"},
	{"a hint after the agents, two spaces in it", "──────────\n❯ \n──────────\n  ● main\n  ◯ Explore  x\n  ● Enter  to confirm · Esc to cancel\n", "none", "none"},
	{"the list's keys after the agents, two spaces", "──────────\n❯ \n──────────\n  ● main\n  ◯ Explore  x\n  ● ↑/↓  to select\n", "list", "list"},
	{"a picker drawn with bullets, its keys too", "──────────\n❯ \n──────────\n Select model\n  ● Default\n  ◯ Opus\n  ● Enter to select · Esc to cancel\n", "none", "none"},
	{"a bullet hint: confirm", "──────────\n❯ \n──────────\n  ● Enter to confirm · Esc to cancel\n", "none", "none"},
	{"a bullet hint after the agents: the list's keys", "──────────\n❯ \n──────────\n  ● main\n  ◯ Explore  x\n  ● ↑/↓ to select\n", "list", "list"},
	{"a bullet hint: the sessions' view", "──────────\n❯ \n──────────\n  ● enter to return · space to reply\n", "sessions", "sessions"},
	// (A picker under the box has its cursor on one of its choices: refused, whatever its rows look like.
	// One without a cursor is no picker of Claude Code's: its pickers keep the cursor on a choice.)
	{"a branch picker, its cursor on main", "──────────\n❯ \n──────────\n Select git branch\n❯ ● main\n  ◯ feature/login  Add login\n", "list", "list"},
	{"a branch picker, its cursor on another", "──────────\n❯ \n──────────\n Select git branch\n  ● main\n❯ ◯ feature/login  Add login\n", "list", "list"},
	{"agents with a trailing space", "──────────\n❯ \n──────────\n  ● main \n  ◯ Explore  inspect the handler\n", "main", "main"},
	{"wrapped: a picker's keys", "──────────\n❯ \n──────────\n  Esc to\n  cancel\n", "none", "none"},
	{"a numbered choice under the box", "──────────\n❯ \n──────────\n  1. Yes\n  2. No\n", "none", "none"},
	{"too much under the box", "──────────\n❯ \n──────────\n" + strings.Repeat("  status\n", 13), "none", "none"},
	{"many agents listed", "──────────\n❯ \n──────────\n  ⏵⏵ auto mode on\n\n  ● main\n" + strings.Repeat("  ◯ Explore  reading\n", 15), "main", "main"},
	{"main-agent is not main", "──────────\n❯ \n──────────\n  ◯ main-agent  x\n  ● main\n", "main", "main"},
}

// ownerLocale: the locale the awk runs in (C, and a UTF-8 one: a host may have either).
var ownerLocale = "C"

func runOwnerAwk(t *testing.T, awk, screen string, after bool) string {
	t.Helper()
	a := "0"
	if after {
		a = "1"
	}
	args := []string{"-v", "after=" + a, claudeOwnerAwk}
	if awk == "busybox" {
		args = append([]string{"awk"}, args...)
	}
	cmd := exec.Command(awk, args...)
	cmd.Stdin = strings.NewReader(screen)
	cmd.Env = append(os.Environ(), "LC_ALL="+ownerLocale)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", awk, err)
	}
	return strings.TrimSpace(string(out))
}

func TestClaudeOwner(t *testing.T) {
	var awks []string
	for _, a := range []string{"gawk", "mawk", "busybox"} {
		if _, err := exec.LookPath(a); err == nil {
			awks = append(awks, a)
		}
	}
	if len(awks) == 0 {
		t.Log("no awk here: the host's program not run")
	}
	check := func(name, screen, before, after string) {
		for _, c := range []struct {
			after bool
			want  string
		}{{false, before}, {true, after}} {
			if got := claudeOwner(screen, c.after); got != c.want {
				t.Errorf("%s (after=%v): claudeOwner %q, want %q", name, c.after, got, c.want)
			}
			for _, a := range awks {
				if got := runOwnerAwk(t, a, screen, c.after); got != c.want {
					t.Errorf("%s (after=%v): %s %q, want %q", name, c.after, a, got, c.want)
				}
			}
		}
	}
	for _, loc := range []string{"C", "C.UTF-8"} {
		ownerLocale = loc
		runOwnerCases(t, check)
	}
}

func runOwnerCases(t *testing.T, check func(name, screen, before, after string)) {
	for _, c := range ownerCases {
		b, err := os.ReadFile(filepath.Join("testdata", "view", c.file))
		if err != nil {
			t.Fatal(err)
		}
		check(c.file, string(b), c.before, c.after)
	}
	for _, c := range ownerMade {
		check(c.name, c.screen, c.before, c.after)
	}
}
