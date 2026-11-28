package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

const (
	devinQuotaReadMax   = 32 << 10
	devinQuotaPassive   = 5 * time.Minute
	devinQuotaTimeout   = 10 * time.Second
	opencodeQuotaMax    = 64 << 10
	opencodeHostSeenTTL = 10 * time.Minute
)

// devinQuotaReadScript passively reads the newest regular non-symlink
// user_status.*.bin cache file written by Devin CLI itself under
// ${XDG_CACHE_HOME:-$HOME/.cache}/devin/cli/. It never reads credentials.toml
// or any token/cookie/auth file, and extracts only non-secret account metadata
// and PlanStatus quota fields on the host so the model catalog and everything
// else stay on the host.
const devinQuotaReadScript = `import base64, glob, json, os, struct, sys

dirs = []
xdg = os.environ.get("XDG_CACHE_HOME")
if xdg:
    dirs.append(os.path.join(xdg, "devin", "cli"))
default_dir = os.path.join(os.path.expanduser("~"), ".cache", "devin", "cli")
if default_dir not in dirs:
    dirs.append(default_dir)

cands = []
seen = set()
for cache_dir in dirs:
    if not os.path.isdir(cache_dir) or os.path.islink(cache_dir):
        continue
    for pat in ("user_status.*.bin", "user_status.bin"):
        for p in glob.glob(os.path.join(cache_dir, pat)):
            if p in seen or os.path.islink(p) or not os.path.isfile(p):
                continue
            seen.add(p)
            try:
                st = os.stat(p)
            except OSError:
                continue
            if st.st_size <= 0 or st.st_size > 4 * 1024 * 1024:
                continue
            cands.append((st.st_mtime, p))
if not cands:
    print("{}")
    sys.exit(0)
cands.sort(reverse=True)
path = cands[0][1]

try:
    with open(path, "r", encoding="utf-8") as fh:
        wrapper = json.load(fh)
    payload_b64 = wrapper.get("payload")
    if not isinstance(payload_b64, str) or not payload_b64:
        print("{}")
        sys.exit(0)
    buf = base64.b64decode(payload_b64, validate=True)
except Exception:
    print("{}")
    sys.exit(0)

def iter_pb(b):
    pos = 0
    n = len(b)
    while pos < n:
        shift = 0
        tag = 0
        while pos < n:
            x = b[pos]; pos += 1
            tag |= (x & 0x7f) << shift
            if not (x & 0x80):
                break
            shift += 7
            if shift > 63:
                return
        field = tag >> 3
        wire = tag & 7
        if wire == 0:
            shift = 0
            val = 0
            while pos < n:
                x = b[pos]; pos += 1
                val |= (x & 0x7f) << shift
                if not (x & 0x80):
                    break
                shift += 7
                if shift > 63:
                    return
            if val >= (1 << 63):
                val -= (1 << 64)
            yield field, 0, val
        elif wire == 1:
            if pos + 8 > n:
                return
            yield field, 1, struct.unpack("<d", b[pos:pos+8])[0]
            pos += 8
        elif wire == 2:
            shift = 0
            ln = 0
            while pos < n:
                x = b[pos]; pos += 1
                ln |= (x & 0x7f) << shift
                if not (x & 0x80):
                    break
                shift += 7
                if shift > 63:
                    return
            if pos + ln > n:
                return
            yield field, 2, b[pos:pos+ln]
            pos += ln
        elif wire == 5:
            if pos + 4 > n:
                return
            pos += 4
            yield field, 5, None
        else:
            return

def parse_ts(b):
    for f, w, v in iter_pb(b):
        if f == 1 and w == 0:
            return int(v)
    return 0

def safe_str(b):
    try:
        return b.decode("utf-8")
    except Exception:
        return ""

out = {}
if isinstance(wrapper.get("fetched_at_secs"), (int, float)):
    out["fetched_at"] = int(wrapper["fetched_at_secs"])

plan_status_raw = None
for f, w, v in iter_pb(buf):
    if f == 7 and w == 2:
        out["email"] = safe_str(v)
    elif f == 36 and w == 2:
        out["user_id"] = safe_str(v)
    elif f == 13 and w == 2:
        plan_status_raw = v

if plan_status_raw:
    for f, w, v in iter_pb(plan_status_raw):
        if f == 1 and w == 2:
            for pf, pw, pv in iter_pb(v):
                if pf == 2 and pw == 2:
                    out["plan_name"] = safe_str(pv)
                elif pf == 34 and pw == 0:
                    out["is_devin"] = bool(pv)
                elif pf == 36 and pw == 0:
                    out["hide_daily_quota"] = bool(pv)
                elif pf == 37 and pw == 0:
                    out["hide_weekly_quota"] = bool(pv)
        elif f == 3 and w == 2:
            out["plan_end"] = parse_ts(v)
        elif f == 14 and w == 0:
            out["has_daily_remaining"] = True
            out["daily_remaining_percent"] = int(v)
        elif f == 15 and w == 0:
            out["has_weekly_remaining"] = True
            out["weekly_remaining_percent"] = int(v)
        elif f == 17 and w == 0:
            out["daily_reset_at"] = int(v)
        elif f == 18 and w == 0:
            out["weekly_reset_at"] = int(v)
        elif f == 19 and w == 1:
            out["acu_consumed"] = v
        elif f == 20 and w == 1:
            out["acu_limit"] = v
        elif f == 21 and w == 1:
            out["dollars_consumed"] = v
        elif f == 22 and w == 1:
            out["dollars_limit"] = v

print(json.dumps(out, ensure_ascii=False, separators=(",", ":")))
`

