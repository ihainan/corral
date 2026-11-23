package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

func cxScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/codex/" + name + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Codex 0.164's picker, with paths removed: its two steps.
func TestParseCodexPicker(t *testing.T) {
	latest := parseCodexPicker(cxScreen(t, "model-0167"))
	if !latest.ok || !latest.below || latest.options[0].Label != "GPT-6.1-Sol" {
		t.Fatalf("0.167 viewport: %+v", latest)
	}
	m := parseCodexPicker(cxScreen(t, "model"))
	c, _ := m.cursor()
	if !m.ok || m.effort || len(m.options) != 8 || c.N != 3 || m.options[0].Label != "GPT-6.1-Sol" || !strings.HasPrefix(m.options[0].Desc, "Latest") {
		t.Fatalf("models: %+v", m)
	}
	e := parseCodexPicker(cxScreen(t, "effort"))
	c, _ = e.cursor()
	if !e.ok || !e.effort || e.of != "GPT-6-Sol" || len(e.options) != 5 || c.N != 2 || codexEffortName(c.Label) != "Medium" {
		t.Fatalf("efforts: %+v", e)
	}
	// (In a narrow pane a description wraps: its line under it, indented.)
	w := parseCodexPicker(cxScreen(t, "effort-at07"))
	c, _ = w.cursor()
	if !w.ok || !w.effort || len(w.options) != 5 || c.N != 2 || !strings.HasSuffix(c.Desc, "everyday tasks") {
		t.Fatalf("efforts, wrapped: %+v", w)
	}
	if p := parseCodexPicker(cxScreen(t, "modelset")); p.ok {
		t.Fatalf("no picker: %+v", p)
	}
	if err := codexIdleInput(cxScreen(t, "modelset")); err != nil {
		t.Fatalf("idle: %v", err)
	}
	if err := codexIdleInput(strings.Replace(cxScreen(t, "modelset"), "› Ask Codex to do anything", "› half a message", 1)); err != errDraft {
		t.Fatalf("draft: %v", err)
	}
}

// A stand-in Codex for the flow: its picker drawn as a real one draws it,
// the keys pressed noted. Enter on the effort step (which saves the default)
// and digits are never to be pressed.
type fakeCodexPicker struct {
	t       *testing.T
	step    int // 0 at rest, 1 models, 2 efforts
	at      int
	of      string
	set     int
	setTo   string // (what its "Model changed to" says: the model chosen, or another — a Codex that took another)
	pressed []string
	drawn   int // (screens drawn: a digest's)
	cut     int // (a key typed in its terminal before the press of that number: the screen changed)
	presses int
	lag     bool   // (what a press brings drawn a read late)
	shown   string // (the screen the last press left, while lagging)
	late    bool
	wrap    bool // (its "Model changed…" wrapped, as in a narrow pane)
	queue   bool // (keys taken in a read late, the screen as it was meanwhile — its digest too)
	pending [][]string
	page    int  // models visible at once, zero: the whole list
	stuck   bool // a movement ignored by the terminal
}

var fakeModels = []string{"GPT-6.1-Sol", "GPT-6-Astra", "GPT-6-Sol", "GPT-6-Luna"}
var fakeEfforts = []string{"Low", "Medium (default)", "High", "Extra high"}

