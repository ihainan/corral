package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func buildSampleDevinCacheWrapper() []byte {
	var planInfo []byte
	planInfo = append(planInfo, pbVarintField(1, 17)...) // TEAMS_TIER_DEVIN_MAX
	planInfo = append(planInfo, pbBytesField(2, []byte("sk-secret-in-plan-name"))...)
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
	userStatus = append(userStatus, pbBytesField(3, []byte("Bearer-secret-display-name"))...)
	userStatus = append(userStatus, pbBytesField(5, []byte("secret-team-id-value"))...)
	userStatus = append(userStatus, pbBytesField(7, []byte("alice@example.com"))...)
	userStatus = append(userStatus, pbBytesField(13, planStatus)...)
	userStatus = append(userStatus, pbBytesField(33, []byte("huge-model-catalog-must-stay-on-host"))...)
	userStatus = append(userStatus, pbBytesField(36, []byte("user-test-1"))...)
	userStatus = append(userStatus, pbBytesField(99, []byte("eyJhbGciOiJIUzI1NiJ9.secret.token"))...)

	wrapper, _ := json.Marshal(map[string]any{
		"version":         1,
		"identity_digest": "deadbeef01234567",
		"fetched_at_secs": 1795869005,
		"extra_secret":    "tok-wrapper-secret-must-not-leave",
		"payload":         base64.StdEncoding.EncodeToString(userStatus),
	})
	return wrapper
}

