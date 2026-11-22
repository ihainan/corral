package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgyTitleBare(t *testing.T) {
	cases := []struct {
		agent, title, cwd string
		want              bool
	}{
		{"agy", "", "/home/u/AI", true},
		{"agy", "agy", "/home/u/AI", true},
		{"agy", "agy --conversation 123", "/home/u/AI", true},
		{"agy", "agy --conversation=7741b9ca-3d5d-448a-8418-1a8464f064bf --mode auto", "/home/u/AI", true},
		{"agy", "aswap run 1 --share-history -- --conversation  -i", "/home/u/AI", true},
		{"agy", "✳ agy", "/home/u/AI", true},
		{"agy", "AI", "/home/u/AI", true},
		{"agy", "Antigravity", "/home/u/AI", true},
		{"antigravity", "agy", "/home/u/AI", true},
		{"agy", "自定义标题", "/home/u/AI", false},
		{"claude", "agy", "/home/u/AI", false},
		{"codex", "AI", "/home/u/AI", false},
	}
	for _, tc := range cases {
		if got := agyTitleBare(tc.agent, tc.title, tc.cwd); got != tc.want {
			t.Errorf("agyTitleBare(%q, %q, %q) = %v, want %v", tc.agent, tc.title, tc.cwd, got, tc.want)
		}
	}
}

func TestCleanAndFilterAgyPrompts(t *testing.T) {
	raw := "<USER_REQUEST>\n/home/ihainan/.cache/corral/uploads/20261121-105200-954941bf.png 下面的描述文字，有两个问题：\n\n1. 每次重启显示目录名\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-21\n</ADDITIONAL_METADATA>"
	got := cleanAgyPrompt(raw)
	want := "下面的描述文字，有两个问题： 1. 每次重启显示目录名"
	if got != want {
		t.Fatalf("cleanAgyPrompt = %q, want %q", got, want)
	}

	for _, triv := range []string{"", "继续", "确认。", "可以！", "好的", "OK", "yes!", "1", "  "} {
		if !isTrivialAgyPrompt(triv) {
			t.Errorf("isTrivialAgyPrompt(%q) = false, want true", triv)
		}
	}
	for _, real := range []string{"排查 nginx 403", "chat mode 模型加载提速", "fix menu"} {
		if isTrivialAgyPrompt(real) {
			t.Errorf("isTrivialAgyPrompt(%q) = true, want false", real)
		}
	}

	info1 := agyPaneTitleInfo{
		Pane:         "w28:p1",
		SID:          "6490c7d4-0626-4ed8-a158-da789586d5b1",
		SummaryTitle: "Claude Code Session Continuation",
		First:        "你能读取到 claude code session 91df602d 吗",
		Recent: []string{
			"chat mode 模型加载的速度能否提升",
			"确认",
			"下面的描述文字，有两个问题",
		},
	}
	info2 := info1
	info2.Recent = append(append([]string(nil), info1.Recent...), "可以", "继续")
	if agyTitleDigest(info1) != agyTitleDigest(info2) {
		t.Fatalf("trivial follow-ups changed agyTitleDigest")
	}
	if fb := agyFallbackTitle(info2); fb != "下面的描述文字，有两个问题" {
		t.Fatalf("agyFallbackTitle = %q, want latest meaningful prompt", fb)
	}

	imgOnly := agyPaneTitleInfo{
		Pane:         "w28:p1",
		SID:          "6490c7d4-0626-4ed8-a158-da789586d5b1",
		SummaryTitle: "Image Diagnosis",
		Recent:       []string{"/home/u/.cache/corral/uploads/a.png", "继续"},
	}
	if fb := agyFallbackTitle(imgOnly); fb != "Image Diagnosis" {
		t.Fatalf("img-only agyFallbackTitle = %q, want Image Diagnosis", fb)
	}
}