func (f *fakeCodexPicker) screen() string {
	var b strings.Builder
	b.WriteString("  >_ OpenAI Codex (v0.164.0)\n\n")
	for i := 0; i < f.set; i++ {
		if f.wrap {
			b.WriteString("• Model changed to " + f.setTo + "\n  for this session only\n")
		} else {
			b.WriteString("• Model changed to " + f.setTo + " for this session only\n")
		}
	}
	switch f.step {
	case 0:
		b.WriteString("\n› Ask Codex to do anything\n\n  gpt-mock default · ~/p\n")
	default:
		list := fakeModels
		if f.step == 1 {
			b.WriteString("  Select Model and Effort\n\n")
		} else {
			list = fakeEfforts
			b.WriteString("  Select Reasoning Level for " + f.of + "\n\n")
		}
		first, end := 0, len(list)
		if f.step == 1 && f.page > 0 {
			first = f.at / f.page * f.page
			end = min(first+f.page, len(list))
			if first > 0 {
				b.WriteString("↑\n")
			}
		}
		for i := first; i < end; i++ {
			l := list[i]
			mark := " "
			if i == f.at {
				mark = "›"
			}
			fmt.Fprintf(&b, "%s %d. %-18s description %d\n", mark, i+1, l, i)
		}
		if end < len(list) {
			b.WriteString("↓\n")
		}
		if f.step == 1 {
			b.WriteString("\n  enter select · esc back\n")
		} else {
			b.WriteString("\n  enter default · s session · esc back\n")
		}
	}
	return b.String()
}

func (f *fakeCodexPicker) look() (string, string, error) {
	if f.queue && len(f.pending) > 0 {
		if f.late {
			f.late = false // (not yet: this read sees the screen before them)
			return f.screen(), fmt.Sprint(f.drawn), nil
		}
		for _, k := range f.pending {
			f.run(k, "", 0)
		}
		f.pending = nil
	}
	if f.late {
		f.late = false
		return f.shown, "stale", nil // (the screen before: its digest no longer the screen's)
	}
	return f.screen(), fmt.Sprint(f.drawn), nil
}

// press: as the host does, only on the screen read last (its digest).
func (f *fakeCodexPicker) press(keys []string, text, digest string) error {
	if keys == nil && text == "" {
		f.t.Fatalf("an empty paste, and Enter (review498)")
	}
	f.presses++
	if f.lag {
		f.shown, f.late = f.screen(), true
	}
	if f.presses == f.cut {
		f.drawn++ // (the person pressed a key in the terminal: another screen)
	}
	if digest != fmt.Sprint(f.drawn) {
		return &sshx.ExitError{Status: 10}
	}
	if f.queue && text == "" {
		f.pending, f.late = append(f.pending, keys), true
		return nil
	}
	f.run(keys, text, 0)
	return nil
}

func (f *fakeCodexPicker) run(keys []string, text string, read int) (string, error) {
	if text == "/model" {
		f.step, f.at = 1, 0
	}
	for _, k := range keys {
		f.pressed = append(f.pressed, fmt.Sprint(f.step)+":"+k)
		if _, ok := modelKeys[k]; !ok {
			f.t.Fatalf("a key not of modelKeys: %q", k)
		}
		switch {
		case k == "down":
			if !f.stuck {
				f.at++
			}
		case k == "up":
			if !f.stuck {
				f.at--
			}
		case k == "esc":
			f.step = 0
		case k == "yes" && f.step == 1:
			f.of, f.step, f.at = fakeModels[f.at], 2, 1
		case k == "yes" && f.step == 2:
			f.t.Fatalf("Enter on the effort step: the default saved")
		case k == "this" && f.step == 2:
			if f.setTo == "" {
				f.setTo = strings.ToLower(f.of) + " " + codexEffortID(codexEffortName(fakeEfforts[f.at]))
			}
			f.step, f.set = 0, f.set+1
		}
	}
	if text != "" || len(keys) > 0 {
		f.drawn++
	}
	return f.screen(), nil
}

func TestCodexModelFlowScrolled(t *testing.T) {
	codexSettleGap = 0
	f := &fakeCodexPicker{t: t, page: 2}
	io := codexIO{f.look, f.press}
	options, err := codexModelFlow(io, io, "", "")
	if err != nil || len(options) != len(fakeModels) || f.step != 0 {
		t.Fatalf("complete paged list: %+v, %v, step %d", options, err, f.step)
	}
	f = &fakeCodexPicker{t: t, page: 2}
	io = codexIO{f.look, f.press}
	if _, err := codexModelFlow(io, io, "GPT-6.1-Sol", "High"); err != nil || f.set != 1 {
		t.Fatalf("switch back to an earlier page: %v, %+v", err, f)
	}
	f = &fakeCodexPicker{t: t, page: 2, stuck: true}
	io = codexIO{f.look, f.press}
	if options, err := codexModelFlow(io, io, "", ""); err == nil || len(options) != 0 || f.step != 0 {
		t.Fatalf("ignored scroll returned a partial list: %+v, %v, step %d", options, err, f.step)
	}
}

