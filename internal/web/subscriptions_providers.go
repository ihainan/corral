package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	devinQuotaReadMax          = 32 << 10
	devinQuotaPassive          = 5 * time.Minute
	devinQuotaTimeout          = 10 * time.Second
	opencodeQuotaMax           = 64 << 10
	opencodeHostSeenTTL        = 10 * time.Minute
	opencodeNotRunningExitCode = 75 // sentinel exit code when opencode service is not running on host
)

var errOpenCodeNotRunning = errors.New("opencode service is not running on this host")

// devinQuotaReadScript passively reads the newest regular non-symlink
// user_status.*.bin cache file written by Devin CLI itself under
// ${XDG_CACHE_HOME}/devin/cli and ~/.cache/devin/cli. Every directory step and
// the target file itself are opened with O_NOFOLLOW (via dir_fd), verified via
// fstat (regular file, size within CAP), and read from that exact descriptor up
// to CAP bytes so no symlink can be followed at any step. Only numeric/boolean
// plan & quota fields plus email and user_id leave the host; all other strings
// and fields stay on the host.
const devinQuotaReadScript = `import base64, json, os, stat, struct, sys

CAP = 4 * 1024 * 1024
O_NOFOLLOW = getattr(os, "O_NOFOLLOW", 0)
O_DIRECTORY = getattr(os, "O_DIRECTORY", 0)
O_NONBLOCK = getattr(os, "O_NONBLOCK", 0)

def open_dir_nofollow(base, parts):
    if not base or not os.path.isabs(base):
        return -1
    try:
        fd = os.open(base, os.O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
    except OSError:
        return -1
    for comp in parts:
        try:
            nfd = os.open(comp, os.O_RDONLY | O_DIRECTORY | O_NOFOLLOW, dir_fd=fd)
        except OSError:
            os.close(fd)
            return -1
        os.close(fd)
        fd = nfd
    return fd

roots = []
xdg = os.environ.get("XDG_CACHE_HOME")
if xdg and os.path.isabs(xdg):
    roots.append((xdg, ("devin", "cli")))
home = os.path.expanduser("~")
if home and os.path.isabs(home):
    roots.append((home, (".cache", "devin", "cli")))

best_mtime = None
best_fd = -1
seen_dev_ino = set()

for base, parts in roots:
    dfd = open_dir_nofollow(base, parts)
    if dfd < 0:
        continue
    try:
        try:
            names = os.listdir(dfd)
        except OSError:
            names = []
        for name in names:
            if not (name == "user_status.bin" or (name.startswith("user_status.") and name.endswith(".bin"))):
                continue
            try:
                ffd = os.open(name, os.O_RDONLY | O_NOFOLLOW | O_NONBLOCK, dir_fd=dfd)
            except OSError:
                continue
            try:
                st = os.fstat(ffd)
                if not stat.S_ISREG(st.st_mode) or st.st_size <= 0 or st.st_size > CAP:
                    os.close(ffd)
                    continue
                key = (st.st_dev, st.st_ino)
                if key in seen_dev_ino:
                    os.close(ffd)
                    continue
                seen_dev_ino.add(key)
                if best_mtime is None or st.st_mtime > best_mtime:
                    if best_fd >= 0:
                        os.close(best_fd)
                    best_mtime = st.st_mtime
                    best_fd = ffd
                else:
                    os.close(ffd)
            except OSError:
                os.close(ffd)
    finally:
        os.close(dfd)

if best_fd < 0:
    print("{}")
    sys.exit(0)

try:
    chunks = []
    total = 0
    while total <= CAP:
        chunk = os.read(best_fd, min(65536, CAP + 1 - total))
        if not chunk:
            break
        chunks.append(chunk)
        total += len(chunk)
finally:
    os.close(best_fd)

if total <= 0 or total > CAP:
    print("{}")
    sys.exit(0)

try:
    wrapper = json.loads(b"".join(chunks).decode("utf-8"))
    if not isinstance(wrapper, dict):
        print("{}")
        sys.exit(0)
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
                if pf == 1 and pw == 0:
                    out["teams_tier"] = int(pv)
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
// non-secret quota fields from Devin CLI's own user_status.*.bin cache. Runs
// python3 in isolated mode (-I: ignores PYTHONPATH, user site, and script dir)
// from a fixed safe cwd (/) so planted local modules on the host cannot run.
func devinQuotaReadCommand() string {
	script := `cd / || exit 1
