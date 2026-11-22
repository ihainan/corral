package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// The subscription's quota (5-hour and weekly use) of a host's Claude Code, as
// Claude Code itself keeps it in ~/.claude.json (cachedUsageUtilization, the
// account's, with when it was fetched). Its log has none. Only that leaves the host:
// not the rest of the file, not its sign-in, which Anthropic's terms keep to its own
// programs; so no call to its usage endpoint is made from here.
//
// That record is new only when something asks Claude Code: so, where it has grown
// old and someone is looking, its own binary is run, unmodified, to answer its own
// /usage (claude -p /usage): no model is asked (cost 0, no turn: seen on 2.1.318),
// no tools, settings or session; it fetches, as it does for the panel, and keeps
// that. At most once in quotaEvery for an account on a host, however many look,
// on the host, and not where it is not signed in with an account. See plans/3c-quota.md.

const (
	quotaReadMax = 256 << 10 // what quotaReadCommand may print: a few windows
	quotaLimit   = 15 * time.Second
	quotaEvery   = 15 * time.Minute
	quotaRunMax  = 90 * time.Second // (its look over the sessions kept here takes a while)
)

type quotaWindow struct {
	Used     float64 `json:"used"`      // percent (0..100)
	ResetsAt int64   `json:"resets_at"` // unix seconds
}

type quotaGroup struct {
	Name     string       `json:"name"`
	FiveHour *quotaWindow `json:"five_hour,omitempty"`
	Weekly   *quotaWindow `json:"weekly,omitempty"`
}

type quotaInfo struct {
	Known     bool         `json:"known"`
	UpdatedAt int64        `json:"updated_at,omitempty"` // unix seconds
	FiveHour  *quotaWindow `json:"five_hour,omitempty"`
	Weekly    *quotaWindow `json:"weekly,omitempty"`
	Groups    []quotaGroup `json:"groups,omitempty"` // per-model-group quota (agy)
}