func TestCodexModelFlow(t *testing.T) {
	codexSettleGap = 0
	flow := func(f *fakeCodexPicker, model, effort string) ([]modelOpt, error) {
		io := codexIO{f.look, f.press}
		return codexModelFlow(io, io, model, effort)
	}
	f := &fakeCodexPicker{t: t}
	opts, err := flow(f, "", "")
	if err != nil || len(opts) != 4 || opts[2].Label != "GPT-6-Sol" || f.step != 0 {
		t.Fatalf("list: %v %+v step %d", err, opts, f.step)
	}
	f = &fakeCodexPicker{t: t}
	opts, err = flow(f, "GPT-6-Sol", "High")
	if err != nil || len(opts) != 1 || f.set != 1 || f.of != "GPT-6-Sol" {
		t.Fatalf("switch: %v %+v %+v", err, opts, f)
	}
	if got := strings.Join(f.pressed, " "); got != "1:down 1:down 1:yes 2:down 2:this" {
		t.Fatalf("keys: %s", got)
	}
	f = &fakeCodexPicker{t: t}
	if _, err = flow(f, "GPT-9", "High"); err != errNoModel || f.step != 0 {
		t.Fatalf("no such model: %v, left at step %d", err, f.step)
	}
	f = &fakeCodexPicker{t: t}
	if _, err = flow(f, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Fatalf("no such effort: %v, left at step %d", err, f.step)
	}
	// (review493: a key typed in the terminal between a read and a press —
	// at each press in turn: that press not made, the picker left, never
	// Enter on the effort step; nothing said to be done.)
	for cut := 1; cut <= 5; cut++ {
		f = &fakeCodexPicker{t: t, cut: cut}
		_, err = flow(f, "GPT-6-Sol", "High")
		if !screenRefused(err) || f.step != 0 || f.set != 0 {
			t.Errorf("screen changed before press %d: %v, step %d, set %d (%v)", cut, err, f.step, f.set, f.pressed)
		}
	}
	// (review493: what it says it took is another model's: not done.)
	f = &fakeCodexPicker{t: t, setTo: "gpt-6-luna high"}
	if _, err = flow(f, "GPT-6-Sol", "High"); err == nil {
		t.Errorf("another model said taken: ok")
	}
	// (review498: Codex draws what a press brought a read late: read again.)
	f = &fakeCodexPicker{t: t, lag: true}
	if _, err = flow(f, "GPT-6-Sol", "High"); err != nil || f.set != 1 {
		t.Errorf("drawn late: %v, set %d (%v)", err, f.set, f.pressed)
	}
	// (review498: its "Model changed…" wrapped: done all the same.)
	f = &fakeCodexPicker{t: t, wrap: true}
	if _, err = flow(f, "GPT-6-Sol", "High"); err != nil {
		t.Errorf("wrapped: %v", err)
	}
	// (review498: left after a failure on the effort step — Escape only,
	// never Enter (the fake fails on Enter there, and on an empty paste).)
	f = &fakeCodexPicker{t: t}
	if _, err = flow(f, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Errorf("no such effort, left: %v step %d (%v)", err, f.step, f.pressed)
	}
	// (review501: leaving, the Escape taken in a read late: not pressed again.)
	f = &fakeCodexPicker{t: t, queue: true}
	if _, err = flow(f, "GPT-6-Sol", "Ultra"); err != errNoModel || f.step != 0 {
		t.Errorf("left, drawn late: %v step %d", err, f.step)
	}
	if n := strings.Count(strings.Join(f.pressed, " "), ":esc"); n != 1 {
		t.Errorf("left, drawn late: %d Escapes (%v)", n, f.pressed)
	}
	// (A picker left open that does not read as known (its footer there):
	// closed first, then on as at rest.)
	f = &fakeCodexPicker{t: t, step: 1}
	scr := f.screen()
	if !codexPickerOpen(strings.Replace(scr, "1. ", "1) ", 1)) || parseCodexPicker(strings.Replace(scr, "1. ", "1) ", 1)).ok {
		t.Fatalf("a picker not read as known, its footer there")
	}
}

