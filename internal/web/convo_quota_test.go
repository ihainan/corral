package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQuotaOfConfig(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	const signedIn = `"oauthAccount":{"accountUuid":"acc-1"},`
	rec := func(account, five, week string) string {
		return `{` + signedIn + `"other":{"a":1},"cachedUsageUtilization":{"fetchedAtMs":1793937000000,"accountUuid":"` + account + `","utilization":{"five_hour":` + five + `,"seven_day":` + week + `,"seven_day_opus":null}}}`
	}
	const later = `{"utilization":62,"resets_at":"2026-11-08T14:59:59.676874+00:00","limit_dollars":null}`
	const passed = `{"utilization":99,"resets_at":"2026-11-06T03:00:00+00:00"}`

	q, in, _ := quotaOfConfig([]byte(rec("acc-1", later, later)), now)
	if !in || !q.Known || q.UpdatedAt != 1793937000 || q.Weekly == nil || q.Weekly.Used != 62 || q.Weekly.ResetsAt != 1794149999 || q.FiveHour == nil {
		t.Fatalf("%+v %v", q, in)
	}
	// (A window not begun is null: left out, the other said.)
	if q, _, _ := quotaOfConfig([]byte(rec("acc-1", "null", later)), now); !q.Known || q.FiveHour != nil || q.Weekly == nil {
		t.Errorf("no five hours: %+v", q)
	}
	// (Its reset passed: nothing of now.)
	if q, _, _ := quotaOfConfig([]byte(rec("acc-1", passed, later)), now); q.FiveHour != nil || q.Weekly == nil {
		t.Errorf("reset passed: %+v", q)
	}
	if q, in, _ := quotaOfConfig([]byte(rec("acc-1", passed, passed)), now); q.Known || !in {
		t.Errorf("both passed: %+v %v", q, in)
	}
	// (Another account's record: nothing of this one; still signed in.)
	if q, in, _ := quotaOfConfig([]byte(rec("acc-2", later, later)), now); q.Known || !in {
		t.Errorf("another's: %+v %v", q, in)
	}
	// (Not signed in with an account, an API key's say: no record, nothing to ask.)
	if q, in, _ := quotaOfConfig([]byte(`{"cachedUsageUtilization":{"accountUuid":"acc-1","utilization":{"seven_day":`+later+`}}}`), now); q.Known || in {
		t.Errorf("signed out: %+v %v", q, in)
	}
	if q, in, _ := quotaOfConfig([]byte(`{`+signedIn+`"x":1}`), now); q.Known || !in {
		t.Errorf("signed in, nothing kept: %+v %v", q, in)
	}
	// (Of a kind not looked for, or not read: nothing, and no error.)
	if _, _, ok := quotaOfConfig([]byte(`{"oauthAccount":{"accountUuid":"a"},"cachedUsageUtilization":{"fetched`), now); ok {
		t.Error("cut short: taken as read")
	}
	for _, bad := range []string{`not json`, ``, `[]`, rec("acc-1", `{"utilization":"62","resets_at":"x"}`, `{"utilization":140,"resets_at":"2026-11-08T14:59:59Z"}`)} {
		if q, _, _ := quotaOfConfig([]byte(bad), now); q.Known {
			t.Errorf("%q: %+v", bad, q)
		}
	}
}

// However many look, an account on a host has it asked at most once in quotaEvery.
func TestQuotaRunsAtMostSoOften(t *testing.T) {
	var r quotaRuns
	at := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	n := 0
	for i := 0; i < 24*60; i++ { // (a look a minute for a day)
		if r.due("a", at.Add(time.Duration(i)*time.Minute)) {
			n++
		}
	}
	if n != 96 {
		t.Errorf("%d in a day", n)
	}
	if !r.due("b", at) {
		t.Error("another account is its own")
	}
}