// quotaOfConfig reads what quotaReadCommand printed: the quota kept (Known false where
// there is none usable), and whether it is signed in to an account at all (ok false:
// not read). A record
// of another account than the one signed in now says nothing of this one (Claude
// Code drops it too); a window whose reset has passed, nothing of now (as it does).
func quotaOfConfig(raw []byte, now time.Time) (q quotaInfo, signedIn, ok bool) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var c struct {
		Account *struct {
			UUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
		Cached *struct {
			FetchedAtMs float64 `json:"fetchedAtMs"`
			AccountUUID string  `json:"accountUuid"`
			Utilization *struct {
				FiveHour *window `json:"five_hour"`
				SevenDay *window `json:"seven_day"`
			} `json:"utilization"`
		} `json:"cachedUsageUtilization"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return quotaInfo{}, false, false
	}
	signedIn = c.Account != nil && c.Account.UUID != ""
	if !signedIn || c.Cached == nil || c.Cached.AccountUUID != c.Account.UUID || c.Cached.Utilization == nil {
		return quotaInfo{}, signedIn, true
	}
	of := func(w *window) *quotaWindow {
		if w == nil || w.Utilization == nil || *w.Utilization < 0 || *w.Utilization > 100 || math.IsNaN(*w.Utilization) {
			return nil
		}
		at, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
		if err != nil || !at.After(now) {
			return nil
		}
		return &quotaWindow{Used: *w.Utilization, ResetsAt: at.Unix()}
	}
	q.FiveHour, q.Weekly = of(c.Cached.Utilization.FiveHour), of(c.Cached.Utilization.SevenDay)
	if ms := c.Cached.FetchedAtMs; ms > 0 && ms < 1e15 {
		q.UpdatedAt = int64(ms / 1000)
	}
	q.Known = q.FiveHour != nil || q.Weekly != nil
	return q, signedIn, true
}

// quotaRuns says when claude -p /usage was last set going, for each account on a host.
type quotaRuns struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due: whether to run it now, and if so it is taken to have been: failed or not,
// not again till quotaEvery has passed.
func (q *quotaRuns) due(key string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, ok := q.last[key]; ok && now.Sub(t) < quotaEvery {
		return false
	}
	if q.last == nil {
		q.last = map[string]time.Time{}
	}
	for k, t := range q.last {
		if now.Sub(t) >= quotaEvery {
			delete(q.last, k)
		}
	}
	q.last[key] = now
	return true
}

// quotaRefreshCommand has Claude Code answer its own /usage, as in its terminal,
// which keeps what it fetched in its config. Nothing else of it is used: no tools,
// servers, settings or session; nothing of what it prints is read. The limit is
// kept on the host too (a file's time, ~/.cache/corral): a gateway restarted, or the
// same account reached by another saved host or user of Corral, does not go round
// it. Fixed script.
func quotaRefreshCommand() string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
command -v claude >/dev/null 2>&1 || exit 127
cd "$HOME" || exit 1
mkdir -p .cache/corral 2>/dev/null
s=.cache/corral/usage-asked
[ -n "$(find "$s" -mmin -` + strconv.Itoa(int(quotaEvery/time.Minute)) + ` 2>/dev/null)" ] && exit 0
touch "$s" 2>/dev/null
unset CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
n=; command -v nice >/dev/null 2>&1 && n=nice
DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 exec $n claude -p /usage --output-format json --tools "" --strict-mcp-config --setting-sources "" --no-session-persistence </dev/null >/dev/null 2>&1`
	return "sh -c " + sshx.QuoteShell(script)
}

// quotaReadCommand prints, of Claude Code's config (a plain file, not a link), only
// the account's id and the quota it keeps, as a JSON object: all else in that file
// stays on the host, keys of servers it may hold above all. It is written with two
// spaces to a level, its top level's keys so at a line's start; written otherwise,
// nothing is found, and no quota is said. A block not ended: exit 3, not read whole.
// Fixed script.
func quotaReadCommand() string {
	script := `f=$HOME/.claude.json
[ -f "$f" ] && [ ! -L "$f" ] || { echo '{}'; exit 0; }
awk '
/^  "oauthAccount": \{/ { a = 1; next }
a && /^  }/ { a = 0; next }
a && /^    "accountUuid": / { u = $0; sub(/,[ \t\r]*$/, "", u); next }
/^  "cachedUsageUtilization": \{/ { c = 1; b = ""; next }
c && /^  }/ { c = 0; got = 1; next }
c { b = b $0 "\n"; next }
END {
  if (a || c) exit 3
  printf "{"
  if (u != "") printf "\"oauthAccount\":{%s}", u
  if (got) { if (u != "") printf ","; printf "\"cachedUsageUtilization\":{%s}", b }
  print "}"
}' "$f"`
	return "sh -c " + sshx.QuoteShell(script)
}

const (
	agyQuotaFresh       = 3 * time.Minute  // served directly without triggering background refresh
	agyQuotaStaleMax    = 60 * time.Minute // served immediately while refreshing asynchronously
	agyQuotaPollEvery   = 5 * time.Minute  // periodic background refresh while agy panes are active
	agyQuotaActiveTTL   = 20 * time.Minute // stop background loop once no agy pane/viewer is active
	agyQuotaFetchLimit  = 25 * time.Second
	agyQuotaFailBackoff = 90 * time.Second
)

type agyQuotaEntry struct {
	q  quotaInfo
	at time.Time
}

type agyQuotaFlight struct {
	done chan struct{}
	q    quotaInfo
	err  error
}

type agyQuotaHost struct {
	sess   *term.Session
	seenAt time.Time
	failed time.Time
	loopOn bool
}

// agyQuotaPoller caches Antigravity/Gemini subscription quota per (owner, host),
// pre-warms it in the background whenever an agy session is active, refreshes
// periodically, and serves stale-while-revalidate so opening the quota popover
// is instant instead of blocking 3-8s on `agy -p /usage`.
type agyQuotaPoller struct {
	mu     sync.Mutex
	cache  map[string]agyQuotaEntry
	flight map[string]*agyQuotaFlight
	hosts  map[string]*agyQuotaHost
	nowFn  func() time.Time
}

func (p *agyQuotaPoller) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

func (p *agyQuotaPoller) getCached(key string) (agyQuotaEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ent, ok := p.cache[key]
	return ent, ok
}

// startFetch ensures at most one in-flight fetch per key (singleflight) and
// returns the flight handle.
func (p *agyQuotaPoller) startFetch(key string, fetch func(context.Context) (quotaInfo, error)) (*agyQuotaFlight, bool) {
	p.mu.Lock()
	if p.flight == nil {
		p.flight = map[string]*agyQuotaFlight{}
	}
	if fl, ok := p.flight[key]; ok {
		p.mu.Unlock()
		return fl, false
	}
	fl := &agyQuotaFlight{done: make(chan struct{})}
	p.flight[key] = fl
	p.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), agyQuotaFetchLimit)
		q, err := fetch(ctx)
		cancel()
		now := p.now()
		p.mu.Lock()
		if err == nil && q.Known {
			if p.cache == nil {
				p.cache = map[string]agyQuotaEntry{}
			}
			p.cache[key] = agyQuotaEntry{q: q, at: now}
			if h := p.hosts[key]; h != nil {
				h.failed = time.Time{}
			}
		} else if h := p.hosts[key]; h != nil {
			h.failed = now
		}
		delete(p.flight, key)
		fl.q, fl.err = q, err
		close(fl.done)
		p.mu.Unlock()
	}()
	return fl, true
}