func TestCodexModelSwitchCaseInsensitiveAndEarlyStop(t *testing.T) {
	codexSettleGap = 0
	// Case-insensitive match (e.g. slug/display_name casing differences) and
	// early stop on page 1 without scrolling through every subsequent page.
	f := &fakeCodexPicker{t: t, page: 2}
	io := codexIO{f.look, f.press}
	if _, err := codexModelFlow(io, io, "gpt-6.1-sol", "High"); err != nil || f.set != 1 {
		t.Fatalf("case-insensitive switch: err=%v set=%d pressed=%v", err, f.set, f.pressed)
	}
	if strings.Contains(strings.Join(f.pressed, " "), "1:down") {
		t.Fatalf("scrolled past page 1 when target was already on page 1: %v", f.pressed)
	}
}

func TestParseCodexModelsCache(t *testing.T) {
	now := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC)
	raw := []byte(`{
		"fetched_at": "2026-11-23T14:00:00Z",
		"client_version": "0.171.0",
		"models": [
			{"slug": "gpt-4o-audio", "display_name": "gpt-4o-audio", "description": "Routed via Sub2API", "visibility": "list", "priority": 50, "supported_in_api": true},
			{"slug": "hidden-model", "display_name": "Hidden", "description": "Should not appear", "visibility": "hide", "priority": 0, "supported_in_api": true},
			{"slug": "gpt-5.6", "display_name": "GPT-5.6 (Sol)", "description": "Latest workhorse", "visibility": "list", "priority": 1, "supported_in_api": true},
			{"slug": "gpt-5.6-dup", "display_name": "gpt-5.6 (sol)", "description": "Duplicate casing", "visibility": "list", "priority": 2, "supported_in_api": true},
			{"slug": "slug-only", "display_name": "", "description": "Fallback to slug", "visibility": "list", "priority": 5}
		]
	}`)
	opts, fetchedAt, ok := parseCodexModelsCache(raw, now)
	if !ok || len(opts) != 3 {
		t.Fatalf("got ok=%v %d models, want ok=true 3: %+v", ok, len(opts), opts)
	}
	if !fetchedAt.Equal(time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("fetchedAt = %v", fetchedAt)
	}
	if opts[0].Label != "GPT-5.6 (Sol)" || opts[0].Slug != "gpt-5.6" || opts[0].Desc != "Latest workhorse" {
		t.Fatalf("first model: %+v", opts[0])
	}
	if opts[1].Label != "slug-only" || opts[1].Slug != "slug-only" || opts[2].Label != "gpt-4o-audio" {
		t.Fatalf("ordering: %+v", opts)
	}

	// Missing fetched_at -> reject (fall back to TUI).
	if _, _, ok := parseCodexModelsCache([]byte(`{"models":[{"slug":"gpt-5.6","visibility":"list"}]}`), now); ok {
		t.Fatalf("missing fetched_at should be rejected")
	}
	// Stale fetched_at (>12h) -> reject.
	stale := []byte(`{"fetched_at":"2026-11-23T01:00:00Z","models":[{"slug":"gpt-5.6","visibility":"list"}]}`)
	if _, _, ok := parseCodexModelsCache(stale, now); ok {
		t.Fatalf("stale fetched_at (>12h) should be rejected")
	}
	// Far-future fetched_at (>5m) -> reject.
	future := []byte(`{"fetched_at":"2026-11-23T15:10:00Z","models":[{"slug":"gpt-5.6","visibility":"list"}]}`)
	if _, _, ok := parseCodexModelsCache(future, now); ok {
		t.Fatalf("future fetched_at should be rejected")
	}
	// Any model with supported_in_api=false -> reject fast path (fall back to TUI).
	unsupported := []byte(`{"fetched_at":"2026-11-23T14:30:00Z","models":[{"slug":"gpt-5.6","visibility":"list","supported_in_api":true},{"slug":"chatgpt-only","visibility":"list","supported_in_api":false}]}`)
	if _, _, ok := parseCodexModelsCache(unsupported, now); ok {
		t.Fatalf("supported_in_api=false should disqualify disk cache fast path")
	}
	if _, _, ok := parseCodexModelsCache([]byte(`{"fetched_at":"2026-11-23T14:30:00Z","models":[]}`), now); ok {
		t.Fatalf("empty models should return ok=false")
	}
	if _, _, ok := parseCodexModelsCache([]byte(`not json`), now); ok {
		t.Fatalf("invalid json should return ok=false")
	}
}