// Run on a shell as the host's, on a config as Claude Code writes it: what is printed
// is the account's id and the quota, read as they are; nothing else of the file.
func TestQuotaRead(t *testing.T) {
	home := t.TempDir()
	run := func() (string, error) {
		c := exec.Command("sh", "-c", quotaReadCommand())
		c.Env = append(os.Environ(), "HOME="+home)
		out, err := c.Output()
		return string(out), err
	}
	f := filepath.Join(home, ".claude.json")
	if out, err := run(); err != nil || strings.TrimSpace(out) != "{}" {
		t.Fatalf("no file: %q %v", out, err)
	}
	later := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339Nano)
	cfg := map[string]any{
		"numStartups":   3,
		"mcpServers":    map[string]any{"x": map[string]any{"env": map[string]any{"TOKEN": "server-key-kept-here"}}},
		"primaryApiKey": "sk-kept-here",
		"oauthAccount":  map[string]any{"accountUuid": "acc-1", "emailAddress": "someone@example.com", "organizationUuid": "org-1"},
		"cachedUsageUtilization": map[string]any{"fetchedAtMs": 1793937000000, "accountUuid": "acc-1", "utilization": map[string]any{
			"five_hour": nil, "seven_day": map[string]any{"utilization": 62, "resets_at": later}, "extra_usage": map[string]any{"is_enabled": false}}},
		"zLast": map[string]any{"utilization": 1},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(f, b, 0o600)
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"server-key-kept-here", "sk-kept-here", "someone@example.com", "org-1", "numStartups"} {
		if strings.Contains(out, kept) {
			t.Errorf("%q left the host: %s", kept, out)
		}
	}
	q, in, ok := quotaOfConfig([]byte(out), time.Now())
	if !ok || !in || !q.Known || q.Weekly == nil || q.Weekly.Used != 62 || q.FiveHour != nil || q.UpdatedAt != 1793937000 {
		t.Errorf("%q: %+v %v %v", out, q, in, ok)
	}
	// (As the last key of the file too: no comma after its block.)
	b, _ = json.MarshalIndent(map[string]any{"a": 1, "cachedUsageUtilization": cfg["cachedUsageUtilization"], "oauthAccount": map[string]any{"accountUuid": "acc-1"}}, "", "  ")
	os.WriteFile(f, b, 0o600)
	out, _ = run()
	if q, in, ok := quotaOfConfig([]byte(out), time.Now()); !ok || !in || !q.Known {
		t.Errorf("last key: %q", out)
	}
	// (Signed in, nothing kept; neither.)
	os.WriteFile(f, []byte("{\n  \"oauthAccount\": {\n    \"accountUuid\": \"a\"\n  }\n}\n"), 0o600)
	out, _ = run()
	if _, in, ok := quotaOfConfig([]byte(out), time.Now()); !ok || !in {
		t.Errorf("signed in: %q", out)
	}
	os.WriteFile(f, []byte("{\n  \"x\": 1\n}\n"), 0o600)
	out, _ = run()
	if _, in, ok := quotaOfConfig([]byte(out), time.Now()); !ok || in {
		t.Errorf("neither: %q", out)
	}
	// (Cut short in its block: an error, not "none". All on one line: nothing found.)
	os.WriteFile(f, []byte("{\n  \"cachedUsageUtilization\": {\n    \"fetchedAtMs\": 1"), 0o600)
	if _, err := run(); err == nil {
		t.Error("cut short: no error")
	}
	os.WriteFile(f, []byte(`{"oauthAccount":{"accountUuid":"a"},"cachedUsageUtilization":{}}`), 0o600)
	out, _ = run()
	if _, in, ok := quotaOfConfig([]byte(out), time.Now()); !ok || in {
		t.Errorf("one line: %q", out)
	}
	// (A link in its place: no config of this account's.)
	os.Remove(f)
	target := filepath.Join(t.TempDir(), "other")
	os.WriteFile(target, []byte("{\n  \"oauthAccount\": {\n    \"accountUuid\": \"a\"\n  }\n}\n"), 0o600)
	os.Symlink(target, f)
	if out, _ := run(); strings.TrimSpace(out) != "{}" {
		t.Errorf("a link: %q", out)
	}
}

// The limit on the host: a second run within it does not start Claude Code, whoever
// sets it going; and with no nice there, it starts all the same.
func TestQuotaRefreshOnTheHost(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	seen := filepath.Join(home, "asked")
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+seen+"\n"), 0o755)
	run := func() {
		c := exec.Command("sh", "-c", quotaRefreshCommand())
		c.Env = append(os.Environ(), "HOME="+home)
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
	}
	run()
	run()
	b, _ := os.ReadFile(seen)
	if got := strings.Count(string(b), "\n"); got != 1 || !strings.HasPrefix(string(b), "-p /usage --output-format json --tools  --strict-mcp-config --setting-sources  --no-session-persistence") {
		t.Fatalf("%q", b)
	}
	// (The mark old: again.)
	old := time.Now().Add(-quotaEvery - time.Minute)
	os.Chtimes(filepath.Join(home, ".cache", "corral", "usage-asked"), old, old)
	run()
	b, _ = os.ReadFile(seen)
	if strings.Count(string(b), "\n") != 2 {
		t.Errorf("after the limit: %q", b)
	}
}

