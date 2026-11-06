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
	Used     float64 `json:"used"`      // percent
	ResetsAt int64   `json:"resets_at"` // unix seconds
}

type quotaInfo struct {
	Known     bool         `json:"known"`
	UpdatedAt int64        `json:"updated_at,omitempty"` // unix seconds
	FiveHour  *quotaWindow `json:"five_hour,omitempty"`
	Weekly    *quotaWindow `json:"weekly,omitempty"`
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

// GET /api/sessions/{id}/conversation/quota?session=<Claude Code session id>
// → quotaInfo ({"known": false} where the host has no such record).
func (s *Server) apiConversationQuota(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	sid := r.URL.Query().Get("session")
	if !convoUUID.MatchString(sid) {
		writeError(w, http.StatusBadRequest, "bad_request", "a Claude Code session id is required")
		return
	}
	now := time.Now()
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