func TestCodexDiskCacheTimestampAndRejection(t *testing.T) {
	srv := &Server{}
	ctx := context.Background()
	const hostID int64 = 904001
	const via = "gw.example"
	const expiredHostID int64 = 904002
	key := codexModelStoreKey(hostID, via, "")
	expiredKey := codexModelStoreKey(expiredHostID, via, "")
	t.Cleanup(func() {
		codexModelsCacheMu.Lock()
		delete(codexModelsCache, key)
		delete(codexModelsCache, expiredKey)
		delete(codexRejectedDiskAt, key)
		delete(codexRejectedDiskAt, expiredKey)
		codexModelsCacheMu.Unlock()
	})

	// 1. Stored timestamp uses fetched_at, not time.Now(): an entry older than lineListFor is not served.
	oldFetched := time.Now().Add(-lineListFor - time.Minute).UTC().Truncate(time.Second)
	srv.noteCodexDiskModels(ctx, hostID, via, "", []modelOpt{{Label: "Old"}}, oldFetched)
	var outcome, reason string
	if srv.keptCodexModelsFor(httptest.NewRecorder(), ctx, hostID, via, "", &outcome, &reason) {
		t.Fatalf("expired fetched_at should not be served from keptCodexModelsFor")
	}

	// 2. Fresh disk cache is served; after forgetCodexModels (switch failure), fetched_at <= failure time is rejected,
	// while a strictly newer fetched_at (after failure time) is accepted.
	freshFetched := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	srv.noteCodexDiskModels(ctx, hostID, via, "", []modelOpt{{Label: "GPT-5.6 (Sol)", Slug: "gpt-5.6"}}, freshFetched)
	rec := httptest.NewRecorder()
	if !srv.keptCodexModelsFor(rec, ctx, hostID, via, "", &outcome, &reason) {
		t.Fatalf("expected fresh disk cache hit")
	}
	if isCodexDiskCacheRejected(hostID, via, "", freshFetched) {
		t.Fatalf("fresh disk cache should not be rejected before forgetCodexModels")
	}
	srv.forgetCodexModels(ctx, hostID, via, "")
	if !isCodexDiskCacheRejected(hostID, via, "", freshFetched) {
		t.Fatalf("old disk fetched_at (<= failure time) must be rejected after forgetCodexModels")
	}
	newerFetched := time.Now().Add(time.Second).UTC().Truncate(time.Second)
	if isCodexDiskCacheRejected(hostID, via, "", newerFetched) {
		t.Fatalf("disk fetched_at strictly after failure time should be accepted")
	}

	// 3. A subsequent live TUI read (noteCodexModels) must NOT clear the disk rejection timestamp:
	// the same stale on-disk file remains rejected if needed later.
	srv.noteCodexModels(ctx, hostID, via, "", []modelOpt{{Label: "GPT-5.6 (Sol)"}})
	if !isCodexDiskCacheRejected(hostID, via, "", freshFetched) {
		t.Fatalf("live TUI read must not clear disk rejection for older fetched_at")
	}

	// 4. Simulated restart (in-memory maps cleared, as if list was recovered from SQLite):
	// calling forgetCodexModels must still record rejection for older disk files.
	codexModelsCacheMu.Lock()
	delete(codexModelsCache, key)
	delete(codexRejectedDiskAt, key)
	codexModelsCacheMu.Unlock()
	srv.forgetCodexModels(ctx, hostID, via, "")
	if !isCodexDiskCacheRejected(hostID, via, "", freshFetched) {
		t.Fatalf("forgetCodexModels after restart (empty memory) must still reject older disk fetched_at")
	}

	// 5. Expired entries in both codexModelsCache and codexRejectedDiskAt are pruned on writes.
	staleTime := time.Now().Add(-codexDiskCacheMaxAge - time.Minute)
	codexModelsCacheMu.Lock()
	codexModelsCache[expiredKey] = codexModelCacheEntry{opts: []modelOpt{{Label: "Stale"}}, at: staleTime}
	codexRejectedDiskAt[expiredKey] = staleTime
	codexModelsCacheMu.Unlock()
	srv.noteCodexModels(ctx, hostID, via, "", []modelOpt{{Label: "Fresh"}})
	codexModelsCacheMu.Lock()
	_, cacheStillThere := codexModelsCache[expiredKey]
	_, rejStillThere := codexRejectedDiskAt[expiredKey]
	codexModelsCacheMu.Unlock()
	if cacheStillThere || rejStillThere {
		t.Fatalf("expected expired map entries to be pruned, cache=%v rejected=%v", cacheStillThere, rejStillThere)
	}
}