command -v python3 >/dev/null 2>&1 || { echo '{}'; exit 0; }
exec python3 -I -c ` + sshx.QuoteShell(devinQuotaReadScript) + ` </dev/null 2>/dev/null`
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

// devinPlanName maps numeric PlanInfo.teams_tier enum values to display plan
// labels purely in Go so no arbitrary plan string ever leaves the host.
func devinPlanName(teamsTier int, isDevin bool) string {
	switch teamsTier {
	case 12:
		return "Devin Enterprise"
	case 14, 15:
		return "Devin Teams"
	case 16:
		return "Devin Pro"
	case 17:
		return "Devin Max"
	case 19:
		return "Devin Free"
	case 20:
		return "Devin Trial"
	case 1:
		if isDevin {
			return "Devin Teams"
		}
		return "Teams"
	case 2:
		if isDevin {
			return "Devin Pro"
		}
		return "Pro"
	case 3, 5, 10, 11:
		if isDevin {
			return "Devin Enterprise"
		}
		return "Enterprise"
	case 7:
		return "Teams Ultimate"
	case 8:
		return "Pro Ultimate"
	case 9:
		if isDevin {
			return "Devin Trial"
		}
		return "Trial"
	case 18:
		if isDevin {
			return "Devin Max"
		}
		return "Max"
	default:
		return ""
	}
}

// quotaOfDevinStatus parses the non-secret JSON emitted by devinQuotaReadCommand.
// Unknown/missing/garbled fields are ignored so partial payloads degrade safely.
func quotaOfDevinStatus(raw []byte, now time.Time) (q quotaInfo, accountKey, accountLabel string, ok bool) {
	var doc struct {
		FetchedAt              int64    `json:"fetched_at"`
		UserID                 string   `json:"user_id"`
		Email                  string   `json:"email"`
		TeamsTier              int      `json:"teams_tier"`
		IsDevin                bool     `json:"is_devin"`
		HideDailyQuota         bool     `json:"hide_daily_quota"`
		HideWeeklyQuota        bool     `json:"hide_weekly_quota"`
		HasDailyRemaining      bool     `json:"has_daily_remaining"`
		DailyRemainingPercent  int      `json:"daily_remaining_percent"`
		DailyResetAt           int64    `json:"daily_reset_at"`
		HasWeeklyRemaining     bool     `json:"has_weekly_remaining"`
		WeeklyRemainingPercent int      `json:"weekly_remaining_percent"`
		WeeklyResetAt          int64    `json:"weekly_reset_at"`
		PlanEnd                int64    `json:"plan_end"`
		ACUConsumed            *float64 `json:"acu_consumed"`
		ACULimit               *float64 `json:"acu_limit"`
		DollarsConsumed        *float64 `json:"dollars_consumed"`
		DollarsLimit           *float64 `json:"dollars_limit"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return quotaInfo{}, "", "", false
	}
	accountKey = cleanAccountKey(doc.UserID)
	accountLabel = cleanPlanName(doc.Email)
	q.Plan = devinPlanName(doc.TeamsTier, doc.IsDevin)
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

// opencodeQuotaCommand queries OpenCode's already-running background service
// via `opencode api experimental.usage.limits`. It never starts a standalone
// or background server: if `opencode service status` does not report a live
// URL, it exits 75 immediately. When coreutils `timeout` is available on the
// host, both commands are bounded (`timeout -k 5 30`) so a hung CLI is killed
// on the host rather than orphaned when SSH closes. Fixed script.
func opencodeQuotaCommand() string {
	script := `PATH="$HOME/.opencode/bin:$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
command -v opencode >/dev/null 2>&1 || exit 75
t=
if command -v timeout >/dev/null 2>&1 && timeout -k 1 1 true >/dev/null 2>&1; then
  t="timeout -k 5 30"
fi
st=$($t opencode service status </dev/null 2>/dev/null) || exit 75
case "$st" in
  http://*|https://*) exec $t opencode api experimental.usage.limits </dev/null 2>/dev/null ;;
  *) exit 75 ;;
esac`
	return "sh -c " + sshx.QuoteShell(script)
}

// opencodeWorkspaceKey hashes OpenCode's workspace.id into an opaque snapshot
// key ("ws:<16 hex>") so workspaces never collide or overwrite each other and
// the raw workspace ID never needs to be displayed.
func opencodeWorkspaceKey(wsID string) string {
	wsID = strings.TrimSpace(wsID)
	if wsID == "" || utf8.RuneCountInString(wsID) > 256 {
		return ""
	}
	for _, r := range wsID {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	sum := sha256.Sum256([]byte(wsID))
	return "ws:" + hex.EncodeToString(sum[:8])
}

func parseOpenCodeMeter(raw json.RawMessage) *quotaWindow {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		Percent  *float64 `json:"percent"`
		Limited  bool     `json:"limited"`
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
		Limited:  m.Limited,
		ResetsAt: resetSec,
	}
}