func TestQuotaOfAgyUsage(t *testing.T) {
	raw := []byte(`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[
		{"name":"Gemini Models","buckets":[
			{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.4768,"reset_time":"2026-11-26T12:20:38Z"},
			{"id":"gemini-5h","window":"5h","remaining_fraction":0.8371,"reset_time":"2026-11-21T05:24:40Z"}
		]},
		{"name":"Claude and GPT models","buckets":[
			{"id":"3p-weekly","window":"weekly","remaining_fraction":1,"reset_time":"2026-11-28T01:04:04Z"},
			{"id":"3p-5h","window":"5h","remaining_fraction":1,"reset_time":"2026-11-21T06:04:04Z"}
		]},
		{"name":"Gemini 4 Argon","buckets":[
			{"id":"gemini-argon-wtu-weekly","window":"weekly","remaining_fraction":1,"reset_time":"2026-11-28T01:04:04Z"}
		]}
	]}}}`)
	now := time.Date(2026, 11, 21, 1, 5, 0, 0, time.UTC)
	q, ok := quotaOfAgyUsage(raw, now)
	if !ok || !q.Known || len(q.Groups) != 3 {
		t.Fatalf("got ok=%v q=%+v", ok, q)
	}
	if q.Groups[0].Name != "Gemini Models" || q.Groups[0].FiveHour == nil || q.Groups[0].Weekly == nil {
		t.Fatalf("group 0 = %+v", q.Groups[0])
	}
	if q.Groups[0].FiveHour.Used < 16 || q.Groups[0].FiveHour.Used > 16.5 {
		t.Fatalf("5h used = %v, want ~16.3", q.Groups[0].FiveHour.Used)
	}
	if q.Groups[0].Weekly.Used < 52 || q.Groups[0].Weekly.Used > 52.5 {
		t.Fatalf("weekly used = %v, want ~52.3", q.Groups[0].Weekly.Used)
	}
	if q.Groups[1].FiveHour.Used != 0 || q.Groups[2].FiveHour != nil || q.Groups[2].Weekly.Used != 0 {
		t.Fatalf("groups 1/2 = %+v / %+v", q.Groups[1], q.Groups[2])
	}
	if _, ok := quotaOfAgyUsage([]byte(`not json`), now); ok {
		t.Fatal("expected invalid json to fail")
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(agyQuotaCommand())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agyQuotaCommand sh -n failed: %v\n%s", err, out)
	}
}

func TestAgyQuotaPollerStaleWhileRevalidateAndSingleflight(t *testing.T) {
	now := time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	p := &agyQuotaPoller{nowFn: clock}
	var calls int
	gate := make(chan struct{})
	fetch := func(ctx context.Context) (quotaInfo, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-gate
		}
		return quotaInfo{Known: true, UpdatedAt: clock().Unix(), Groups: []quotaGroup{{Name: "Gemini", FiveHour: &quotaWindow{Used: float64(n * 10)}}}}, nil
	}

	// Two concurrent cold requests coalesce into a single fetch.
	errCh := make(chan error, 2)
	qCh := make(chan quotaInfo, 2)
	for i := 0; i < 2; i++ {
		go func() {
			q, err := p.getOrRefresh(context.Background(), "1\x001", fetch)
			errCh <- err
			qCh <- q
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(gate)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("cold fetch err: %v", err)
		}
		q := <-qCh
		if !q.Known || len(q.Groups) != 1 || q.Groups[0].FiveHour.Used != 10 {
			t.Fatalf("unexpected cold quota: %+v", q)
		}
	}
	mu.Lock()
	if calls != 1 {
		t.Fatalf("expected singleflight 1 call, got %d", calls)
	}
	mu.Unlock()

	// Advance past fresh TTL (within stale window): returns cached immediately and triggers async refresh.
	advance(agyQuotaFresh + time.Second)
	q, err := p.getOrRefresh(context.Background(), "1\x001", fetch)
	if err != nil || !q.Known || q.Groups[0].FiveHour.Used != 10 {
		t.Fatalf("stale-while-revalidate should return cached immediately: %+v %v", q, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ent, ok := p.getCached("1\x001")
		if ok && len(ent.q.Groups) == 1 && ent.q.Groups[0].FiveHour.Used == 20 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected async background refresh to update cached quota to 20")
}