// getOrRefresh returns cached quota immediately when available (triggering an
// async background refresh if stale), or joins/starts a singleflight fetch on
// cold miss.
func (p *agyQuotaPoller) getOrRefresh(ctx context.Context, key string, fetch func(context.Context) (quotaInfo, error)) (quotaInfo, error) {
	now := p.now()
	if ent, ok := p.getCached(key); ok && now.Sub(ent.at) < agyQuotaStaleMax {
		if now.Sub(ent.at) >= agyQuotaFresh {
			p.startFetch(key, fetch)
		}
		return ent.q, nil
	}
	fl, _ := p.startFetch(key, fetch)
	select {
	case <-fl.done:
		return fl.q, fl.err
	case <-ctx.Done():
		return quotaInfo{}, ctx.Err()
	}
}

// warm registers an active session hosting agy, triggers an immediate
// background pre-warm if the cache is missing/stale, and keeps a lightweight
// periodic refresher running while the session stays active.
func (p *agyQuotaPoller) warm(key string, sess *term.Session, fetch func(context.Context, *term.Session) (quotaInfo, error)) {
	if sess == nil || sess.State() != term.StateRunning {
		return
	}
	now := p.now()
	p.mu.Lock()
	if p.hosts == nil {
		p.hosts = map[string]*agyQuotaHost{}
	}
	h := p.hosts[key]
	if h == nil {
		h = &agyQuotaHost{}
		p.hosts[key] = h
	}
	h.sess = sess
	h.seenAt = now
	ent, hasCache := p.cache[key]
	needNow := (!hasCache || now.Sub(ent.at) >= agyQuotaFresh) && (h.failed.IsZero() || now.Sub(h.failed) >= agyQuotaFailBackoff)
	startLoop := !h.loopOn
	if startLoop {
		h.loopOn = true
	}
	p.mu.Unlock()

	if needNow {
		p.startFetch(key, func(ctx context.Context) (quotaInfo, error) {
			return fetch(ctx, sess)
		})
	}
	if !startLoop {
		return
	}
	go func() {
		ticker := time.NewTicker(agyQuotaPollEvery)
		defer ticker.Stop()
		for range ticker.C {
			now := p.now()
			p.mu.Lock()
			h := p.hosts[key]
			if h == nil || h.sess == nil || h.sess.State() != term.StateRunning || now.Sub(h.seenAt) > agyQuotaActiveTTL {
				if h != nil {
					h.loopOn = false
					delete(p.hosts, key)
				}
				p.mu.Unlock()
				return
			}
			curSess := h.sess
			ent, hasCache := p.cache[key]
			due := !hasCache || now.Sub(ent.at) >= agyQuotaFresh
			p.mu.Unlock()
			if due {
				p.startFetch(key, func(ctx context.Context) (quotaInfo, error) {
					return fetch(ctx, curSess)
				})
			}
		}
	}()
}

func (s *Server) fetchAgyQuota(ctx context.Context, sess *term.Session) (quotaInfo, error) {
	if sess == nil || sess.State() != term.StateRunning {
		return quotaInfo{}, errors.New("session not running")
	}
	res, err := sshx.Exec(ctx, sess.Client(), agyQuotaCommand(), quotaReadMax, sshx.ExecOptions{AwaitSlot: 12 * time.Second})
	if err != nil {
		return quotaInfo{}, err
	}
	q, ok := quotaOfAgyUsage(res.Out, time.Now())
	if !ok {
		return quotaInfo{}, errors.New("quota parse failed")
	}
	return q, nil
}

// warmAgyQuota pre-warms and periodically refreshes Antigravity/Gemini quota
// for the host of sess whenever active agy panes are detected.
func (s *Server) warmAgyQuota(sess *term.Session) {
	if sess == nil || sess.State() != term.StateRunning {
		return
	}
	key := fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID)
	s.agyQuota.warm(key, sess, s.fetchAgyQuota)
}

