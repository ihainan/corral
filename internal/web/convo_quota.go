package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
)

// The subscription's quota (5-hour and weekly use) of a host's Claude Code, as
// Claude Code itself keeps it in ~/.claude.json (cachedUsageUtilization, the
// account's, with when it was fetched). Its log has none. Nothing else is read:
// not its sign-in, which Anthropic's terms keep to its own programs; so no call
// to its usage endpoint is made from here.
//
// That record is new only when something asks Claude Code: so, where it has grown
// old and someone is looking, its own binary is run, unmodified, to answer its own
// /usage (claude -p /usage): no model is asked (cost 0, no turn: seen on 2.1.318),
// no tools, settings or session; it fetches, as it does for the panel, and keeps
// that. At most once in quotaEvery for an account on a host, however many look,
// and not where it is not signed in with a subscription. See plans/3c-quota.md.

const (
	quotaFileMax = 16 << 20 // ~/.claude.json, read whole; more is not read
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

// quotaOfConfig reads Claude Code's config: the quota it keeps (Known false where
// there is none usable), and whether it is signed in to an account at all. A record
// of another account than the one signed in now says nothing of this one (Claude
// Code drops it too); a window whose reset has passed, nothing of now (as it does).
func quotaOfConfig(raw []byte, now time.Time) (q quotaInfo, signedIn bool) {
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
		return quotaInfo{}, false
	}
	signedIn = c.Account != nil && c.Account.UUID != ""
	if !signedIn || c.Cached == nil || c.Cached.AccountUUID != c.Account.UUID || c.Cached.Utilization == nil {
		return quotaInfo{}, signedIn
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
	return q, signedIn
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
// servers, settings or session; nothing of what it prints is read. Fixed script.
func quotaRefreshCommand() string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
command -v claude >/dev/null 2>&1 || exit 127
cd "$HOME" || exit 1
unset CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 exec nice claude -p /usage --output-format json --tools "" --strict-mcp-config --setting-sources "" --no-session-persistence </dev/null >/dev/null 2>&1`
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
	var q quotaInfo
	var signedIn bool
	var home string
	now := time.Now()
	err := s.withSFTP(r.Context(), sess, quotaLimit, func(c *sftplite.Client) error {
		var err error
		if home, err = c.RealPath("."); err != nil {
			return err
		}
		// (A plain file, not a link: a link is no config of this account's.)
		p := home + "/.claude.json"
		fi, err := c.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() || fi.Size() > quotaFileMax {
			return nil
		}
		b, err := c.ReadRange(p, 0, int(fi.Size()))
		if err != nil {
			return err
		}
		q, signedIn = quotaOfConfig(b, now)
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeFileError(w, err)
		return
	}
	// Grown old, or none yet: asked for afresh, for the next look. (Not waited for.)
	old := !q.Known || now.Unix()-q.UpdatedAt >= int64(quotaEvery/time.Second)
	if signedIn && old && s.quotaRuns.due(fmt.Sprintf("%d\x00%d\x00%s", sess.OwnerID, sess.HostID, home), now) {
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
