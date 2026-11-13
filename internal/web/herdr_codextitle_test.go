package web

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexTitleParts(t *testing.T) {
	for _, c := range []struct {
		agent, title, cwd, thread string
		bare                      bool
	}{
		{"codex", "调研项目最新进展 | AI", "/home/u/Projects/AI", "调研项目最新进展", false},
		{"codex", "AI", "/home/u/Projects/AI", "", true},
		{"codex", "", "/home/u/Projects/AI", "", true},
		{"codex", "  ⠋ AI", "/home/u/Projects/AI", "", true},
		{"codex", "ZGCAI-Agent-Platform-...", "/home/u/AI/ZGCAI-Agent-Platform-dev2", "", true},
		{"codex", "修 bug | ZGCAI-Agent-Platform-…", "/home/u/AI/ZGCAI-Agent-Platform-dev2", "修 bug", false},
		{"codex", "a | b | AI", "/x/AI", "a | b", false},
		{"codex", "下载全季 | Movie", "/home/u/smb/Movie/", "下载全季", false},
		{"codex", "something else", "/x/AI", "", false}, // (a title of its own: as it is)
		{"codex", "fix | Other", "/x/AI", "", false},    // (not its folder)
		{"codex", "home", "/home/u", "", false},         // (a home folder names no project)
		{"codex-cli", "AI", "/x/AI", "", true},
		{"claude", "AI", "/x/AI", "", false},   // (not a Codex)
		{"codex", "Z-...", "/x/AB", "", false}, // (cut short, not of it)
		{"codex", "...", "/x/AB", "", false},
	} {
		th, b := codexTitleParts(c.agent, c.title, c.cwd)
		if th != c.thread || b != c.bare {
			t.Errorf("%q %q %q: %q %v, want %q %v", c.agent, c.title, c.cwd, th, b, c.thread, c.bare)
		}
	}
}

func TestCodexName(t *testing.T) {
	long := strings.Repeat("字", 100)
	for _, c := range []struct{ name, title, first, want string }{
		{"命名", "标题", "第一句", "命名"},
		{"", "标题", "第一句", "标题"},
		{"  ", "", "第一句", "第一句"},
		{"", "[Image #1] 检查红圈问题。\n\n[external unsupported block: image]", "", "检查红圈问题。"},
		{"", "\n\n  第二行  有\t空白 \n第三行", "", "第二行 有 空白"},
		{"", "[Image #1]\n[Image #2]", "看这两张图", "看这两张图"},
		{"", long, "", strings.Repeat("字", codexNameRunes-1) + "…"},
		{"", "", "", ""},
	} {
		if got := codexName(c.name, c.title, c.first); got != c.want {
			t.Errorf("%q %q %q: %q, want %q", c.name, c.title, c.first, got, c.want)
		}
	}
}

func TestParseCodexPanes(t *testing.T) {
	const a = "01a1cd3b-8a27-75f1-ba56-dd9f7f22cd01"
	const b = "01a1cd3b-8685-7b23-9bfc-b218af59d6d1"
	rec := func(f ...string) string {
		return strings.Join(append(f, "1", "resume", "0", "0", "sub2api.example.org"), "\x1f") + "\x00"
	}
	out := rec("w2:p1", "965934", "69873389", "1794460425000", "/home/u/AI", a, "/home/u/.codex/sessions/2026/11/12/rollout-2026-11-12T08-49-53-"+a+".jsonl", "", "[Image #1] 检查红圈", "first") +
		rec("w3:p1", "100", "200", "0", "/home/u/AI", b, "/home/u/.codex/sessions/x/rollout-1-"+a+".jsonl", "named", "t", "f") + // (a log of another thread)
		rec("w4:p1", "101", "201", "0", "/x", b, "/home/u/.codex/sessions/../../etc/rollout-1-"+b+".jsonl", "", "", "") + // (a path leaving its folder)
		rec("w5:p1", "102", "202", "0", "/x", b, "/home/u/.codex/sessions/x/rollout-1-"+b+"_2.jsonl", "", "", "") + // (a segment)
		rec("w6:p1", "103", "203", "0", "/x", "", "", "", "", "") + // (no thread known)
		rec("w7:p1", "104", "204", "0", "/x", a, "", "", "", "") + rec("w7:p1", "105", "205", "0", "/x", b, "", "", "", "") + // (two in one pane)
		rec("bad pane", "1", "2", "0", "/x", a, "", "", "", "") +
		rec("w8:p1", "0", "2", "0", "/x", a, "", "", "", "") + // (no process)
		"w9:p1\x1fshort\x00"
	got := parseCodexPanes(out)
	if w := got["w2:p1"]; w.Via != "sub2api.example.org" || !w.FG || w.PID != 965934 || w.Start != 69873389 || w.Started != 1794460425000 || w.Thread != a || !strings.HasSuffix(w.Path, a+".jsonl") || w.Cwd != "/home/u/AI" {
		t.Fatalf("w2: %+v", w)
	}
	if got["w3:p1"].Path != "" || got["w4:p1"].Path != "" || got["w5:p1"].Path == "" || got["w6:p1"].Thread != "" {
		t.Fatalf("paths: %+v %+v %+v %+v", got["w3:p1"], got["w4:p1"], got["w5:p1"], got["w6:p1"])
	}
	if !got["w7:p1"].Several || got["w7:p1"].Thread != "" {
		t.Fatalf("two in a pane: %+v", got["w7:p1"])
	}
	if _, ok := got["bad pane"]; ok {
		t.Fatal("a bad pane id taken")
	}
	if _, ok := got["w8:p1"]; ok {
		t.Fatal("no process taken")
	}
	known := parseCodexPanes(strings.Join([]string{"w9:p1", "1", "2", "0", "/x", "", "", "", "", "", "1", "", "0", "0", "="}, "\x1f") + "\x00")["w9:p1"]
	if !known.viaKnown || known.Via != "" {
		t.Fatalf("a known endpoint: %+v", known)
	}
	if bad := parseCodexPanes(strings.Join([]string{"w9:p1", "1", "2", "0", "/x", "", "", "", "", "", "1", "", "0", "0", "evil host;rm"}, "\x1f") + "\x00")["w9:p1"]; bad.Via != "" {
		t.Fatalf("a host not a host: %+v", bad)
	}
	names := codexNames(got)
	if names["w2:p1"] != "检查红圈" || names["w3:p1"] != "named" || names["w7:p1"] != "" || len(names) != 2 {
		t.Fatalf("names %v", names)
	}
}