// agyQuotaCommand runs `agy -p /usage --output-format json`, which prints
// Antigravity's subscription quota buckets as JSON without starting a turn or
// spending any tokens. Fixed script.
func agyQuotaCommand() string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
command -v agy >/dev/null 2>&1 || exit 127
exec agy -p /usage --output-format json </dev/null 2>/dev/null`
	return "sh -c " + sshx.QuoteShell(script)
}

// quotaOfAgyUsage parses the JSON output of `agy -p /usage --output-format json`.
func quotaOfAgyUsage(raw []byte, now time.Time) (quotaInfo, bool) {
	var doc struct {
		Status  string `json:"status"`
		Command *struct {
			Name string `json:"name"`
			Data *struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						Window            string   `json:"window"`
						RemainingFraction *float64 `json:"remaining_fraction"`
						ResetTime         string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Command == nil || doc.Command.Data == nil {
		return quotaInfo{}, false
	}
	var q quotaInfo
	for _, g := range doc.Command.Data.Groups {
		name := g.Name
		if name == "" {
			continue
		}
		var qg quotaGroup
		qg.Name = name
		for _, b := range g.Buckets {
			if b.RemainingFraction == nil || math.IsNaN(*b.RemainingFraction) {
				continue
			}
			rem := *b.RemainingFraction
			if rem < 0 {
				rem = 0
			} else if rem > 1 {
				rem = 1
			}
			used := math.Round((1-rem)*1000) / 10
			var resetSec int64
			if b.ResetTime != "" {
				if at, err := time.Parse(time.RFC3339Nano, b.ResetTime); err == nil {
					resetSec = at.Unix()
				}
			}
			w := &quotaWindow{Used: used, ResetsAt: resetSec}
			switch b.Window {
			case "5h":
				qg.FiveHour = w
			case "weekly":
				qg.Weekly = w
			}
		}
		if qg.FiveHour != nil || qg.Weekly != nil {
			q.Groups = append(q.Groups, qg)
		}
	}
	if len(q.Groups) > 0 {
		q.Known = true
		q.UpdatedAt = now.Unix()
	}
	return q, true
}

// GET /api/sessions/{id}/conversation/quota?session=<session id>[&agent=agy]
// → quotaInfo ({"known": false} where the host has no such record).
func (s *Server) apiConversationQuota(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	sid := r.URL.Query().Get("session")
	if !convoUUID.MatchString(sid) {
		writeError(w, http.StatusBadRequest, "bad_request", "a session id is required")
		return
	}
	agent := r.URL.Query().Get("agent")
	now := time.Now()
	if agent == "agy" {
		cacheKey := fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID)
		s.warmAgyQuota(sess)
		ctx, cancel := context.WithTimeout(r.Context(), agyQuotaFetchLimit)
		defer cancel()
		q, err := s.agyQuota.getOrRefresh(ctx, cacheKey, func(fctx context.Context) (quotaInfo, error) {
			return s.fetchAgyQuota(fctx, sess)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			writeError(w, http.StatusBadGateway, "no_quota", "Antigravity's quota could not be read")
			return
		}
		writeJSON(w, http.StatusOK, q)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), quotaLimit)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), quotaReadCommand(), quotaReadMax, sshx.ExecOptions{})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeError(w, http.StatusBadGateway, "no_quota", "Claude Code's record could not be read")
		return
	}
	q, signedIn, ok := quotaOfConfig(res.Out, now)
	if !ok {
		// (Not read whole, say: not that there is none. The page keeps what it had.)
		writeError(w, http.StatusBadGateway, "no_quota", "Claude Code's record could not be read")
		return
	}
	// Grown old, or none yet: asked for afresh, for the next look. (Not waited for.)
	old := !q.Known || now.Unix()-q.UpdatedAt >= int64(quotaEvery/time.Second)
	if signedIn && old && s.quotaRuns.due(fmt.Sprintf("%d\x00%d", sess.OwnerID, sess.HostID), now) {
		actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		s.audit(r.WithContext(actx), "convo.quota_refresh", sess.HostName, map[string]any{"session": sess.ID})
		acancel()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), quotaRunMax)
			defer cancel()
			_, _ = sshx.Exec(ctx, sess.Client(), quotaRefreshCommand(), 4<<10, sshx.ExecOptions{})
		}()
	}
	writeJSON(w, http.StatusOK, q)
}
