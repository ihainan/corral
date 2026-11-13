package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseGitInfo(t *testing.T) {
	out := "TOP /home/a/proj\n" +
		"# branch.oid 1234\n# branch.head feat/x\n# branch.upstream origin/feat/x\n# branch.ab +2 -1\n" +
		"N 3 1\n" +
		"LAST abc1234 1700000000 Fix: the thing (and more)\n" +
		"BASE origin/main\nBASEAHEAD 5\n" +
		"DIFF  4 files changed, 120 insertions(+), 7 deletions(-)\n"
	g := parseGitInfo(out)
	if !g.Repo || g.Root != "/home/a/proj" || g.Branch != "feat/x" || g.Detached || g.Upstream != "origin/feat/x" || g.Ahead != 2 || g.Behind != 1 {
		t.Fatalf("head: %+v", g)
	}
	if !g.Status || g.Changed != 3 || g.Untracked != 1 {
		t.Fatalf("status: %+v", g)
	}
	if g.Last == nil || g.Last.Hash != "abc1234" || g.Last.Time != 1700000000 || g.Last.Subject != "Fix: the thing (and more)" {
		t.Fatalf("last: %+v", g.Last)
	}
	if g.Base != "origin/main" || g.BaseAhead != 5 || g.Diff == nil || *g.Diff != (gitDiff{4, 120, 7}) {
		t.Fatalf("base: %+v %+v", g, g.Diff)
	}
	for in, want := range map[string]string{"E nodir\n": "nodir", "E nogit\n": "nogit", "E norepo\n": "norepo", "": "norepo"} {
		if g := parseGitInfo(in); g.Repo || g.Reason != want {
			t.Errorf("%q: %+v", in, g)
		}
	}
	// A status not read in time: the rest still is, and says so.
	g = parseGitInfo("TOP /p\nE status\nLAST a 1 s\n")
	if !g.Repo || g.Status || g.Last == nil {
		t.Errorf("no status: %+v", g)
	}
	// Detached; one sided stats.
	g = parseGitInfo("TOP /p\n# branch.head (detached)\nN 0 0\nDIFF 1 file changed, 1 insertion(+)\n")
	if !g.Detached || g.Branch != "" || g.Diff == nil || *g.Diff != (gitDiff{1, 1, 0}) {
		t.Errorf("detached: %+v %+v", g, g.Diff)
	}
}

func TestParseGitPR(t *testing.T) {
	if gh, pr := parseGitPR("NOGH\n"); gh || pr != nil {
		t.Error("no gh")
	}
	if gh, pr := parseGitPR("NOPR\n"); !gh || pr != nil {
		t.Error("no pr")
	}
	gh, pr := parseGitPR(`{"isDraft":true,"number":12,"state":"OPEN","title":"Add it","url":"https://github.com/o/r/pull/12"}`)
	if !gh || pr == nil || pr.Number != 12 || !pr.Draft || pr.State != "OPEN" || pr.URL != "https://github.com/o/r/pull/12" {
		t.Errorf("pr: %v %+v", gh, pr)
	}
	for _, bad := range []string{`{"number":1,"url":"javascript:alert(1)"}`, `{"number":0,"url":"https://x"}`, `not json`} {
		if _, pr := parseGitPR(bad); pr != nil {
			t.Errorf("%s: %+v", bad, pr)
		}
	}
}