func TestCodexModelsCacheCommand(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	dir := t.TempDir()
	cache := `{"fetched_at":"` + now.Format(time.RFC3339) + `","client_version":"0.171.0","models":[{"slug":"gpt-5.6","display_name":"GPT-5.6 (Sol)","description":"Fast","visibility":"list","priority":0,"supported_in_api":true,"model_messages":{"instructions_template":"` + strings.Repeat("x", 20000) + `"}},{"slug":"secret","display_name":"Secret","visibility":"hide","priority":0,"supported_in_api":true}]}`
	if err := os.WriteFile(dir+"/models_cache.json", []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", codexModelsCacheCommand(0))
	c.Env = append(os.Environ(), "CODEX_HOME="+dir)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("codexModelsCacheCommand failed: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("now=")) {
		t.Fatalf("output does not start with the host's clock: %q", out[:min(len(out), 40)])
	}
	opts, fetchedAt, ok := parseCodexModelsCacheAt(out, time.Now())
	if !ok || len(opts) != 1 || opts[0].Label != "GPT-5.6 (Sol)" || opts[0].Slug != "gpt-5.6" || opts[0].Desc != "Fast" {
		t.Fatalf("parsed output: ok=%v %+v (raw %d bytes)", ok, opts, len(out))
	}
	if d := fetchedAt.Sub(now); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("fetchedAt = %v, want about %v", fetchedAt, now)
	}
	if _, err := exec.LookPath("python3"); err == nil && len(out) > 2048 {
		t.Fatalf("expected python3 extractor to strip instructions_template, got %d bytes", len(out))
	}

	// Preserve supported_in_api=false through the python3 extractor so Go rejects fast path.
	apiFalse := `{"fetched_at":"` + now.Format(time.RFC3339) + `","models":[{"slug":"gpt-5.6","visibility":"list","supported_in_api":false}]}`
	if err := os.WriteFile(dir+"/models_cache.json", []byte(apiFalse), 0o644); err != nil {
		t.Fatal(err)
	}
	c = exec.Command("sh", "-c", codexModelsCacheCommand(0))
	c.Env = append(os.Environ(), "CODEX_HOME="+dir)
	out, err = c.Output()
	if err != nil {
		t.Fatalf("codexModelsCacheCommand (supported_in_api=false) failed: %v", err)
	}
	_, body, found := bytes.Cut(out, []byte("\n"))
	var rawCheck map[string]any
	if !found || json.Unmarshal(body, &rawCheck) != nil {
		t.Fatalf("invalid json from extractor: %q", string(out))
	}
	if _, _, ok := parseCodexModelsCacheAt(out, time.Now()); ok {
		t.Fatalf("expected supported_in_api=false to be preserved and rejected")
	}

	// Oversized file (>4 MiB) must exit 3 before python3 parses it.
	if err := os.WriteFile(dir+"/models_cache.json", make([]byte, (4<<20)+10), 0o644); err != nil {
		t.Fatal(err)
	}
	c = exec.Command("sh", "-c", codexModelsCacheCommand(0))
	c.Env = append(os.Environ(), "CODEX_HOME="+dir)
	_, err = c.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("oversized cache: got err=%v, want exit 3", err)
	}

	// CODEX_HOME containing a newline in /proc/<pid>/environ must exit 3 (not guess the path).
	if _, statErr := os.Stat("/proc/self/environ"); statErr == nil {
		sleeper := exec.Command("sleep", "5")
		sleeper.Env = append(os.Environ(), "CODEX_HOME="+dir+"\ninjected")
		if err := sleeper.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = sleeper.Process.Kill()
			_ = sleeper.Wait()
		}()
		c = exec.Command("sh", "-c", codexModelsCacheCommand(int64(sleeper.Process.Pid)))
		_, err = c.Output()
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("newline in CODEX_HOME: got err=%v, want exit 3", err)
		}
	}
}