// quotaOfOpenCodeUsage parses `opencode api experimental.usage.limits` JSON
// output, keeping ONLY Go usage numbers (percent, limited flag, resetsAt for
// rolling/weekly/monthly) and deriving an opaque hashed workspace key from
// workspace.id. All other fields (including workspace.name, location, usageURL,
// budgets) are ignored. When workspace.id is missing, nothing is returned.
func quotaOfOpenCodeUsage(raw []byte, now time.Time) (q quotaInfo, wsKey string, ok bool) {
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
			ID string `json:"id"`
		}
		if json.Unmarshal(data.Workspace, &ws) == nil {
			wsKey = opencodeWorkspaceKey(ws.ID)
		}
	}
	if wsKey == "" {
		return quotaInfo{}, "", true
	}
	if len(data.Go) == 0 || string(data.Go) == "null" {
		return quotaInfo{}, wsKey, true
	}
	var goLim struct {
		Rolling json.RawMessage `json:"rolling"`
		Weekly  json.RawMessage `json:"weekly"`
		Monthly json.RawMessage `json:"monthly"`
	}
	if json.Unmarshal(data.Go, &goLim) != nil {
		return quotaInfo{}, wsKey, false
	}
	q.FiveHour = parseOpenCodeMeter(goLim.Rolling)
	q.Weekly = parseOpenCodeMeter(goLim.Weekly)
	q.Monthly = parseOpenCodeMeter(goLim.Monthly)
	q.Known = q.FiveHour != nil || q.Weekly != nil || q.Monthly != nil
	if q.Known {
		q.Plan = "OpenCode Go"
		q.UpdatedAt = now.Unix()
	}
	return q, wsKey, true
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

// fetchOpenCodeQuota executes OpenCode's own CLI command against an already
// running OpenCode service during a user-initiated manual Refresh and persists
// the parsed Go usage snapshot under the hashed workspace key ("ws:<16 hex>").
// Returns the resolved workspace account key on success.
func (s *Server) fetchOpenCodeQuota(ctx context.Context, sess *term.Session, expectedAccountKey string) (string, error) {
	if sess == nil || sess.State() != term.StateRunning {
		return "", errors.New("session not running")
	}
	res, err := sshx.Exec(ctx, sess.Client(), opencodeQuotaCommand(), opencodeQuotaMax, sshx.ExecOptions{})
	if err != nil {
		var exitErr *sshx.ExitError
		if errors.As(err, &exitErr) && exitErr.Status == opencodeNotRunningExitCode {
			return "", errOpenCodeNotRunning
		}
		return "", err
	}
	q, wsKey, ok := quotaOfOpenCodeUsage(res.Out, time.Now())
	if !ok || !q.Known || wsKey == "" {
		return "", errors.New("opencode go usage unavailable")
	}
	// Label the card by the host name the gateway already knows; never store workspace.name.
	label := strings.TrimSpace(sess.HostName)
	cooldownSec := int64(subscriptionRefreshCooldown / time.Second)

	if strings.HasPrefix(expectedAccountKey, "host:") {
		// Promote the unfetched host placeholder to the real workspace key.
		s.recordSubscriptionSnapshot(sess.OwnerID, "opencode", wsKey, label, sess.HostID, sess.HostName, q)
		if s.store != nil {
			rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _, _, _ = s.store.TryClaimSubscriptionRefresh(rctx, sess.OwnerID, "opencode", wsKey, cooldownSec)
			_ = s.store.DeleteSubscriptionSnapshot(rctx, sess.OwnerID, "opencode", expectedAccountKey)
			rcancel()
		}
		return wsKey, nil
	}
	if wsKey != expectedAccountKey {
		// Host switched to a different workspace: never overwrite expectedAccountKey.
		// Record the active workspace under its own key and report account_changed.
		s.recordSubscriptionSnapshot(sess.OwnerID, "opencode", wsKey, label, sess.HostID, sess.HostName, q)
		if s.store != nil {
			rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _, _, _ = s.store.TryClaimSubscriptionRefresh(rctx, sess.OwnerID, "opencode", wsKey, cooldownSec)
			rcancel()
		}
		return "", errSubscriptionAccountChanged
	}
	s.recordSubscriptionSnapshot(sess.OwnerID, "opencode", wsKey, label, sess.HostID, sess.HostName, q)
	return wsKey, nil
}
