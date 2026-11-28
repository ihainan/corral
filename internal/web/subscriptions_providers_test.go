package web

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pbVarint(v uint64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			break
		}
	}
	return b
}

func pbTag(field, wire uint64) []byte { return pbVarint((field << 3) | wire) }

func pbVarintField(field uint64, val int64) []byte {
	return append(pbTag(field, 0), pbVarint(uint64(val))...)
}

func pbBytesField(field uint64, payload []byte) []byte {
	out := append(pbTag(field, 2), pbVarint(uint64(len(payload)))...)
	return append(out, payload...)
}

func pbFloat64Field(field uint64, val float64) []byte {
	out := pbTag(field, 1)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(val))
	return append(out, b[:]...)
}

func TestQuotaOfDevinStatus(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 30, 0, 0, time.UTC)

	// Real-shaped Devin Max sample: hide_daily_quota=true, weekly remaining omitted (=0 -> 100% used).
	rawMax := []byte(`{"fetched_at":1795869005,"user_id":"user-abc123","email":"dev@example.com","plan_name":"Max","is_devin":true,"hide_daily_quota":true,"hide_weekly_quota":false,"has_daily_remaining":true,"daily_remaining_percent":100,"daily_reset_at":1795939200,"weekly_reset_at":1795939200,"plan_end":1797057675}`)
	q, accKey, label, ok := quotaOfDevinStatus(rawMax, now)
	if !ok || !q.Known {
		t.Fatalf("expected valid devin max quota: ok=%v q=%+v", ok, q)
	}
	if accKey != "user-abc123" || label != "dev@example.com" || q.Plan != "Devin Max" {
		t.Fatalf("unexpected identity/plan: key=%q label=%q plan=%q", accKey, label, q.Plan)
	}
	if q.Daily != nil {
		t.Fatalf("hidden daily quota must be omitted, got %+v", q.Daily)
	}
	if q.Weekly == nil || q.Weekly.Used != 100 || q.Weekly.ResetsAt != 1795939200 {
		t.Fatalf("unexpected weekly window: %+v", q.Weekly)
	}
	if q.UpdatedAt != 1795869005 {
		t.Fatalf("unexpected updated_at: %d", q.UpdatedAt)
	}

	// Both daily and weekly visible plus ACU credits.
	rawBoth := []byte(`{"fetched_at":1795869005,"user_id":"user-2","plan_name":"Core","is_devin":true,"hide_daily_quota":false,"hide_weekly_quota":false,"has_daily_remaining":true,"daily_remaining_percent":75,"daily_reset_at":1795900000,"has_weekly_remaining":true,"weekly_remaining_percent":60,"weekly_reset_at":1796000000,"plan_end":1797000000,"acu_consumed":12.5,"acu_limit":50}`)
	q, _, _, ok = quotaOfDevinStatus(rawBoth, now)
	if !ok || !q.Known || q.Plan != "Devin Core" {
		t.Fatalf("devin core failed: ok=%v q=%+v", ok, q)
	}
	if q.Daily == nil || q.Daily.Used != 25 || q.Weekly == nil || q.Weekly.Used != 40 {
		t.Fatalf("unexpected daily/weekly: daily=%+v weekly=%+v", q.Daily, q.Weekly)
	}
	if q.Credits == nil || q.Credits.Unit != "ACU" || q.Credits.Used != 12.5 || q.Credits.Limit != 50 || q.Credits.ResetsAt != 1797000000 {
		t.Fatalf("unexpected ACU credits: %+v", q.Credits)
	}

	// Dollar-limit credits plan.
	rawUSD := []byte(`{"plan_name":"Devin Team","is_devin":true,"hide_daily_quota":true,"hide_weekly_quota":true,"dollars_consumed":8.25,"dollars_limit":100,"plan_end":1797000000}`)
	q, _, _, ok = quotaOfDevinStatus(rawUSD, now)
	if !ok || !q.Known || q.Plan != "Devin Team" || q.Credits == nil || q.Credits.Unit != "USD" || q.Credits.Used != 8.25 || q.Credits.Limit != 100 {
		t.Fatalf("devin USD failed: ok=%v q=%+v", ok, q)
	}

	// Empty object, out-of-range percentages, and garbled JSON.
	if q, _, _, ok := quotaOfDevinStatus([]byte(`{}`), now); !ok || q.Known {
		t.Fatalf("empty object should yield known=false ok=true, got ok=%v q=%+v", ok, q)
	}
	if q, _, _, ok := quotaOfDevinStatus([]byte(`{"has_daily_remaining":true,"daily_remaining_percent":140,"has_weekly_remaining":true,"weekly_remaining_percent":-5}`), now); !ok || q.Known {
		t.Fatalf("out-of-range percentages should be ignored, got ok=%v q=%+v", ok, q)
	}
	if _, _, _, ok := quotaOfDevinStatus([]byte(`not json`), now); ok {
		t.Fatalf("invalid json must return ok=false")
	}
}

