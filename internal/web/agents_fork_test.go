package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A log's start copied up to a message the person typed, run here by sh as
// on a host: the lines before it, the session id the new one, the file the
// account's own; the line found by its own uuid, not by a later line naming
// it as a parent.
func TestForkCopyCommands(t *testing.T) {
	const from, at, id = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	dir := t.TempDir()
	src := filepath.Join(dir, from+".jsonl")
	lines := []string{
		`{"type":"user","uuid":"aaaaaaaa-0000-4000-8000-000000000001","sessionId":"` + from + `","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"aaaaaaaa-0000-4000-8000-000000000002","parentUuid":"aaaaaaaa-0000-4000-8000-000000000001","sessionId":"` + from + `"}`,
		`{"type":"user","uuid":"` + at + `","parentUuid":"aaaaaaaa-0000-4000-8000-000000000002","sessionId":"` + from + `","message":{"role":"user","content":"second"}}`,
		`{"type":"assistant","uuid":"aaaaaaaa-0000-4000-8000-000000000003","parentUuid":"` + at + `","sessionId":"` + from + `"}`,
	}
	if err := os.WriteFile(src, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sh := func(cmd string) (string, error) {
		out, err := exec.Command("sh", "-c", cmd).Output()
		return strings.TrimSpace(string(out)), err
	}
	off, err := sh(forkCutCommand(src, at))
	want := len(lines[0]) + len(lines[1]) + 2
	if err != nil || off != strconv.Itoa(want) {
		t.Fatalf("cut at %q (%v), want %d", off, err, want)
	}
	// (All of it: its whole lines, not one being written.)
	if err := os.WriteFile(src+".part", []byte(strings.Join(lines, "\n")+"\n{\"type\":\"user\""), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := sh(forkCutCommand(src+".part", ""))
	if whole := len(strings.Join(lines, "\n")) + 1; err != nil || all != strconv.Itoa(whole) {
		t.Fatalf("all of it: %q (%v), want %d", all, err, whole)
	}
	if _, err := sh(forkCutCommand(src, "44444444-4444-4444-8444-444444444444")); err == nil {
		t.Fatal("a uuid not there: no cut")
	}
	if !typedMessage([]byte(lines[2]), at) || typedMessage([]byte(lines[1]), "aaaaaaaa-0000-4000-8000-000000000002") ||
		typedMessage([]byte(`{"type":"user","uuid":"`+at+`","message":{"role":"user","content":[{"type":"tool_result"}]}}`), at) ||
		typedMessage([]byte(`{"type":"user","uuid":"`+at+`","isMeta":true,"message":{"role":"user","content":"x"}}`), at) {
		t.Fatal("typedMessage")
	}
	dst := filepath.Join(dir, id+".jsonl")
	if out, err := sh(forkCopyCommand(src, dst, int64(want), from, id)); err != nil || out != "ok" {
		t.Fatalf("copy: %q %v", out, err)
	}
	b, _ := os.ReadFile(dst)
	if got := string(b); got != strings.ReplaceAll(lines[0]+"\n"+lines[1]+"\n", from, id) {
		t.Fatalf("copied %q", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// (Not over one there; nothing half written left.)
	if _, err := sh(forkCopyCommand(src, dst, 10, from, id)); err == nil {
		t.Fatal("wrote over the copy")
	}
	if _, err := os.Stat(dst + ".tmp"); err == nil {
		t.Fatal("left a .tmp")
	}
	if _, err := sh(forkDropCommand(dst)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("not removed")
	}
	if id := newSessionID(); !sessionUUID.MatchString(id) {
		t.Fatalf("new id %q", id)
	}
}

// What a copy may be asked as: of Claude Code's or Codex's session (a
// UUID); up to a message, Claude Code's only; not with a worktree; Codex
// taken up only as a copy.
func TestForkRequests(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	_, id := herdrReplyEnv(t, h, admin, hd)
	path := "/api/sessions/" + id + "/agents"
	const u = "0b5d3f0e-1c1a-4f7e-9d1e-2a3b4c5d6e7f"
	for name, b := range map[string]map[string]any{
		"fork without a session":   {"kind": "claude", "fork": true},
		"fork of opencode":         {"kind": "opencode", "resume": u, "fork": true},
		"codex taken up, not copy": {"kind": "codex", "resume": u, "dir": "/tmp"},
		"at without fork":          {"kind": "claude", "resume": u, "at": u, "dir": "/tmp"},
		"at of codex":              {"kind": "codex", "resume": u, "fork": true, "at": u},
		"at not a uuid":            {"kind": "claude", "resume": u, "fork": true, "at": "x"},
		"fork in a worktree":       {"kind": "claude", "resume": u, "fork": true, "worktree": true},
		"session not a uuid":       {"kind": "claude", "resume": "../x", "fork": true},
	} {
		b["key"] = "k-" + strconv.Itoa(len(name)) + "-abcdefgh"
		mustStatus(t, admin.do("POST", path, b), 400, name)
	}
}
