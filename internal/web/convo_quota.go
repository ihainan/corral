package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"time"

	"github.com/ihainan/corral/internal/sftplite"
)

// The subscription's quota (5-hour and weekly use) of a Claude Code
// session, as the cc-statusline status line program keeps it on the host:
// ~/.claude/cc-statusline/usage-<session>.jsonl, a line each time Claude
// Code redraws its status line (Claude Code hands status line programs
// rate_limits; its log has none). Only that file is read, only what is
// there: nothing is installed or set (plans/3c-quota.md). The quota is the
// account's, whichever of its sessions says it.

const (
	quotaTail  = 4 << 10 // bytes read from the file's end: its last line is in them
	quotaLimit = 15 * time.Second
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

// quotaOf reads the last whole line of the file's end (tail; whole: it is
// the file from its start, else its first line may be a piece of one), as
// cc-statusline writes it; Known false where it says nothing usable. A line
// still being written (no newline yet) is passed over for the one before.
// (A record longer than quotaTail is not read: none is near that long.)
func quotaOf(tail []byte, whole bool) quotaInfo {
	if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
		tail = tail[:i] // (what follows the last newline: not written whole yet)
	} else {
		return quotaInfo{}
	}
	tail = bytes.TrimRight(tail, "\r")
	if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	} else if !whole {
		return quotaInfo{} // (the first line read, begun before what was read)
	}
	// Each part on its own: one of a kind not looked for (a string, say)
	// leaves that part out, not the rest.
	var rec map[string]json.RawMessage
	if json.Unmarshal(tail, &rec) != nil {
		return quotaInfo{}
	}
	num := func(raw json.RawMessage) (float64, bool) {
		var f float64
		return f, raw != nil && json.Unmarshal(raw, &f) == nil && !math.IsNaN(f)
	}
	window := func(raw json.RawMessage) *quotaWindow {
		var w map[string]json.RawMessage
		if json.Unmarshal(raw, &w) != nil {
			return nil
		}
		u, ok1 := num(w["used_percentage"])
		r, ok2 := num(w["resets_at_epoch"])
		if !ok1 || !ok2 || u < 0 || u > 100 || r <= 0 || r > 1e12 {
			return nil
		}
		return &quotaWindow{Used: u, ResetsAt: int64(r)}
	}
	q := quotaInfo{FiveHour: window(rec["five_hour"]), Weekly: window(rec["weekly"])}
	if u, ok := num(rec["updated_at"]); ok && u > 0 && u < 1e12 {
		q.UpdatedAt = int64(u)
	}
	q.Known = q.FiveHour != nil || q.Weekly != nil
	return q
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
	err := s.withSFTP(r.Context(), sess, quotaLimit, func(c *sftplite.Client) error {
		home, err := c.RealPath(".")
		if err != nil {
			return err
		}
		// (Each step a folder, the last a plain file, none a link: a link is
		// no record of this account's. Looked at, then read: not at once, so
		// a file put in its place between the two by the account's own
		// programs is read as it is then; only its own home is reached.)
		p := home
		for i, part := range []string{".claude", "cc-statusline", "usage-" + sid + ".jsonl"} {
			p += "/" + part
			fi, err := c.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			last := i == 2
			if fi.Mode()&fs.ModeSymlink != 0 || !last && !fi.IsDir() || last && !fi.Mode().IsRegular() {
				return nil
			}
			if last {
				off := max(0, fi.Size()-quotaTail)
				b, err := c.ReadRange(p, off, int(fi.Size()-off))
				if err != nil {
					return err
				}
				q = quotaOf(b, off == 0)
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}