func TestDevinQuotaReadCommand(t *testing.T) {
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(devinQuotaReadCommand())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("devinQuotaReadCommand sh -n failed: %v\n%s", err, out)
	}

	home := t.TempDir()
	run := func() string {
		c := exec.Command("sh", "-c", devinQuotaReadCommand())
		c.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME="+filepath.Join(home, ".cache"))
		out, err := c.Output()
		if err != nil {
			t.Fatalf("devinQuotaReadCommand failed: %v", err)
		}
		return string(out)
	}

	// No cache file yet -> empty JSON object.
	if got := strings.TrimSpace(run()); got != "{}" {
		t.Fatalf("missing cache: got %q, want {}", got)
	}

	// Write a fake credentials.toml alongside the cache to confirm it is never read.
	shareDir := filepath.Join(home, ".local", "share", "devin")
	os.MkdirAll(shareDir, 0o700)
	os.WriteFile(filepath.Join(shareDir, "credentials.toml"), []byte("api_key = \"super-secret-devin-token\"\n"), 0o600)

	// Build a real-shaped protobuf UserStatus message.
	var planInfo []byte
	planInfo = append(planInfo, pbVarintField(1, 17)...)
	planInfo = append(planInfo, pbBytesField(2, []byte("Max"))...)
	planInfo = append(planInfo, pbVarintField(34, 1)...) // is_devin = true
	planInfo = append(planInfo, pbVarintField(36, 1)...) // hide_daily_quota = true

	planEnd := pbVarintField(1, 1797057675)

	var planStatus []byte
	planStatus = append(planStatus, pbBytesField(1, planInfo)...)
	planStatus = append(planStatus, pbBytesField(3, planEnd)...)
	planStatus = append(planStatus, pbVarintField(14, 100)...)
	planStatus = append(planStatus, pbVarintField(15, 35)...) // 35% remaining -> 65% used
	planStatus = append(planStatus, pbVarintField(17, 1795939200)...)
	planStatus = append(planStatus, pbVarintField(18, 1795939200)...)
	planStatus = append(planStatus, pbFloat64Field(19, 4.5)...)
	planStatus = append(planStatus, pbFloat64Field(20, 20.0)...)

	var userStatus []byte
	userStatus = append(userStatus, pbBytesField(7, []byte("alice@example.com"))...)
	userStatus = append(userStatus, pbBytesField(13, planStatus)...)
	userStatus = append(userStatus, pbBytesField(33, []byte("huge-model-catalog-must-stay-on-host"))...)
	userStatus = append(userStatus, pbBytesField(36, []byte("user-test-1"))...)

	wrapper, _ := json.Marshal(map[string]any{
		"version":         1,
		"identity_digest": "deadbeef01234567",
		"fetched_at_secs": 1795869005,
		"payload":         base64.StdEncoding.EncodeToString(userStatus),
	})
	cacheDir := filepath.Join(home, ".cache", "devin", "cli")
	os.MkdirAll(cacheDir, 0o700)
	cacheFile := filepath.Join(cacheDir, "user_status.deadbeef01234567.bin")
	os.WriteFile(cacheFile, wrapper, 0o600)

	out := run()
	for _, forbidden := range []string{"super-secret-devin-token", "huge-model-catalog-must-stay-on-host"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("%q leaked out of host: %s", forbidden, out)
		}
	}
	q, accKey, label, ok := quotaOfDevinStatus([]byte(out), time.Unix(1795869100, 0))
	if !ok || !q.Known || accKey != "user-test-1" || label != "alice@example.com" || q.Plan != "Devin Max" {
		t.Fatalf("parsed output mismatch: ok=%v key=%q label=%q q=%+v out=%s", ok, accKey, label, q, out)
	}
	if q.Daily != nil || q.Weekly == nil || q.Weekly.Used != 65 || q.Weekly.ResetsAt != 1795939200 {
		t.Fatalf("unexpected windows: daily=%+v weekly=%+v", q.Daily, q.Weekly)
	}
	if q.Credits == nil || q.Credits.Unit != "ACU" || q.Credits.Used != 4.5 || q.Credits.Limit != 20 {
		t.Fatalf("unexpected credits: %+v", q.Credits)
	}

	// Symlinked cache file must be refused.
	os.Remove(cacheFile)
	target := filepath.Join(t.TempDir(), "other.bin")
	os.WriteFile(target, wrapper, 0o600)
	os.Symlink(target, cacheFile)
	if got := strings.TrimSpace(run()); got != "{}" {
		t.Fatalf("symlink cache should be refused, got %q", got)
	}
}