// A host's clock off the gateway's: fetched_at is aged by the host's clock and
// told in the gateway's, so a switch's failure (gateway time) keeps refusing
// the list read before it, and lets one read after it in (review E, 11-24).
func TestCodexModelsCacheHostClockSkew(t *testing.T) {
	gw := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	out := func(hostNow, fetched time.Time) []byte {
		return []byte("now=" + strconv.FormatInt(hostNow.Unix(), 10) + "\n" + `{"fetched_at":"` + fetched.Format(time.RFC3339) + `","models":[{"slug":"gpt-5.6","visibility":"list","supported_in_api":true}]}`)
	}
	for _, skew := range []time.Duration{-30 * time.Minute, 0, 30 * time.Minute} {
		host := gw.Add(skew)
		// Read 10 minutes ago by the host's clock: 10 minutes ago by the gateway's.
		_, at, ok := parseCodexModelsCacheAt(out(host, host.Add(-10*time.Minute)), gw)
		if !ok || !at.Equal(gw.Add(-10*time.Minute)) {
			t.Fatalf("skew %v: ok=%v at=%v, want %v", skew, ok, at, gw.Add(-10*time.Minute))
		}
		// 11 hours old by the host's clock: still in its 12; 13: not, whatever the gateway's clock says.
		if _, _, ok := parseCodexModelsCacheAt(out(host, host.Add(-11*time.Hour)), gw); !ok {
			t.Fatalf("skew %v: an 11-hour-old cache refused", skew)
		}
		if _, _, ok := parseCodexModelsCacheAt(out(host, host.Add(-13*time.Hour)), gw); ok {
			t.Fatalf("skew %v: a 13-hour-old cache taken", skew)
		}
	}
	// No clock line: the gateway's clock for both, as before.
	raw := []byte(`{"fetched_at":"` + gw.Add(-time.Minute).Format(time.RFC3339) + `","models":[{"slug":"a","visibility":"list"}]}`)
	if _, at, ok := parseCodexModelsCacheAt(raw, gw); !ok || !at.Equal(gw.Add(-time.Minute)) {
		t.Fatalf("no clock line: ok=%v at=%v", ok, at)
	}
	// A clock line that is not one: refused.
	for _, bad := range []string{"now=x\n{}", "now=123", "now=-5\n{}"} {
		if _, _, ok := parseCodexModelsCacheAt([]byte(bad), gw); ok {
			t.Fatalf("%q taken", bad)
		}
	}
}
