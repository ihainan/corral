package web

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/term"
	"github.com/ihainan/corral/internal/testutil"
)

func TestGitChangesCommandRuns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	changesFrom := func(dir, against, file, from string) gitChanges {
		t.Helper()
		nonce := gitNonce()
		out, err := exec.Command("sh", "-c", gitChangesCommand(dir, against, file, from, nonce)).Output()
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		g, err := parseGitChanges(string(out), nonce)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return g
	}
	changes := func(dir, against, file string) gitChanges { t.Helper(); return changesFrom(dir, against, file, "") }
	byPath := func(g gitChanges) map[string]gitChangedFile {
		m := map[string]gitChangedFile{}
		for _, f := range g.Files {
			m[f.Path] = f
		}
		return m
	}
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remote := filepath.Join(root, "remote")
	run(root, "init", "-q", "-b", "main", remote)
	write(filepath.Join(remote, "a.txt"), "one\ntwo\nthree\n")
	write(filepath.Join(remote, "gone.txt"), "bye\n")
	write(filepath.Join(remote, "pic.bin"), "\x00\x01\x02")
	write(filepath.Join(remote, "old name.txt"), strings.Repeat("same line\n", 20))
	run(remote, "add", ".")
	run(remote, "commit", "-qm", "first")
	clone := filepath.Join(root, `it's a "dir"`)
	run(root, "clone", "-q", remote, clone)
	run(clone, "checkout", "-qb", "feat")
	write(filepath.Join(clone, "a.txt"), "one\nTWO\nthree\nfour\n")
	run(clone, "commit", "-qam", "on the branch")
	// Uncommitted (against HEAD, the branch's commit): a line added with no newline at its end, a deletion, a binary, a rename, a staged new file with odd names; untracked ones.
	write(filepath.Join(clone, "a.txt"), "one\nTWO\nthree\nfour\nfive")
	os.Remove(filepath.Join(clone, "gone.txt"))
	write(filepath.Join(clone, "pic.bin"), "\x00\x09\x02")
	run(clone, "mv", "old name.txt", "new name.txt")
	write(filepath.Join(clone, "sub", `q"uote ü.txt`), "x\n")
	run(clone, "add", "sub")
	write(filepath.Join(clone, "loose.txt"), "u\n")
	write(filepath.Join(clone, "newdir", "f.txt"), "u\n")

	// From a subfolder: the repository's paths, from its top.
	g := changes(filepath.Join(clone, "sub"), "work", "")
	if !g.Repo || g.Root != clone || g.More || len(g.Files) != 5 {
		t.Fatalf("work: %+v", g)
	}
	m := byPath(g)
	if f := m["a.txt"]; f.Status != "M" || f.Add != 1 || f.Del != 0 || len(f.Hunks) != 1 || f.Cut || f.Omitted {
		t.Fatalf("a.txt: %+v", f)
	}
	// ("\ No newline at end of file" is no line of it.)
	for _, l := range m["a.txt"].Hunks[0].Lines {
		if !strings.ContainsAny(l[:1], " +-") {
			t.Fatalf("a.txt line %q", l)
		}
	}
	if f := m["gone.txt"]; f.Status != "D" || f.Del != 1 || len(f.Hunks) != 1 {
		t.Fatalf("gone: %+v", f)
	}
	if f := m["pic.bin"]; !f.Binary || f.Hunks != nil {
		t.Fatalf("binary: %+v", f)
	}
	if f := m["new name.txt"]; f.Status != "R" || f.From != "old name.txt" || f.Add != 0 || f.Del != 0 {
		t.Fatalf("rename: %+v", f)
	}
	if f := m[`sub/q"uote ü.txt`]; f.Status != "A" || f.Add != 1 || len(f.Hunks) != 1 || f.Hunks[0].Lines[0] != "+x" {
		t.Fatalf("quoted name: %+v", f)
	}
	if strings.Join(g.Untracked, ",") != "loose.txt,newdir/" || g.UntrackedMore {
		t.Fatalf("untracked: %q", g.Untracked)
	}
	// One file alone; a path asked for literally (no pathspec magic).
	if g := changes(clone, "work", "a.txt"); len(g.Files) != 1 || g.Files[0].Path != "a.txt" || g.Untracked != nil {
		t.Fatalf("one: %+v", g)
	}
	if g := changes(clone, "work", ":(glob)*.txt"); len(g.Files) != 0 {
		t.Fatalf("magic: %+v", g)
	}
	// Against the base: the branch's commit and what is not committed, from the merge-base.
	g = changes(clone, "base", "")
	if g.Base != "origin/main" || g.Untracked != nil {
		t.Fatalf("base: %+v", g)
	}
	if f := byPath(g)["a.txt"]; f.Add != 3 || f.Del != 1 {
		t.Fatalf("base a.txt: %+v", f)
	}
	// No base; no commit yet; no repository.
	local := filepath.Join(root, "local")
	run(root, "init", "-q", "-b", "main", local)
	if g := changes(local, "base", ""); g.Repo || g.Reason != "nobase" {
		t.Fatalf("nobase: %+v", g)
	}
	write(filepath.Join(local, "first.txt"), "1\n")
	run(local, "add", ".")
	if g := changes(local, "work", ""); len(g.Files) != 1 || g.Files[0].Status != "A" {
		t.Fatalf("no commit: %+v", g)
	}
	if g := changes(filepath.Join(root, "nowhere"), "work", ""); g.Reason != "nodir" {
		t.Fatalf("nodir: %+v", g)
	}
	// A conflict being resolved: a file of its own, those around it still theirs. A blank line of
	// context kept as one, whatever the person's config; an empty untracked folder not listed.
	run(local, "-c", "diff.suppressBlankEmpty=true", "commit", "-qm", "first")
	write(filepath.Join(local, "c.txt"), "a\n\nb\n")
	write(filepath.Join(local, "z.txt"), "z\n")
	run(local, "add", ".")
	run(local, "commit", "-qm", "two")
	run(local, "checkout", "-qb", "x")
	write(filepath.Join(local, "c.txt"), "a\n\nX\n")
	run(local, "commit", "-qam", "x")
	run(local, "checkout", "-q", "main")
	write(filepath.Join(local, "c.txt"), "a\n\nM\n")
	write(filepath.Join(local, "z.txt"), "z\nzz\n")
	run(local, "commit", "-qam", "m")
	c := exec.Command("git", "merge", "x")
	c.Dir = local
	c.Run() // (fails: the conflict)
	write(filepath.Join(local, "first.txt"), "1\n2\n")
	os.Mkdir(filepath.Join(local, "empty"), 0o755)
	run(local, "config", "diff.suppressBlankEmpty", "true")
	g = changes(local, "work", "")
	m = byPath(g)
	if len(g.Files) != 2 || m["c.txt"].Omitted || m["first.txt"].Omitted || m["first.txt"].Add != 1 || g.Untracked != nil {
		t.Fatalf("conflict: %+v", g)
	}
	if f := m["c.txt"]; len(f.Hunks) != 1 || f.Cut || f.Hunks[0].Lines[1] != " " {
		t.Fatalf("conflict's blank line: %+v", f)
	}
	// Review, 11-15: a change of type (a link become a file: two chunks of one record) and a
	// rename to a name git quotes, each with its own lines, the files after them still theirs;
	// a file whose name holds what once was a marker; a rename read alone, as a rename.
	odd := filepath.Join(root, "odd")
	run(root, "init", "-q", "-b", "main", odd)
	os.Symlink("target", filepath.Join(odd, "link"))
	write(filepath.Join(odd, "old.txt"), strings.Repeat("line\n", 30))
	write(filepath.Join(odd, "bad\n\x01RC 128\nsuffix"), "x\n")
	write(filepath.Join(odd, "z.txt"), "z\n")
	run(odd, "add", ".")
	run(odd, "commit", "-qm", "i")
	os.Remove(filepath.Join(odd, "link"))
	write(filepath.Join(odd, "link"), "now a file\n")
	run(odd, "mv", "old.txt", `new"name.txt`)
	write(filepath.Join(odd, `new"name.txt`), strings.Repeat("line\n", 30)+"more\n")
	write(filepath.Join(odd, "bad\n\x01RC 128\nsuffix"), "y\n")
	write(filepath.Join(odd, "z.txt"), "z\nzz\n")
	run(odd, "add", "-A")
	g = changes(odd, "work", "")
	m = byPath(g)
	if f := m["link"]; f.Status != "T" || len(f.Hunks) != 2 || f.Omitted {
		t.Fatalf("type change: %+v", f)
	}
	if f := m[`new"name.txt`]; f.Status != "R" || f.From != "old.txt" || f.Add != 1 || len(f.Hunks) != 1 || f.Omitted {
		t.Fatalf("quoted rename: %+v", f)
	}
	if f := m["z.txt"]; f.Add != 1 || len(f.Hunks) != 1 || f.Omitted {
		t.Fatalf("after them: %+v", f)
	}
	if _, ok := m["bad\n\x01RC 128\nsuffix"]; ok || len(g.Files) != 3 {
		t.Fatalf("marker name: %+v", g.Files)
	}
	run(odd, "commit", "-qm", "j")
	run(odd, "mv", "z.txt", "zz.txt")
	if g := changesFrom(odd, "work", "zz.txt", "z.txt"); len(g.Files) != 1 || g.Files[0].Status != "R" || g.Files[0].Add != 0 {
		t.Fatalf("rename alone: %+v", g.Files)
	}
	// Past the cap: cut on the host, said so; the files after it listed, their diffs left out.
	big := filepath.Join(root, "big")
	run(root, "init", "-q", "-b", "main", big)
	write(filepath.Join(big, "a.txt"), "")
	write(filepath.Join(big, "z.txt"), "")
	run(big, "add", ".")
	run(big, "commit", "-qm", "empty")
	write(filepath.Join(big, "a.txt"), strings.Repeat(strings.Repeat("x", 99)+"\n", gitChangesCap/100+10))
	write(filepath.Join(big, "z.txt"), "z\n")
	g = changes(big, "work", "")
	if len(g.Files) != 2 || !g.Files[0].Cut || g.Files[0].Add != gitChangesCap/100+10 || !g.Files[1].Omitted || g.Files[1].Add != 1 {
		t.Fatalf("cut: %+v", g.Files)
	}
	if g := changes(big, "work", "z.txt"); len(g.Files) != 1 || g.Files[0].Cut || len(g.Files[0].Hunks) != 1 {
		t.Fatalf("one after the cut: %+v", g.Files)
	}
}

