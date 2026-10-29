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

// quotaOf reads the last whole line of the file's end (tail), as
// cc-statusline writes it; Known false where it says nothing usable.
func quotaOf(tail []byte) quotaInfo {
	tail = bytes.TrimRight(tail, "\r\n")
	if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	// (Numbers only: a string, or a field of another kind, makes it nothing.)
	type win struct {
		Used     *float64 `json:"used_percentage"`
		ResetsAt *float64 `json:"resets_at_epoch"`
	}
	var rec struct {
		UpdatedAt *float64 `json:"updated_at"`
		FiveHour  *win     `json:"five_hour"`
		Weekly    *win     `json:"weekly"`
	}
	if json.Unmarshal(tail, &rec) != nil {
		return quotaInfo{}
	}
	window := func(w *win) *quotaWindow {
		if w == nil || w.Used == nil || w.ResetsAt == nil || math.IsNaN(*w.Used) || *w.Used < 0 || *w.Used > 100 || *w.ResetsAt <= 0 || *w.ResetsAt > 1e12 {
			return nil
		}
		return &quotaWindow{Used: *w.Used, ResetsAt: int64(*w.ResetsAt)}
	}
	q := quotaInfo{}
	q.FiveHour, q.Weekly = window(rec.FiveHour), window(rec.Weekly)
	if rec.UpdatedAt != nil && *rec.UpdatedAt > 0 && *rec.UpdatedAt < 1e12 {
		q.UpdatedAt = int64(*rec.UpdatedAt)
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
		// no record of this account's.)
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
				q = quotaOf(b)
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