func TestQuotaOfOpenCodeUsage(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 25, 0, 0, time.UTC)

	// Real-shaped opencode api experimental.usage.limits output.
	raw := []byte(`{"location":{"directory":"/home/user/proj"},"data":{"workspace":{"id":"wrk_01ABC","name":"Default","usageURL":"https://opencode.ai/console/wrk_01ABC/activity/overview"},"go":{"rolling":{"percent":0,"limited":false,"resetsAt":1795869156000},"weekly":{"percent":36,"limited":false,"resetsAt":1795996800000},"monthly":{"percent":38,"limited":false,"resetsAt":1797315094000}},"budgets":[{"id":"b1","limit":100,"spent":5}]}}`)
	q, label, ok := quotaOfOpenCodeUsage(raw, now)
	if !ok || !q.Known || q.Plan != "OpenCode Go" || label != "Default" {
		t.Fatalf("unexpected opencode quota: ok=%v label=%q q=%+v", ok, label, q)
	}
	if q.FiveHour == nil || q.FiveHour.Used != 0 || q.FiveHour.ResetsAt != 1795869156 {
		t.Fatalf("unexpected five_hour: %+v", q.FiveHour)
	}
	if q.Weekly == nil || q.Weekly.Used != 36 || q.Weekly.ResetsAt != 1795996800 {
		t.Fatalf("unexpected weekly: %+v", q.Weekly)
	}
	if q.Monthly == nil || q.Monthly.Used != 38 || q.Monthly.ResetsAt != 1797315094 {
		t.Fatalf("unexpected monthly: %+v", q.Monthly)
	}

	// Partial / garbled meters: valid meters survive, invalid ones are ignored.
	rawPartial := []byte(`{"data":{"workspace":{"name":"Team Space"},"go":{"rolling":"bad","weekly":{"percent":42.5,"resetsAt":1795996800000},"monthly":{"percent":150,"resetsAt":1797315094000}}}}`)
	q, label, ok = quotaOfOpenCodeUsage(rawPartial, now)
	if !ok || !q.Known || label != "Team Space" || q.FiveHour != nil || q.Monthly != nil || q.Weekly == nil || q.Weekly.Used != 42.5 {
		t.Fatalf("partial parse failed: ok=%v label=%q q=%+v", ok, label, q)
	}

	// Signed out (data: null) or no Go subscription (go: null) -> Known=false, ok=true.
	for _, empty := range []string{`{"data":null}`, `{"data":{"workspace":{"name":"Default"},"go":null,"budgets":[]}}`} {
		q, _, ok := quotaOfOpenCodeUsage([]byte(empty), now)
		if !ok || q.Known {
			t.Fatalf("%s: want ok=true known=false, got ok=%v q=%+v", empty, ok, q)
		}
	}

	// 503 / non-JSON error output -> ok=false.
	for _, bad := range []string{``, `503 Service Unavailable`, `{"error":"upstream 503"}`, `not json`} {
		q, _, ok := quotaOfOpenCodeUsage([]byte(bad), now)
		if q.Known {
			t.Fatalf("%q: must not be known, got ok=%v q=%+v", bad, ok, q)
		}
	}
}