func TestCodexTitleAgrees(t *testing.T) {
	for _, c := range []struct {
		title, cwd, name string
		want             bool
	}{
		{"调研项目最新进展 | AI", "/x/AI", "调研项目最新进展", true},
		{"⠋ 调研项目最新进展 | AI", "/x/AI", "调研项目最新进展", true},
		{"AI", "/x/AI", "", true},                    // (no name: the folder alone)
		{"AI", "/x/AI", "named since", false},        // (a thread with a name, its title bare: another one)
		{"新会话 | AI", "/x/AI", "调研项目最新进展", false},     // (another thread: /new, /resume)
		{"新会话 | AI", "/x/AI", "", false},             // (a name where the thread has none)
		{"调研项目最... | AI", "/x/AI", "调研项目最新进展", true}, // (cut short)
		{"something else", "/x/AI", "x", false},      // (a title of another form)
	} {
		if got := codexTitleAgrees(c.title, c.cwd, c.name); got != c.want {
			t.Errorf("%q %q %q: %v", c.title, c.cwd, c.name, got)
		}
	}
}

// Looked for only while a pane's title is bare: soon after it comes to be,
// told to that pane's summary; let go once its title names its thread.
func TestCodexTitler(t *testing.T) {
	var scans atomic.Int32
	c := newCodexTitler(func(context.Context) (map[string]string, error) {
		scans.Add(1)
		return map[string]string{"w1:p1": "检查红圈", "w9:p1": "another"}, nil
	})
	c.every = time.Hour
	poked := make(chan struct{}, 4)
	c.setPoke(func() { poked <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)
	sum := func(bare bool, thread string) herdrSummary {
		return herdrSummary{Workspaces: []herdrWorkspace{{ID: "w1", Agents: []herdrAgent{{PaneID: "w1:p1", Agent: "codex", BareTitle: bare, ThreadTitle: thread}}}}}
	}
	time.Sleep(50 * time.Millisecond)
	if scans.Load() != 0 {
		t.Fatal("looked for with none wanted")
	}
	s := sum(true, "")
	c.track(&s)
	select {
	case <-poked:
	case <-time.After(5 * time.Second):
		t.Fatal("not looked for")
	}
	c.decorate(&s)
	if got := s.Workspaces[0].Agents[0].ThreadTitle; got != "检查红圈" {
		t.Fatalf("told %q", got)
	}
	if _, ok := c.found["w9:p1"]; ok {
		t.Fatal("kept one not wanted")
	}
	// Its title names its thread now: that stands, and what was found is let go.
	s = sum(false, "新名字")
	c.track(&s)
	c.decorate(&s)
	if got := s.Workspaces[0].Agents[0].ThreadTitle; got != "新名字" || len(c.found) != 0 {
		t.Fatalf("named: %q %v", got, c.found)
	}
	if n := scans.Load(); n != 1 {
		t.Fatalf("%d looks", n)
	}
}