// devinQuotaReadCommand builds the fixed read-only host command that extracts
// non-secret quota fields from Devin CLI's own user_status.*.bin cache.
func devinQuotaReadCommand() string {
	script := `command -v python3 >/dev/null 2>&1 || { echo '{}'; exit 0; }
exec python3 -c ` + sshx.QuoteShell(devinQuotaReadScript) + ` </dev/null 2>/dev/null`
	return "sh -c " + sshx.QuoteShell(script)
}

func cleanAccountKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 128 {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return ""
		}
	}
	return s
}

func validFiniteNonNeg(p *float64) (float64, bool) {
	if p == nil || math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 {
		return 0, false
	}
	return *p, true
}

// quotaOfDevinStatus parses the non-secret JSON emitted by devinQuotaReadCommand.
// Unknown/missing/garbled fields are ignored so partial payloads degrade safely.
func quotaOfDevinStatus(raw []byte, now time.Time) (q quotaInfo, accountKey, accountLabel string, ok bool) {
	var doc struct {
		FetchedAt             int64    `json:"fetched_at"`
		UserID                string   `json:"user_id"`
		Email                 string   `json:"email"`
		PlanName              string   `json:"plan_name"`
		IsDevin               bool     `json:"is_devin"`
		HideDailyQuota        bool     `json:"hide_daily_quota"`
		HideWeeklyQuota       bool     `json:"hide_weekly_quota"`
		HasDailyRemaining     bool     `json:"has_daily_remaining"`
		DailyRemainingPercent int      `json:"daily_remaining_percent"`
		DailyResetAt          int64    `json:"daily_reset_at"`
		HasWeeklyRemaining    bool     `json:"has_weekly_remaining"`
		WeeklyRemainingPercent int     `json:"weekly_remaining_percent"`
		WeeklyResetAt         int64    `json:"weekly_reset_at"`
		PlanEnd               int64    `json:"plan_end"`
		ACUConsumed           *float64 `json:"acu_consumed"`
		ACULimit              *float64 `json:"acu_limit"`
		DollarsConsumed       *float64 `json:"dollars_consumed"`
		DollarsLimit          *float64 `json:"dollars_limit"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return quotaInfo{}, "", "", false
	}
	accountKey = cleanAccountKey(doc.UserID)
	accountLabel = cleanPlanName(doc.Email)

	plan := cleanPlanName(doc.PlanName)
	if plan != "" && doc.IsDevin && !strings.HasPrefix(strings.ToLower(plan), "devin") {
		plan = "Devin " + plan
	}
	q.Plan = plan
	if doc.FetchedAt > 0 && doc.FetchedAt < 1e12 {
		q.UpdatedAt = doc.FetchedAt
	}

	if !doc.HideDailyQuota && (doc.HasDailyRemaining || doc.DailyResetAt > 0) {
		if doc.DailyRemainingPercent >= 0 && doc.DailyRemainingPercent <= 100 {
			var resetAt int64
			if doc.DailyResetAt > 0 && doc.DailyResetAt < 1e12 {
				resetAt = doc.DailyResetAt
			}
			q.Daily = &quotaWindow{
				Used:     float64(100 - doc.DailyRemainingPercent),
				ResetsAt: resetAt,
			}
		}
	}

	if !doc.HideWeeklyQuota && (doc.HasWeeklyRemaining || doc.WeeklyResetAt > 0) {
		if doc.WeeklyRemainingPercent >= 0 && doc.WeeklyRemainingPercent <= 100 {
			var resetAt int64
			if doc.WeeklyResetAt > 0 && doc.WeeklyResetAt < 1e12 {
				resetAt = doc.WeeklyResetAt
			}
			q.Weekly = &quotaWindow{
				Used:     float64(100 - doc.WeeklyRemainingPercent),
				ResetsAt: resetAt,
			}
		}
	}

	var planEnd int64
	if doc.PlanEnd > 0 && doc.PlanEnd < 1e12 {
		planEnd = doc.PlanEnd
	}
	if limit, okLim := validFiniteNonNeg(doc.ACULimit); okLim && limit > 0 {
		used, _ := validFiniteNonNeg(doc.ACUConsumed)
		q.Credits = &quotaCredits{
			Used:     math.Round(used*100) / 100,
			Limit:    math.Round(limit*100) / 100,
			Unit:     "ACU",
			ResetsAt: planEnd,
		}
	} else if limit, okLim := validFiniteNonNeg(doc.DollarsLimit); okLim && limit > 0 {
		used, _ := validFiniteNonNeg(doc.DollarsConsumed)
		q.Credits = &quotaCredits{
			Used:     math.Round(used*100) / 100,
			Limit:    math.Round(limit*100) / 100,
			Unit:     "USD",
			ResetsAt: planEnd,
		}
	}

	q.Known = q.Daily != nil || q.Weekly != nil || q.Credits != nil
	if q.Known && q.UpdatedAt == 0 {
		q.UpdatedAt = now.Unix()
	}
	return q, accountKey, accountLabel, true
}

// opencodeQuotaCommand runs OpenCode's own CLI `opencode api experimental.usage.limits`
// (falling back to `--standalone` only when the background service is not running).
// Invoked strictly on manual user Refresh (5-minute SQLite cooldown). Fixed script.
func opencodeQuotaCommand() string {
	script := `PATH="$HOME/.opencode/bin:$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
command -v opencode >/dev/null 2>&1 || exit 127
case "$(opencode service status </dev/null 2>/dev/null)" in
  http://*|https://*) exec opencode api experimental.usage.limits </dev/null 2>/dev/null ;;
  *) exec opencode api --standalone experimental.usage.limits </dev/null 2>/dev/null ;;
esac`
	return "sh -c " + sshx.QuoteShell(script)
}

func parseOpenCodeMeter(raw json.RawMessage) *quotaWindow {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Percent  *float64 `json:"percent"`
		ResetsAt *float64 `json:"resetsAt"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Percent == nil {
		return nil
	}
	pct := *m.Percent
	if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 100 {
		return nil
	}
	var resetSec int64
	if m.ResetsAt != nil && !math.IsNaN(*m.ResetsAt) && !math.IsInf(*m.ResetsAt, 0) && *m.ResetsAt > 0 {
		v := *m.ResetsAt
		if v >= 1e11 && v < 1e15 {
			resetSec = int64(v / 1000)
		} else if v < 1e11 {
			resetSec = int64(v)
		}
	}
	return &quotaWindow{
		Used:     math.Round(pct*10) / 10,
		ResetsAt: resetSec,
	}
}