func TestOpenCodeQuotaCommand(t *testing.T) {
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(opencodeQuotaCommand())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opencodeQuotaCommand sh -n failed: %v\n%s", err, out)
	}

	home := t.TempDir()
	bin := filepath.Join(home, ".opencode", "bin")
	os.MkdirAll(bin, 0o755)
	argsLog := filepath.Join(home, "args.log")

	// Case 1: service running -> uses `opencode api experimental.usage.limits` without --standalone.
	fakeOC := filepath.Join(bin, "opencode")
	os.WriteFile(fakeOC, []byte(`#!/bin/sh
if [ "$1" = "service" ] && [ "$2" = "status" ]; then
  echo "http://127.0.0.1:49374"
  exit 0
fi
printf '%s\n' "$*" > `+argsLog+`
echo '{"data":{"workspace":{"name":"Default"},"go":{"rolling":{"percent":12,"resetsAt":1795869156000}}}}'
`), 0o755)
	c := exec.Command("sh", "-c", opencodeQuotaCommand())
	c.Env = append(os.Environ(), "HOME="+home)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("opencodeQuotaCommand (service running) failed: %v", err)
	}
	argsBytes, _ := os.ReadFile(argsLog)
	if got := strings.TrimSpace(string(argsBytes)); got != "api experimental.usage.limits" {
		t.Fatalf("expected service path args, got %q", got)
	}
	if q, _, ok := quotaOfOpenCodeUsage(out, time.Now()); !ok || !q.Known || q.FiveHour == nil || q.FiveHour.Used != 12 {
		t.Fatalf("unexpected parsed output: ok=%v q=%+v out=%s", ok, q, out)
	}

	// Case 2: service stopped -> uses `opencode api --standalone experimental.usage.limits`.
	os.WriteFile(fakeOC, []byte(`#!/bin/sh
if [ "$1" = "service" ] && [ "$2" = "status" ]; then
  exit 1
fi
printf '%s\n' "$*" > `+argsLog+`
echo '{"data":{"workspace":{"name":"Default"},"go":{"weekly":{"percent":45,"resetsAt":1795996800000}}}}'
`), 0o755)
	c = exec.Command("sh", "-c", opencodeQuotaCommand())
	c.Env = append(os.Environ(), "HOME="+home)
	out, err = c.Output()
	if err != nil {
		t.Fatalf("opencodeQuotaCommand (standalone) failed: %v", err)
	}
	argsBytes, _ = os.ReadFile(argsLog)
	if got := strings.TrimSpace(string(argsBytes)); got != "api --standalone experimental.usage.limits" {
		t.Fatalf("expected standalone args, got %q", got)
	}
	if q, _, ok := quotaOfOpenCodeUsage(out, time.Now()); !ok || !q.Known || q.Weekly == nil || q.Weekly.Used != 45 {
		t.Fatalf("unexpected standalone output: ok=%v q=%+v out=%s", ok, q, out)
	}
}