func TestCleanAgyLLMTitle(t *testing.T) {
	cases := map[string]string{
		"「侧边栏小标题优化」":          "侧边栏小标题优化",
		"标题：Corral 聊天菜单修复。":    "Corral 聊天菜单修复",
		"\n\"生产 k8s 智能体上限\"\n": "生产 k8s 智能体上限",
	}
	for in, want := range cases {
		if got := cleanAgyLLMTitle(in); got != want {
			t.Errorf("cleanAgyLLMTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgyTitlerFallbackAndLLMCache(t *testing.T) {
	h := newHarness(t)
	alice := h.admin()
	me := alice.do("GET", "/api/me", nil)
	mustStatus(t, me, 200, "get me")
	var u struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(me.Body, &u); err != nil || u.ID <= 0 {
		t.Fatalf("parse /api/me: err=%v body=%s", err, me.Body)
	}

	var llmCalls atomic.Int32
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "「侧边栏小标题优化」"}},
			},
		})
	}))
	defer mockLLM.Close()
	h.srv.translateClient = mockLLM.Client()

	var scanMu sync.Mutex
	scanData := map[string]agyPaneTitleInfo{
		"w28:p1": {
			Pane:         "w28:p1",
			SID:          "6490c7d4-0626-4ed8-a158-da789586d5b1",
			SummaryTitle: "Claude Code Session Continuation",
			First:        "你能读取到 claude code session 吗",
			Recent:       []string{"下面的描述文字，有两个问题", "可以"},
		},
	}
	getScan := func(context.Context) (map[string]agyPaneTitleInfo, error) {
		scanMu.Lock()
		defer scanMu.Unlock()
		cp := make(map[string]agyPaneTitleInfo, len(scanData))
		for k, v := range scanData {
			cp[k] = v
		}
		return cp, nil
	}

	makeSum := func() herdrSummary {
		return herdrSummary{
			Workspaces: []herdrWorkspace{{
				ID: "w28",
				Agents: []herdrAgent{{
					PaneID:    "w28:p1",
					Agent:     "agy",
					Title:     "agy",
					BareTitle: true,
				}},
			}},
		}
	}

	// 1. With translate LLM disabled: falls back immediately to latest meaningful prompt, 0 LLM calls.
	poked := make(chan struct{}, 8)
	at1 := newAgyTitler(
		getScan,
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, bool) {
			return h.srv.resolveAgyTitleSync(ctx, u.ID, info)
		},
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, error) {
			return h.srv.generateAgyTitleLLM(ctx, u.ID, info)
		},
	)
	at1.setPoke(func() {
		select {
		case poked <- struct{}{}:
		default:
		}
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() {
		at1.run(ctx1)
		close(done1)
	}()
	sum := makeSum()
	at1.track(&sum)
	select {
	case <-poked:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fallback poke")
	}
	at1.decorate(&sum)
	if got := sum.Workspaces[0].Agents[0].ThreadTitle; got != "下面的描述文字，有两个问题" {
		t.Fatalf("disabled LLM fallback title = %q", got)
	}
	if llmCalls.Load() != 0 {
		t.Fatalf("expected 0 LLM calls when disabled, got %d", llmCalls.Load())
	}
	cancel1()
	<-done1

	// 2. Enable translate LLM: generates concise Chinese title via LLM and caches in SQLite.
	r := alice.do("PUT", "/api/me/translate", map[string]any{
		"enabled":     true,
		"target_lang": "zh-CN",
		"base_url":    mockLLM.URL + "/v1",
		"model":       "deepseek-flash",
		"api_key":     "sk-test-key",
	})
	mustStatus(t, r, 200, "enable translate")

	poked2 := make(chan struct{}, 8)
	at2 := newAgyTitler(
		getScan,
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, bool) {
			return h.srv.resolveAgyTitleSync(ctx, u.ID, info)
		},
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, error) {
			return h.srv.generateAgyTitleLLM(ctx, u.ID, info)
		},
	)
	at2.setPoke(func() {
		select {
		case poked2 <- struct{}{}:
		default:
		}
	})
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		at2.run(ctx2)
		close(done2)
	}()
	sum2 := makeSum()
	at2.track(&sum2)

	deadline := time.After(4 * time.Second)
	for {
		s := makeSum()
		at2.decorate(&s)
		if s.Workspaces[0].Agents[0].ThreadTitle == "侧边栏小标题优化" {
			break
		}
		select {
		case <-poked2:
		case <-deadline:
			t.Fatalf("timed out waiting for LLM title, got %q", s.Workspaces[0].Agents[0].ThreadTitle)
		}
	}
	if llmCalls.Load() != 1 {
		t.Fatalf("expected 1 LLM call, got %d", llmCalls.Load())
	}
	cancel2()
	<-done2

	// 3. Simulate Corral restart (brand new agyTitler): hits SQLite cache immediately with 0 extra LLM calls.
	poked3 := make(chan struct{}, 8)
	at3 := newAgyTitler(
		getScan,
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, bool) {
			return h.srv.resolveAgyTitleSync(ctx, u.ID, info)
		},
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, error) {
			return h.srv.generateAgyTitleLLM(ctx, u.ID, info)
		},
	)
	at3.setPoke(func() {
		select {
		case poked3 <- struct{}{}:
		default:
		}
	})
	ctx3, cancel3 := context.WithCancel(context.Background())
	done3 := make(chan struct{})
	go func() {
		at3.run(ctx3)
		close(done3)
	}()
	sum3 := makeSum()
	at3.track(&sum3)
	select {
	case <-poked3:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cached restart poke")
	}
	at3.decorate(&sum3)
	if got := sum3.Workspaces[0].Agents[0].ThreadTitle; got != "侧边栏小标题优化" {
		t.Fatalf("restarted cached title = %q, want 侧边栏小标题优化", got)
	}
	if llmCalls.Load() != 1 {
		t.Fatalf("expected 0 extra LLM calls after restart cache hit, total=%d", llmCalls.Load())
	}
	cancel3()
	<-done3
}

// TestAgyTitlerSeedBySIDOnRestart verifies that when Herdr reports an agy
// session UUID on the very first snapshot after Corral restarts, track()
// immediately seeds ThreadTitle from SQLite before any SSH scan completes.
func TestAgyTitlerSeedBySIDOnRestart(t *testing.T) {
	at := newAgyTitler(
		func(ctx context.Context) (map[string]agyPaneTitleInfo, error) {
			return nil, context.DeadlineExceeded
		},
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, bool) {
			return agyFallbackTitle(info), false
		},
		nil,
	)
	at.seedBySID = func(_ context.Context, sids []string) map[string]string {
		out := map[string]string{}
		for _, sid := range sids {
			if sid == "7741b9ca-3d5d-448a-8418-1a8464f064bf" {
				out[sid] = "执行任务4 ModelHub 102"
			}
		}
		return out
	}
	sum := herdrSummary{
		Workspaces: []herdrWorkspace{{
			ID: "w33",
			Agents: []herdrAgent{{
				PaneID:    "w33:p5",
				Agent:     "agy",
				Label:     "ag-hub",
				BareTitle: true,
				Session:   "7741b9ca-3d5d-448a-8418-1a8464f064bf",
			}},
		}},
	}
	at.track(&sum)
	at.decorate(&sum)
	if got := sum.Workspaces[0].Agents[0].ThreadTitle; got != "执行任务4 ModelHub 102" {
		t.Fatalf("expected immediate 0ms SQLite seed on restart, got %q", got)
	}
}
