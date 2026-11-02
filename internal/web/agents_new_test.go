package web

import (
	"strings"
	"testing"
)

func TestAgentDir(t *testing.T) {
	for in, want := range map[string]string{
		"/home/a/proj": "/home/a/proj", "/home/a/../b/": "/home/b", "~": "~", "~/x/../y": "~/y", "~/../../etc": "~/etc",
		"proj": "", "": "", "~x": "", "/a\nb": "",
	} {
		got, ok := agentDir(in)
		if (want == "") == ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
}

func TestAgentNames(t *testing.T) {
	if l := agentLabel(" a\tb ", "/x/proj"); l != "ab" {
		t.Errorf("label %q", l)
	}
	if l := agentLabel("", "/x/proj"); l != "proj" {
		t.Errorf("label from the folder %q", l)
	}
	for in, want := range map[string]string{"Fix login bug": "fix-login-bug", "修复登录": "claude", "--x--": "x", "a/b": "a-b", "2026-project": "claude-2026-project"} {
		if n := agentNameBase(in, "claude"); n != want {
			t.Errorf("%q: %q", in, n)
		}
	}
	// (herdr's rule: a lowercase letter first, then letters, digits, - and _, at most 32; one each)
	a, b := agentName("2026-project with a very long name indeed", "claude"), agentName("2026-project with a very long name indeed", "claude")
	if a == b || len(a) > 32 || a[0] < 'a' || a[0] > 'z' || strings.Trim(a, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		t.Errorf("names %q %q", a, b)
	}
	if n := claudeProjectName("/home/a/b_c.d"); n != "-home-a-b-c-d" {
		t.Errorf("project %q", n)
	}
}

func TestReadSessionHead(t *testing.T) {
	head := []byte(`{"type":"permission-mode","permissionMode":"default"}
{"type":"user","isMeta":true,"cwd":"/home/a/p","message":{"role":"user","content":"<local-command-caveat>x"}}
{"type":"user","cwd":"/home/a/p","message":{"role":"user","content":"fix the login page"}}
{"type":"ai-title","aiTitle":"Login fix"}
{"type":"user","cwd":"/home/a/p","message":{"role":"user","content":"and the tests"}}
{"type":"assis`)
	cs, cwd := readSessionHead(head)
	if cwd != "/home/a/p" || cs.First != "fix the login page" || cs.Title != "Login fix" {
		t.Fatalf("%+v %q", cs, cwd)
	}
}

// A request said again (its key) gets the first one's answer; another one
// meanwhile, in the same session, waits for none: it is refused.
func TestNewAgentsOnce(t *testing.T) {
	var a newAgents
	r, first, busy := a.take("s\x00u\x00k1", "s\x00u")
	if !first || busy {
		t.Fatal("the first")
	}
	if _, first, busy := a.take("s\x00u\x00k2", "s\x00u"); first || !busy {
		t.Fatal("another meanwhile: not refused")
	}
	if r2, first, _ := a.take("s\x00u\x00k1", "s\x00u"); first || r2 != r {
		t.Fatal("said again meanwhile: not the same")
	}
	a.finish("s\x00u\x00k1", "s\x00u", r, 200, []byte(`{"pane":"p1"}`))
	if r2, first, _ := a.take("s\x00u\x00k1", "s\x00u"); first || string(r2.body) != `{"pane":"p1"}` {
		t.Fatal("said again after: not its answer")
	}
	if _, first, busy := a.take("s\x00u\x00k3", "s\x00u"); !first || busy {
		t.Fatal("after it: a new one")
	}
}

func TestAgentStartCommandArgs(t *testing.T) {
	tail := func(c string) string {
		if i := strings.Index(c, " -- "); i >= 0 {
			return c[i+4:]
		}
		return ""
	}
	cases := []struct {
		kind, resume string
		worktree     bool
		want         string
	}{
		{"claude", "", false, ""},
		{"claude", "", true, "-w"},
		{"codex", "", true, "--worktree"},
		{"opencode", "", true, ""}, // (none of its own)
		{"claude", "0b5d3f0e-1c1a-4f7e-9d1e-2a3b4c5d6e7f", false, "--resume '0b5d3f0e-1c1a-4f7e-9d1e-2a3b4c5d6e7f'"},
	}
	for _, c := range cases {
		cmd := agentStartCommand("/run/h.sock", "w1:p1", "proj", c.kind, c.resume, c.worktree)
		if got := tail(cmd); got != c.want {
			t.Fatalf("%s resume=%q worktree=%v: agent's args %q, want %q (%s)", c.kind, c.resume, c.worktree, got, c.want, cmd)
		}
		if !strings.Contains(cmd, "herdr agent start 'proj' --kind '"+c.kind+"' --pane 'w1:p1'") {
			t.Fatalf("herdr's part changed: %s", cmd)
		}
	}
}