func TestQuotaOfDevinStatus(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 30, 0, 0, time.UTC)

	// Real-shaped Devin Max sample: numeric teams_tier=17, hide_daily_quota=true, weekly remaining omitted (=0 -> 100% used).
	rawMax := []byte(`{"fetched_at":1795869005,"user_id":"user-abc123","email":"dev@example.com","teams_tier":17,"is_devin":true,"hide_daily_quota":true,"hide_weekly_quota":false,"has_daily_remaining":true,"daily_remaining_percent":100,"daily_reset_at":1795939200,"weekly_reset_at":1795939200,"plan_end":1797057675}`)
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

	// Both daily and weekly visible plus ACU credits (teams_tier=16 -> Devin Pro).
	rawBoth := []byte(`{"fetched_at":1795869005,"user_id":"user-2","teams_tier":16,"is_devin":true,"hide_daily_quota":false,"hide_weekly_quota":false,"has_daily_remaining":true,"daily_remaining_percent":75,"daily_reset_at":1795900000,"has_weekly_remaining":true,"weekly_remaining_percent":60,"weekly_reset_at":1796000000,"plan_end":1797000000,"acu_consumed":12.5,"acu_limit":50}`)
	q, _, _, ok = quotaOfDevinStatus(rawBoth, now)
	if !ok || !q.Known || q.Plan != "Devin Pro" {
		t.Fatalf("devin pro failed: ok=%v q=%+v", ok, q)
	}
	if q.Daily == nil || q.Daily.Used != 25 || q.Weekly == nil || q.Weekly.Used != 40 {
		t.Fatalf("unexpected daily/weekly: daily=%+v weekly=%+v", q.Daily, q.Weekly)
	}
	if q.Credits == nil || q.Credits.Unit != "ACU" || q.Credits.Used != 12.5 || q.Credits.Limit != 50 || q.Credits.ResetsAt != 1797000000 {
		t.Fatalf("unexpected ACU credits: %+v", q.Credits)
	}

	// Dollar-limit credits plan (teams_tier=14 -> Devin Teams).
	rawUSD := []byte(`{"teams_tier":14,"is_devin":true,"hide_daily_quota":true,"hide_weekly_quota":true,"dollars_consumed":8.25,"dollars_limit":100,"plan_end":1797000000}`)
	q, _, _, ok = quotaOfDevinStatus(rawUSD, now)
	if !ok || !q.Known || q.Plan != "Devin Teams" || q.Credits == nil || q.Credits.Unit != "USD" || q.Credits.Used != 8.25 || q.Credits.Limit != 100 {
		t.Fatalf("devin USD failed: ok=%v q=%+v", ok, q)
	}

	// Arbitrary plan_name string in input must be ignored (only numeric teams_tier is mapped).
	rawPlanStr := []byte(`{"plan_name":"InjectedPlan","is_devin":true,"hide_daily_quota":true,"has_weekly_remaining":true,"weekly_remaining_percent":50,"weekly_reset_at":1796000000}`)
	q, _, _, ok = quotaOfDevinStatus(rawPlanStr, now)
	if !ok || !q.Known || q.Plan != "" {
		t.Fatalf("plan_name string should be ignored, got plan=%q q=%+v", q.Plan, q)
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

// Fix 1 [P1]: Devin script must run under `python3 -I` from `cd /` so a planted
// json.py/base64.py in cwd or PYTHONPATH cannot execute. Fails on 3a6a603.
func TestDevinQuotaReadCommandIsolatedPythonAndSafeCwd(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ".cache", "devin", "cli")
	os.MkdirAll(cacheDir, 0o700)
	os.WriteFile(filepath.Join(cacheDir, "user_status.deadbeef01234567.bin"), buildSampleDevinCacheWrapper(), 0o600)

	evilDir := t.TempDir()
	marker := filepath.Join(evilDir, "pwned.marker")
	evilMod := "open(" + strconvQuote(marker) + ", 'w').write('HIJACKED')\nprint('HIJACKED')\n"
	os.WriteFile(filepath.Join(evilDir, "json.py"), []byte(evilMod), 0o600)
	os.WriteFile(filepath.Join(evilDir, "base64.py"), []byte(evilMod), 0o600)

	cmd := exec.Command("sh", "-c", devinQuotaReadCommand())
	cmd.Dir = evilDir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"PYTHONPATH="+evilDir,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("devinQuotaReadCommand failed: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("planted json.py/base64.py executed! marker file created at %s", marker)
	}
	if strings.Contains(string(out), "HIJACKED") {
		t.Fatalf("planted module output leaked: %s", out)
	}
	q, accKey, label, ok := quotaOfDevinStatus(out, time.Unix(1795869100, 0))
	if !ok || !q.Known || accKey != "user-test-1" || label != "alice@example.com" || q.Plan != "Devin Max" {
		t.Fatalf("isolated run did not parse valid cache: ok=%v key=%q label=%q q=%+v out=%s", ok, accKey, label, q, out)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Fix 4 [P1] & Fix 6 [P2]: O_NOFOLLOW at every directory step and on the cache
// file, fstat regular-file + size-cap enforcement, FIFO rejection, and strict
// field allowlist (only numeric/boolean plan/quota fields + email & user_id;
// no other strings leave the host). Fails on 3a6a603.
func TestDevinQuotaReadCommandNoFollowCapAndAllowlist(t *testing.T) {
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(devinQuotaReadCommand())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("devinQuotaReadCommand sh -n failed: %v\n%s", err, out)
	}

	runIn := func(home, xdg string) string {
		c := exec.Command("sh", "-c", devinQuotaReadCommand())
		env := append(os.Environ(), "HOME="+home)
		if xdg != "" {
			env = append(env, "XDG_CACHE_HOME="+xdg)
		} else {
			env = append(env, "XDG_CACHE_HOME=")
		}
		c.Env = env
		out, err := c.Output()
		if err != nil {
			t.Fatalf("devinQuotaReadCommand failed: %v", err)
		}
		return string(out)
	}

	// 1. Missing cache -> {}.
	home := t.TempDir()
	if got := strings.TrimSpace(runIn(home, "")); got != "{}" {
		t.Fatalf("missing cache: got %q, want {}", got)
	}

	// 2. Valid cache with extra token-like strings across plan_name, name, team_id,
	//    catalog, unknown tag 99, wrapper field, and credentials.toml:
	//    ONLY numeric/boolean fields + email and user_id may leave the host.
	shareDir := filepath.Join(home, ".local", "share", "devin")
	os.MkdirAll(shareDir, 0o700)
	os.WriteFile(filepath.Join(shareDir, "credentials.toml"), []byte("api_key = \"super-secret-devin-token\"\n"), 0o600)

	cacheDir := filepath.Join(home, ".cache", "devin", "cli")
	os.MkdirAll(cacheDir, 0o700)
	wrapper := buildSampleDevinCacheWrapper()
	cacheFile := filepath.Join(cacheDir, "user_status.deadbeef01234567.bin")
	os.WriteFile(cacheFile, wrapper, 0o600)

	out := runIn(home, "")
	for _, forbidden := range []string{
		"super-secret-devin-token",
		"sk-secret-in-plan-name",
		"Bearer-secret-display-name",
		"secret-team-id-value",
		"huge-model-catalog-must-stay-on-host",
		"eyJhbGciOiJIUzI1NiJ9.secret.token",
		"tok-wrapper-secret-must-not-leave",
		"plan_name",
	} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("%q leaked out of host: %s", forbidden, out)
		}
	}
	var rawMap map[string]any
	if err := json.Unmarshal([]byte(out), &rawMap); err != nil {
		t.Fatalf("invalid json output %q: %v", out, err)
	}
	for k, v := range rawMap {
		switch v.(type) {
		case float64, bool:
			// numeric/boolean plan/quota fields are allowed
		case string:
			if k != "email" && k != "user_id" {
				t.Fatalf("unexpected string field %q=%q left the host (only email and user_id allowed)", k, v)
			}
		default:
			t.Fatalf("unexpected value type for %q: %T (%v)", k, v, v)
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

	// 3. Symlinked file must be refused via O_NOFOLLOW.
	os.Remove(cacheFile)
	realTarget := filepath.Join(t.TempDir(), "other.bin")
	os.WriteFile(realTarget, wrapper, 0o600)
	os.Symlink(realTarget, cacheFile)
	if got := strings.TrimSpace(runIn(home, "")); got != "{}" {
		t.Fatalf("symlink file should be refused, got %q", got)
	}
	os.Remove(cacheFile)

	// 4. Symlinked intermediate directory ($HOME/.cache/devin -> target) must NOT be followed.
	//    (On 3a6a603, os.path.islink(cache_dir) only checked the final 'cli' component and followed 'devin'!)
	homeMidLink := t.TempDir()
	realDevin := filepath.Join(t.TempDir(), "real-devin")
	os.MkdirAll(filepath.Join(realDevin, "cli"), 0o700)
	os.WriteFile(filepath.Join(realDevin, "cli", "user_status.abcd.bin"), wrapper, 0o600)
	os.MkdirAll(filepath.Join(homeMidLink, ".cache"), 0o700)
	os.Symlink(realDevin, filepath.Join(homeMidLink, ".cache", "devin"))
	if got := strings.TrimSpace(runIn(homeMidLink, "")); got != "{}" {
		t.Fatalf("symlinked intermediate 'devin' directory must not be followed, got %q", got)
	}

	// 5. Symlinked '.cache' directory must NOT be followed.
	homeCacheLink := t.TempDir()
	realCache := filepath.Join(t.TempDir(), "real-cache")
	os.MkdirAll(filepath.Join(realCache, "devin", "cli"), 0o700)
	os.WriteFile(filepath.Join(realCache, "devin", "cli", "user_status.abcd.bin"), wrapper, 0o600)
	os.Symlink(realCache, filepath.Join(homeCacheLink, ".cache"))
	if got := strings.TrimSpace(runIn(homeCacheLink, "")); got != "{}" {
		t.Fatalf("symlinked '.cache' directory must not be followed, got %q", got)
	}

	// 6. Oversized file (> 4 MiB cap) must be refused.
	homeBig := t.TempDir()
	bigDir := filepath.Join(homeBig, ".cache", "devin", "cli")
	os.MkdirAll(bigDir, 0o700)
	bigBuf := make([]byte, 4*1024*1024+128)
	for i := range bigBuf {
		bigBuf[i] = ' '
	}
	copy(bigBuf, wrapper)
	os.WriteFile(filepath.Join(bigDir, "user_status.big.bin"), bigBuf, 0o600)
	if got := strings.TrimSpace(runIn(homeBig, "")); got != "{}" {
		t.Fatalf("oversized cache file should be refused, got %q", got)
	}

	// 7. Non-regular file (FIFO) must be rejected without blocking via O_NONBLOCK + S_ISREG.
	homeFifo := t.TempDir()
	fifoDir := filepath.Join(homeFifo, ".cache", "devin", "cli")
	os.MkdirAll(fifoDir, 0o700)
	if err := syscall.Mkfifo(filepath.Join(fifoDir, "user_status.fifo.bin"), 0o600); err == nil {
		if got := strings.TrimSpace(runIn(homeFifo, "")); got != "{}" {
			t.Fatalf("FIFO must be refused, got %q", got)
		}
	}
}

// Fix 3 [P1] & Fix 5 [P1]: OpenCode parser keeps ONLY usage numbers (percent,
// limited, resetsAt) and derives opaque hashed workspace key ("ws:<16 hex>");
// workspace.name is dropped; missing workspace.id returns nothing. Fails on 3a6a603.
func TestQuotaOfOpenCodeUsage(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 25, 0, 0, time.UTC)

	raw := []byte(`{"location":{"directory":"/home/user/proj"},"data":{"workspace":{"id":"wrk_01ABC","name":"SECRET-WORKSPACE-NAME","usageURL":"https://opencode.ai/console/wrk_01ABC/activity/overview"},"go":{"rolling":{"percent":100,"limited":true,"resetsAt":1795869156000},"weekly":{"percent":36,"limited":false,"resetsAt":1795996800000},"monthly":{"percent":38,"limited":false,"resetsAt":1797315094000}},"budgets":[{"id":"b1","limit":100,"spent":5}]}}`)
	q, wsKey, ok := quotaOfOpenCodeUsage(raw, now)
	sum := sha256.Sum256([]byte("wrk_01ABC"))
	wantKey := "ws:" + hex.EncodeToString(sum[:8])
	if !ok || !q.Known || q.Plan != "OpenCode Go" || wsKey != wantKey {
		t.Fatalf("unexpected opencode quota: ok=%v wsKey=%q (want %q) q=%+v", ok, wsKey, wantKey, q)
	}
	if q.FiveHour == nil || q.FiveHour.Used != 100 || !q.FiveHour.Limited || q.FiveHour.ResetsAt != 1795869156 {
		t.Fatalf("unexpected five_hour (expected limited=true): %+v", q.FiveHour)
	}
	if q.Weekly == nil || q.Weekly.Used != 36 || q.Weekly.Limited || q.Weekly.ResetsAt != 1795996800 {
		t.Fatalf("unexpected weekly: %+v", q.Weekly)
	}
	if q.Monthly == nil || q.Monthly.Used != 38 || q.Monthly.Limited || q.Monthly.ResetsAt != 1797315094 {
		t.Fatalf("unexpected monthly: %+v", q.Monthly)
	}
	qBytes, _ := json.Marshal(q)
	for _, forbidden := range []string{"SECRET-WORKSPACE-NAME", "wrk_01ABC", "/home/user/proj", "usageURL"} {
		if strings.Contains(string(qBytes), forbidden) || strings.Contains(wsKey, forbidden) {
			t.Fatalf("%q leaked into parsed OpenCode output: wsKey=%q q=%s", forbidden, wsKey, qBytes)
		}
	}

	// Missing workspace.id -> must NOT return a known snapshot (nothing stored).
	rawNoWS := []byte(`{"data":{"workspace":{"name":"OnlyName"},"go":{"rolling":{"percent":10,"limited":false,"resetsAt":1795869156000}}}}`)
	q, wsKey, ok = quotaOfOpenCodeUsage(rawNoWS, now)
	if !ok || q.Known || wsKey != "" {
		t.Fatalf("missing workspace.id must return known=false wsKey='': ok=%v wsKey=%q q=%+v", ok, wsKey, q)
	}

	// Partial / garbled meters: valid meters survive, invalid ones are ignored.
	rawPartial := []byte(`{"data":{"workspace":{"id":"wrk_2","name":"Team Space"},"go":{"rolling":"bad","weekly":{"percent":42.5,"limited":true,"resetsAt":1795996800000},"monthly":{"percent":150,"resetsAt":1797315094000}}}}`)
	q, wsKey, ok = quotaOfOpenCodeUsage(rawPartial, now)
	if !ok || !q.Known || !strings.HasPrefix(wsKey, "ws:") || q.FiveHour != nil || q.Monthly != nil || q.Weekly == nil || q.Weekly.Used != 42.5 || !q.Weekly.Limited {
		t.Fatalf("partial parse failed: ok=%v wsKey=%q q=%+v", ok, wsKey, q)
	}

	// Signed out (data: null) or no Go subscription (go: null) -> Known=false, ok=true.
	for _, empty := range []string{`{"data":null}`, `{"data":{"workspace":{"id":"wrk_1","name":"Default"},"go":null,"budgets":[]}}`} {
		q, _, ok := quotaOfOpenCodeUsage([]byte(empty), now)
		if !ok || q.Known {
			t.Fatalf("%s: want ok=true known=false, got ok=%v q=%+v", empty, ok, q)
		}
	}

	// 503 / non-JSON error output -> Known=false.
	for _, bad := range []string{``, `503 Service Unavailable`, `{"error":"upstream 503"}`, `not json`} {
		q, _, _ := quotaOfOpenCodeUsage([]byte(bad), now)
		if q.Known {
			t.Fatalf("%q: must not be known, got q=%+v", bad, q)
		}
	}
}