// quotaOfOpenCodeUsage parses `opencode api experimental.usage.limits` JSON output,
// keeping only OpenCode Go usage numbers (5-hour rolling, weekly, monthly) and the
// non-secret workspace display label. Everything else in the output is ignored.
func quotaOfOpenCodeUsage(raw []byte, now time.Time) (q quotaInfo, label string, ok bool) {
	var root struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &root) != nil {
		return quotaInfo{}, "", false
	}
	if len(root.Data) == 0 || string(root.Data) == "null" {
		return quotaInfo{}, "", true
	}
	var data struct {
		Workspace json.RawMessage `json:"workspace"`
		Go        json.RawMessage `json:"go"`
	}
	if json.Unmarshal(root.Data, &data) != nil {
		return quotaInfo{}, "", false
	}
	if len(data.Workspace) > 0 {
		var ws struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data.Workspace, &ws) == nil {
			label = cleanPlanName(ws.Name)
		}
	}
	if len(data.Go) == 0 || string(data.Go) == "null" {
		return quotaInfo{}, label, true
	}
	var goLim struct {
		Rolling json.RawMessage `json:"rolling"`
		Weekly  json.RawMessage `json:"weekly"`
		Monthly json.RawMessage `json:"monthly"`
	}
	if json.Unmarshal(data.Go, &goLim) != nil {
		return quotaInfo{}, label, false
	}
	q.FiveHour = parseOpenCodeMeter(goLim.Rolling)
	q.Weekly = parseOpenCodeMeter(goLim.Weekly)
	q.Monthly = parseOpenCodeMeter(goLim.Monthly)
	q.Known = q.FiveHour != nil || q.Weekly != nil || q.Monthly != nil
	if q.Known {
		q.Plan = "OpenCode Go"
		q.UpdatedAt = now.Unix()
	}
	return q, label, true
}

