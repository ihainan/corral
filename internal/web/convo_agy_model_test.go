package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

const sampleAgyModelsTSV = `gemini-4-neon-high	Gemini 4 Neon (High)
gemini-4-neon-xhigh	Gemini 4 Neon (XHigh)
gemini-4-neon-medium	Gemini 4 Neon (Medium)
gemini-4-neon-low	Gemini 4 Neon (Low)
gemini-4-argon-medium	Gemini 4 Argon (Medium)
gemini-4-argon-high	Gemini 4 Argon (High)
gemini-4-argon-low	Gemini 4 Argon (Low)
gemini-pro-orchestrator-neon	Orchestrator (Neon)
gemini-pro-orchestrator-g1-pro	Orchestrator (Argon)
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
gemini-3.8-flash-medium	Gemini 3.8 Flash (Medium)
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
claude-opus-5-5-low	Claude Opus 5.5 (Low)
claude-opus-5-5-medium	Claude Opus 5.5 (Medium)
claude-opus-5-5-high	Claude Opus 5.5 (High)
gpt-oss-120b-medium	GPT-OSS 120B (Medium)
`

func TestParseAgyModelsAndResolve(t *testing.T) {
	groups := parseAgyModels(sampleAgyModelsTSV)
	if len(groups) != 7 {
		t.Fatalf("groups = %d, want 7", len(groups))
	}
	opts := agyModelOpts(groups, "Gemini 4 Neon")
	if len(opts) != 7 {
		t.Fatalf("opts = %d, want 7", len(opts))
	}
	if opts[0].Label != "Gemini 4 Neon" || !opts[0].Current || !slices.Equal(opts[0].Efforts, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("opts[0] = %+v", opts[0])
	}
	if opts[1].Label != "Gemini 4 Argon" || !slices.Equal(opts[1].Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("opts[1] = %+v", opts[1])
	}
	if opts[2].Label != "Orchestrator (Neon)" || len(opts[2].Efforts) != 0 {
		t.Fatalf("opts[2] = %+v", opts[2])
	}
	if opts[6].Label != "GPT-OSS 120B (Medium)" || len(opts[6].Efforts) != 0 {
		t.Fatalf("opts[6] = %+v", opts[6])
	}

	// Resolve group label + effort.
	v, disp, ok := resolveAgyModelID(groups, "Claude Opus 5.5", "medium", "high")
	if !ok || v.id != "claude-opus-5-5-medium" || v.effort != "medium" || disp != "Claude Opus 5.5" {
		t.Fatalf("resolve opus medium: ok=%v v=%+v disp=%q", ok, v, disp)
	}
	// Unsupported effort for model rejected.
	if _, _, ok := resolveAgyModelID(groups, "Claude Opus 5.5", "xhigh", "high"); ok {
		t.Fatal("expected unsupported xhigh on Claude Opus 5.5 to fail")
	}
	// No effort passed: falls back to current effort if supported.
	v, _, ok = resolveAgyModelID(groups, "Gemini 3.8 Flash", "", "low")
	if !ok || v.id != "gemini-3.8-flash-low" {
		t.Fatalf("resolve flash fallback low: ok=%v v=%+v", ok, v)
	}
	// Single-variant model resolves directly.
	v, _, ok = resolveAgyModelID(groups, "Orchestrator (Neon)", "", "high")
	if !ok || v.id != "gemini-pro-orchestrator-neon" || v.effort != "" {
		t.Fatalf("resolve orchestrator: ok=%v v=%+v", ok, v)
	}
	v, _, ok = resolveAgyModelID(groups, "GPT-OSS 120B (Medium)", "", "")
	if !ok || v.id != "gpt-oss-120b-medium" || v.effort != "medium" {
		t.Fatalf("resolve gpt-oss: ok=%v v=%+v", ok, v)
	}
}

func TestAgyStatusModelEffortAndConfirmations(t *testing.T) {
	rule := strings.Repeat("─", 80)
	cases := []struct {
		name       string
		screen     string
		wantModel  string
		wantEffort string
	}{
		{
			name: "auto mode idle",
			screen: strings.Join([]string{
				rule, "> ", rule,
				"? for shortcuts                                         auto · Gemini 4 Neon · high",
			}, "\n"),
			wantModel: "Gemini 4 Neon", wantEffort: "high",
		},
		{
			name: "background task strip 3 rules",
			screen: strings.Join([]string{
				rule, "> ", rule,
				"  ● [22:13:40] test running",
				rule,
				"esc to cancel                        auto · Gemini 4 Neon · xhigh · 1 task(s) · /tasks",
			}, "\n"),
			wantModel: "Gemini 4 Neon", wantEffort: "xhigh",
		},
		{
			name: "default mode without mode prefix",
			screen: strings.Join([]string{
				rule, "> ", rule,
				"? for shortcuts                                         Gemini 3.8 Flash · medium",
			}, "\n"),
			wantModel: "Gemini 3.8 Flash", wantEffort: "medium",
		},
		{
			name: "single variant orchestrator without effort",
			screen: strings.Join([]string{
				rule, "> ", rule,
				"? for shortcuts                                         auto · Orchestrator (Neon)",
			}, "\n"),
			wantModel: "Orchestrator (Neon)", wantEffort: "",
		},
	}
	for _, tc := range cases {
		m, e := agyStatusModelEffort(tc.screen)
		if m != tc.wantModel || e != tc.wantEffort {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, m, e, tc.wantModel, tc.wantEffort)
		}
	}

	confirmScreen := "> /model\n⎿  Model set to Gemini 3.8 Flash (Low)\n> /effort\n⎿  Effort set to medium\n"
	if got := agyModelSetResult(confirmScreen); got != "Gemini 3.8 Flash (Low)" {
		t.Fatalf("agyModelSetResult = %q", got)
	}
	if setTo, unsup := agyEffortSetResult(confirmScreen); setTo != "medium" || unsup != "" {
		t.Fatalf("agyEffortSetResult = (%q, %q)", setTo, unsup)
	}
	unsupScreen := "> /effort\n⎿  gemini-3.8-flash has no \"xhigh\" effort (available: low, medium, high)\n"
	if setTo, unsup := agyEffortSetResult(unsupScreen); setTo != "" || !strings.Contains(unsup, `has no "xhigh" effort`) {
		t.Fatalf("agyEffortSetResult unsup = (%q, %q)", setTo, unsup)
	}
}