// The command itself, run here (as the host would run it) on repositories
// made for the test.
func TestGitInfoCommandRuns(t *testing.T) {
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
	info := func(dir string) gitInfo {
		t.Helper()
		// (The command is "sh -c <script> corral <dir>", as the host's shell runs it.)
		out, err := exec.Command("sh", "-c", gitInfoCommand(dir)).Output()
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return parseGitInfo(string(out))
	}
	// A remote with a default branch; a clone of it, a branch off it, work in it.
	remote := filepath.Join(root, "remote")
	run(root, "init", "-q", "-b", "main", remote)
	os.WriteFile(filepath.Join(remote, "a.txt"), []byte("one\n"), 0o644)
	run(remote, "add", ".")
	run(remote, "commit", "-qm", "first")
	clone := filepath.Join(root, `it's a "dir" with spaces`)
	run(root, "clone", "-q", remote, clone)
	run(clone, "checkout", "-qb", "feat")
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	run(clone, "commit", "-qam", "more lines")
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644)
	os.WriteFile(filepath.Join(clone, "new.txt"), []byte("x\n"), 0o644)
	g := info(clone)
	if !g.Repo || g.Branch != "feat" || g.Upstream != "" || !g.Status || g.Changed != 1 || g.Untracked != 1 {
		t.Fatalf("clone: %+v", g)
	}
	if g.Last == nil || g.Last.Subject != "more lines" {
		t.Fatalf("last: %+v", g.Last)
	}
	if g.Base != "origin/main" || g.BaseAhead != 1 || g.Diff == nil || g.Diff.Files != 1 || g.Diff.Add != 3 {
		t.Fatalf("base: %+v %+v", g, g.Diff)
	}
	// No index lock taken (an agent's git beside it would fail on one).
	if _, err := os.Stat(filepath.Join(clone, ".git", "index.lock")); err == nil {
		t.Fatal("index.lock left")
	}
	// Tracking its upstream: ahead and behind.
	run(clone, "push", "-qu", "origin", "feat")
	if g := info(clone); g.Upstream != "origin/feat" || g.Ahead != 0 || g.Behind != 0 {
		t.Fatalf("upstream: %+v", g)
	}
	// Detached.
	run(clone, "stash", "-qu")
	run(clone, "checkout", "-q", "--detach")
	if g := info(clone); !g.Detached || g.Branch != "" {
		t.Fatalf("detached: %+v", g)
	}
	// A folder that is no repository; one that is not there.
	plain := filepath.Join(root, "plain")
	os.Mkdir(plain, 0o755)
	if g := info(plain); g.Repo || g.Reason != "norepo" {
		t.Fatalf("plain: %+v", g)
	}
	if g := info(filepath.Join(root, "gone")); g.Repo || g.Reason != "nodir" {
		t.Fatalf("gone: %+v", g)
	}
	// No origin/HEAD (a repository made here): no base, and so no diff.
	run(root, "init", "-q", "-b", "main", filepath.Join(root, "local"))
	if g := info(filepath.Join(root, "local")); !g.Repo || g.Base != "" || g.Diff != nil {
		t.Fatalf("local: %+v", g)
	}
}

func TestGitCallsOneAtATime(t *testing.T) {
	var c gitCalls
	var calls atomic.Int32
	slow := func() (any, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return 1, nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := c.do(context.Background(), "k", time.Second, slow); err != nil || v != 1 {
				t.Errorf("%v %v", v, err)
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("asked %d times", n)
	}
	// Cached a while; asked again after.
	c.do(context.Background(), "k", time.Second, slow)
	if calls.Load() != 1 {
		t.Fatal("not cached")
	}
	c.do(context.Background(), "k", time.Nanosecond, slow)
	if calls.Load() != 2 {
		t.Fatal("kept too long")
	}
}

// CORRAL_GIT_DIR=<a repository> go test -run TestGitInfoOnDir -v: the command
// on a real one, timed (how long a card's open costs on a big repository).
func TestGitInfoOnDir(t *testing.T) {
	dir := os.Getenv("CORRAL_GIT_DIR")
	if dir == "" {
		t.Skip("CORRAL_GIT_DIR not set")
	}
	start := time.Now()
	out, err := exec.Command("sh", "-c", gitInfoCommand(dir)).Output()
	if err != nil {
		t.Fatal(err)
	}
	g := parseGitInfo(string(out))
	t.Logf("%v %+v diff=%+v last=%+v", time.Since(start).Round(time.Millisecond), g, g.Diff, g.Last)
	start = time.Now()
	out, _ = exec.Command("sh", "-c", gitPRCommand(dir)).Output()
	gh, pr := parseGitPR(string(out))
	t.Logf("pr %v: gh=%v %+v", time.Since(start).Round(time.Millisecond), gh, pr)
}