// hostThrottle deduplicates passive host probes and SQLite host-seen writes.
type hostThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (h *hostThrottle) allow(key string, ttl time.Duration, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.last[key]; ok && now.Sub(t) < ttl {
		return false
	}
	if h.last == nil {
		h.last = map[string]time.Time{}
	}
	if len(h.last) > 256 {
		for k, t := range h.last {
			if now.Sub(t) >= ttl {
				delete(h.last, k)
			}
		}
	}
	h.last[key] = now
	return true
}

// readDevinQuotaPassive reads Devin CLI's local user_status.*.bin cache on the
// host in the background (at most once per devinQuotaPassive per owner/host)
// and persists the non-secret quota snapshot in SQLite. Never invokes any
// provider API or reads credentials.
func (s *Server) readDevinQuotaPassive(sess *term.Session) {
	if s.store == nil || sess == nil || sess.State() != term.StateRunning || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return
	}
	key := fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID)
	if !s.devinQuotaSeen.allow(key, devinQuotaPassive, time.Now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), devinQuotaTimeout)
		defer cancel()
		res, err := sshx.Exec(ctx, sess.Client(), devinQuotaReadCommand(), devinQuotaReadMax, sshx.ExecOptions{})
		if err != nil {
			return
		}
		q, accKey, accLabel, ok := quotaOfDevinStatus(res.Out, time.Now())
		if !ok || !q.Known {
			return
		}
		if accKey == "" {
			accKey = fmt.Sprintf("host:%d", sess.HostID)
		}
		s.recordSubscriptionSnapshot(sess.OwnerID, "devin", accKey, accLabel, sess.HostID, sess.HostName, q)
	}()
}

// recordOpenCodeHostSeen passively records that an OpenCode agent is present on
// sess's host so the Subscriptions page can offer a manual Refresh card without
// running any OpenCode CLI or provider calls in the background.
func (s *Server) recordOpenCodeHostSeen(sess *term.Session) {
	if s.store == nil || sess == nil || sess.OwnerID <= 0 || sess.HostID <= 0 {
		return
	}
	key := fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID)
	if !s.opencodeHostSeen.allow(key, opencodeHostSeenTTL, time.Now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.store.EnsureSubscriptionHost(ctx, sess.OwnerID, "opencode", fmt.Sprintf("host:%d", sess.HostID), sess.HostID, sess.HostName)
	}()
}

// fetchOpenCodeQuota executes OpenCode's own CLI command on the host during a
// user-initiated manual Refresh and persists the parsed Go usage snapshot.
func (s *Server) fetchOpenCodeQuota(ctx context.Context, sess *term.Session, expectedAccountKey string) error {
	if sess == nil || sess.State() != term.StateRunning {
		return errors.New("session not running")
	}
	res, err := sshx.Exec(ctx, sess.Client(), opencodeQuotaCommand(), opencodeQuotaMax, sshx.ExecOptions{})
	if err != nil {
		return err
	}
	q, label, ok := quotaOfOpenCodeUsage(res.Out, time.Now())
	if !ok || !q.Known {
		return errors.New("opencode go usage unavailable")
	}
	accKey := strings.TrimSpace(expectedAccountKey)
	if accKey == "" {
		accKey = fmt.Sprintf("host:%d", sess.HostID)
	}
	s.recordSubscriptionSnapshot(sess.OwnerID, "opencode", accKey, label, sess.HostID, sess.HostName, q)
	return nil
}