func TestCleanRepoPath(t *testing.T) {
	for _, p := range []string{"a.txt", "sub/b c.go", `q"uote.txt`, "ü/x"} {
		if !cleanRepoPath(p) {
			t.Errorf("refused %q", p)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/./b", "a//b", "-p", "..", ".", "a\nb", "a\x00b", "a/", strings.Repeat("a", gitChangesPathMax+1)} {
		if cleanRepoPath(p) {
			t.Errorf("took %q", p)
		}
	}
}

func TestParseGitChangesStates(t *testing.T) {
	const n = "0123abcd"
	mk := func(parts ...string) string { return "TOP /r\n\n\x01" + n + " " + strings.Join(parts, "\n\x01"+n+" ") }
	// Given up on: not kept. Failed: an error. No timeout(1): said.
	if _, err := parseGitChanges(mk("DIFF\n", "RC 124\n", "END\n"), n); err != errGitTimeout {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := parseGitChanges(mk("DIFF\n", "RC 128\n", "END\n"), n); err == nil {
		t.Fatal("failed diff taken")
	}
	if g, err := parseGitChanges("E notimeout\n", n); err != nil || g.Reason != "notimeout" || g.Files == nil {
		t.Fatalf("notimeout: %+v %v", g, err)
	}
	// Nothing changed.
	if g, err := parseGitChanges(mk("DIFF\n", "RC 0\n", "UNTRACKED\n", "URC 0\n", "END\n"), n); err != nil || !g.Repo || len(g.Files) != 0 || g.Untracked != nil || g.UntrackedMore {
		t.Fatalf("clean: %+v %v", g, err)
	}
	// The untracked list given up on, or cut: not whole, not "none".
	for _, u := range []string{"URC 124\n", "END\n"} {
		if g, _ := parseGitChanges(mk("DIFF\n", "RC 0\n", "UNTRACKED\na\x00", u, "END\n"), n); !g.UntrackedMore || len(g.Untracked) != 1 {
			t.Fatalf("untracked %q: %+v", u, g)
		}
	}
	// A marker without the nonce is a name's bytes, nothing more.
	if _, err := parseGitChanges(mk("DIFF\n\n\x01RC 128\n", "RC 0\n", "END\n"), n); err != nil {
		t.Fatalf("forged marker: %v", err)
	}
	// The list itself cut: what was whole of it, said to be more.
	g, _ := parseGitChanges(mk("DIFF\n:100644 100644 a b M\x00a.txt\x00:100644 100644 a b M\x00b.t"), n)
	if !g.More || len(g.Files) != 0 {
		t.Fatalf("cut list: %+v", g)
	}
}

// The cache: an answer kept its while and no longer; never more than so many,
// so much, the oldest going first.
func TestGitCallsBounded(t *testing.T) {
	var c gitCalls
	ctx := context.Background()
	big := &gitChanges{Files: []gitChangedFile{{Path: strings.Repeat("x", 4<<20)}}}
	for i := 0; i < 3*gitCacheMax; i++ {
		c.do(ctx, strconv.Itoa(i), time.Hour, func(context.Context) (any, error) { return 1, nil })
	}
	if len(c.m) > gitCacheMax {
		t.Fatalf("%d kept", len(c.m))
	}
	for i := 0; i < 20; i++ {
		c.do(ctx, "big"+strconv.Itoa(i), time.Hour, func(context.Context) (any, error) { return big, nil })
	}
	if c.bytes > gitCacheBytes {
		t.Fatalf("%d bytes kept", c.bytes)
	}
	c.do(ctx, "short", time.Millisecond, func(context.Context) (any, error) { return 1, nil })
	time.Sleep(5 * time.Millisecond)
	c.do(ctx, "next", time.Hour, func(context.Context) (any, error) { return 1, nil })
	c.mu.Lock()
	_, kept := c.m["short"]
	c.mu.Unlock()
	if kept {
		t.Fatal("kept past its while")
	}
}

// Through the gateway: the pane's folder from tmux, the script run on the host,
// what the page may ask checked.
func TestGitChangesEndpoint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	c := exec.Command("sh", "-c", "git init -q -b main proj && cd proj && echo a > a.txt && git add . && git -c user.name=t -c user.email=t@t commit -qm i && echo b >> a.txt && echo u > new.txt")
	c.Dir = home
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	srv := h.sshServer(home)
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\n[ \"$5\" = \"=work:\" ] && echo '"+repo+"'\n"), 0o755)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, "display-message") || strings.Contains(cmd, "GIT_LITERAL_PATHSPECS")
	}, "GIT_CONFIG_GLOBAL=/dev/null"))
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "tmux", "tmux_session": "work"})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
	mustStatus(t, r, 201, "open session")
	url := "/api/sessions/" + r.json()["id"].(string) + "/git/changes?against="
	for _, bad := range []string{"", "HEAD", "work&path=../x", "work&path=%2Fetc%2Fpasswd", "work&path=-p", "work&path=a%0Ab"} {
		if r := admin.do("GET", url+bad, nil); r.Status != http.StatusBadRequest {
			t.Errorf("%q: %d %s", bad, r.Status, r.Body)
		}
	}
	r = admin.do("GET", url+"work", nil)
	mustStatus(t, r, 200, "changes")
	j := r.json()
	files, _ := j["files"].([]any)
	if j["root"] != repo || len(files) != 1 || files[0].(map[string]any)["path"] != "a.txt" || len(j["untracked"].([]any)) != 1 {
		t.Fatalf("answer: %s", r.Body)
	}
	r = admin.do("GET", url+"work&path=a.txt", nil)
	mustStatus(t, r, 200, "one file")
	// No remote: nothing to compare with, said.
	r = admin.do("GET", url+"base", nil)
	mustStatus(t, r, 200, "base")
	if r.json()["reason"] != "nobase" {
		t.Fatalf("base: %s", r.Body)
	}
}

// The pane as the host is asked of it: none of a tmux session's (so not a key
// the page can vary), a herdr pane's of its form.
func TestGitPane(t *testing.T) {
	tmux, hd := &term.Session{Persistence: "tmux"}, &term.Session{Persistence: "herdr"}
	for _, c := range []struct {
		s    *term.Session
		in   string
		want string
		ok   bool
	}{{tmux, "%3", "", true}, {tmux, "anything", "", true}, {hd, "w1:p2", "w1:p2", true}, {hd, "w1 p2", "", false}, {hd, strings.Repeat("a", gitPaneMax+1), "", false}, {hd, "", "", true}} {
		if got, ok := gitPane(c.s, c.in); got != c.want || ok != c.ok {
			t.Errorf("%s %q: %q %v", c.s.Persistence, c.in, got, ok)
		}
	}
}
