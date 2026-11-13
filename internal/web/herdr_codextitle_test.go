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

func TestParseCodexTitles(t *testing.T) {
	out := "w2:p1\x1fid1\x1f\x1f[Image #1] 检查红圈\x1ffirst\x00" +
		"w3:p1\x1fid2\x1fnamed\x1ft\x1ff\x00" +
		"bad pane\x1fid\x1fn\x1ft\x1ff\x00" + // (not a pane id)
		"w4:p1\x1fid3\x1f\x1f\x1f\x00" + // (nothing to show)
		"w5:p1\x1fshort\x00" // (cut off)
	got := parseCodexTitles(out)
	if len(got) != 2 || got["w2:p1"] != "检查红圈" || got["w3:p1"] != "named" {
		t.Fatalf("got %v", got)
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