func TestAgyModelShellScriptsParse(t *testing.T) {
	scripts := []string{
		agyModelsCommand,
		agySettingsReadCommand,
		agySettingsWriteCommand([]byte(`{"model":"a"}`), []byte(`{"model":"b"}`)),
	}
	for i, sc := range scripts {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(sc)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script %d sh -n failed: %v\n%s", i, err, out)
		}
	}
}

func TestKeptAgyAndCodexModelsCache(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := &Server{store: st}
	ctx := context.Background()

	// 1. AGY models persisted to SQLite and served immediately by keptAgyModels without SSH.
	const agyHost int64 = 9901
	t.Cleanup(func() {
		agyModelsCacheMu.Lock()
		delete(agyModelsCache, agyHost)
		agyModelsCacheMu.Unlock()
		agyModelFronts.Delete(modelFrontKey("s-agy", "w1:p1", 10, 20, "sid-agy"))
	})
	groups := parseAgyModels("gemini-4-neon-low\tGemini 4 Neon (Low)\ngemini-4-neon-high\tGemini 4 Neon (High)\ngemini-pro-orchestrator-neon\tOrchestrator (Neon)\n")
	raw, err := marshalAgyGroups(groups)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutModelList(ctx, agyModelStoreKey(agyHost), time.Now().Unix(), raw); err != nil {
		t.Fatal(err)
	}
	noteAgyModelFront(modelFrontKey("s-agy", "w1:p1", 10, 20, "sid-agy"), time.Now())
	sess := &term.Session{ID: "s-agy", HostID: agyHost}
	rec := httptest.NewRecorder()
	var outcome, reason string
	if !srv.keptAgyModels(rec, ctx, sess, "w1:p1", 10, 20, "sid-agy", &outcome, &reason) {
		t.Fatalf("expected keptAgyModels hit from SQLite store")
	}
	if outcome != "kept" || reason != "list_kept" {
		t.Fatalf("outcome=%q reason=%q", outcome, reason)
	}
	var agyResp struct {
		Kept   bool       `json:"kept"`
		Models []modelOpt `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &agyResp); err != nil || !agyResp.Kept || len(agyResp.Models) != 2 {
		t.Fatalf("unexpected agy kept response: %v %s", err, rec.Body.String())
	}

	// 2. Codex models persisted to SQLite, served without SSH, and cleared on forgetCodexModels.
	const cxHost int64 = 9902
	t.Cleanup(func() {
		srv.forgetCodexModels(ctx, cxHost, "", "")
		codexModelFronts.Delete(modelFrontKey("s-cx", "w2:p1", 30, 40, "sid-cx"))
	})
	srv.noteCodexModels(ctx, cxHost, "", "", []modelOpt{
		{Label: "gpt-5.4", Desc: "Frontier", Current: true},
		{Label: "gpt-5.4-mini", Desc: "Fast"},
	})
	// Clear in-memory cache to verify SQLite fallback hydration too.
	codexModelsCacheMu.Lock()
	delete(codexModelsCache, codexModelStoreKey(cxHost, "", ""))
	codexModelsCacheMu.Unlock()

	noteCodexModelFront(modelFrontKey("s-cx", "w2:p1", 30, 40, "sid-cx"), codexModelFront{hostID: cxHost, at: time.Now()})
	cxSess := &term.Session{ID: "s-cx", HostID: cxHost}
	rec2 := httptest.NewRecorder()
	outcome, reason = "", ""
	if !srv.keptCodexModels(rec2, ctx, cxSess, "w2:p1", 30, 40, "sid-cx", &outcome, &reason) {
		t.Fatalf("expected keptCodexModels hit from SQLite store")
	}
	var cxResp struct {
		Kept   bool       `json:"kept"`
		Models []modelOpt `json:"models"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &cxResp); err != nil || !cxResp.Kept || len(cxResp.Models) != 2 || cxResp.Models[0].Current {
		t.Fatalf("unexpected codex kept response: %v %s", err, rec2.Body.String())
	}
	srv.forgetCodexModels(ctx, cxHost, "", "")
	rec3 := httptest.NewRecorder()
	if srv.keptCodexModels(rec3, ctx, cxSess, "w2:p1", 30, 40, "sid-cx", &outcome, &reason) {
		t.Fatalf("expected miss after forgetCodexModels")
	}
}