// Fix 2 [P1]: Drop --standalone fallback completely (exit 75 when service is not
// running, never invoking `opencode api`), and bound remote command with host
// `timeout -k 5 30` when coreutils timeout is available. Fails on 3a6a603.
func TestOpenCodeQuotaCommandNoStandaloneAndTimeout(t *testing.T) {
	cmdStr := opencodeQuotaCommand()
	if strings.Contains(cmdStr, "--standalone") {
		t.Fatalf("opencodeQuotaCommand must not contain --standalone: %s", cmdStr)
	}
	if !strings.Contains(cmdStr, "timeout -k 5 30") {
		t.Fatalf("opencodeQuotaCommand must bound execution with timeout -k 5 30: %s", cmdStr)
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(cmdStr)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("opencodeQuotaCommand sh -n failed: %v\n%s", err, out)
	}

	home := t.TempDir()
	bin := filepath.Join(home, ".opencode", "bin")
	os.MkdirAll(bin, 0o755)
	argsLog := filepath.Join(home, "args.log")
	timeoutLog := filepath.Join(home, "timeout.log")

	// Fake `timeout` in $HOME/.local/bin so we observe exact `timeout -k 5 30 ...` invocations.
	localBin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(localBin, 0o755)
	os.WriteFile(filepath.Join(localBin, "timeout"), []byte(`#!/bin/sh
if [ "$1" = "-k" ] && [ "$2" = "1" ] && [ "$3" = "1" ] && [ "$4" = "true" ]; then
  exit 0
fi
printf '%s\n' "$*" >> `+timeoutLog+`
shift 3
exec "$@"
`), 0o755)

	// Case 1: service running -> bounded by `timeout -k 5 30` for both status and api call.
	fakeOC := filepath.Join(bin, "opencode")
	os.WriteFile(fakeOC, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+argsLog+`
if [ "$1" = "service" ] && [ "$2" = "status" ]; then
  echo "http://127.0.0.1:49374"
  exit 0
fi
echo '{"data":{"workspace":{"id":"wrk_test1","name":"Ignored"},"go":{"rolling":{"percent":12,"limited":false,"resetsAt":1795869156000}}}}'
`), 0o755)
	c := exec.Command("sh", "-c", cmdStr)
	c.Env = append(os.Environ(), "HOME="+home)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("opencodeQuotaCommand (service running) failed: %v", err)
	}
	tBytes, _ := os.ReadFile(timeoutLog)
	tLines := strings.Split(strings.TrimSpace(string(tBytes)), "\n")
	if len(tLines) != 2 ||
		tLines[0] != "-k 5 30 opencode service status" ||
		tLines[1] != "-k 5 30 opencode api experimental.usage.limits" {
		t.Fatalf("expected timeout -k 5 30 wrapping both calls, got %q", string(tBytes))
	}
	if q, wsKey, ok := quotaOfOpenCodeUsage(out, time.Now()); !ok || !q.Known || !strings.HasPrefix(wsKey, "ws:") || q.FiveHour == nil || q.FiveHour.Used != 12 {
		t.Fatalf("unexpected parsed output: ok=%v wsKey=%q q=%+v out=%s", ok, wsKey, q, out)
	}

	// Case 2: service NOT running (non-zero status or non-URL output) -> exits 75 immediately, NEVER calls `opencode api`.
	for _, statusBody := range []string{"exit 1", "echo 'stopped'; exit 0"} {
		os.Remove(argsLog)
		os.WriteFile(fakeOC, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+argsLog+`
if [ "$1" = "service" ] && [ "$2" = "status" ]; then
  `+statusBody+`
fi
echo "SHOULD_NOT_RUN"
`), 0o755)
		c = exec.Command("sh", "-c", cmdStr)
		c.Env = append(os.Environ(), "HOME="+home)
		out, err = c.Output()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != opencodeNotRunningExitCode {
			t.Fatalf("service stopped (%s): expected exit %d, got err=%v out=%q", statusBody, opencodeNotRunningExitCode, err, out)
		}
		aBytes, _ := os.ReadFile(argsLog)
		if strings.Contains(string(aBytes), "api") || strings.Contains(string(aBytes), "--standalone") {
			t.Fatalf("when service is stopped (%s), must not invoke api or --standalone; got %q", statusBody, string(aBytes))
		}
	}
}
