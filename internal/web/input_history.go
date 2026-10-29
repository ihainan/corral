package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sftplite"
)

// The person's input history on the host: Claude Code's
// ~/.claude/history.jsonl (one line per message sent, appended to), read
// backwards a page at a time, searched as it is read.
//
// A page is whole lines: the cursor is always the start of a line, and what
// is read next is the line that ends at it (its newline at cursor−1). A block
// is read before the cursor, one byte longer than histBlock so that a line of
// just histBlock bytes still shows the newline before it; the lines whose
// both ends are in the block are taken, the latest first, and the piece at
// the block's front, a line begun before it, is left for the next block. A
// line longer than a block is not read: its start is looked for (skip
// "line"), and it is counted in skipped once found. The end of a file that
// has no newline yet, a line still being written, is passed over the same
// way (skip "tail") and not counted. Every limit stops between two lines (or
// partway through a skip, which goes on next time), so a page leaves out
// nothing and the next goes on where it ended.
const (
	histBlock     = 256 << 10
	histScanMax   = 4 * histBlock // read per request (the byte before each block not counted)
	histItemsMax  = 50
	histBytesMax  = 256 << 10 // the items' JSON per response
	histPreview   = 300       // characters
	histTextMax   = 16 << 10  // bytes: longer is shown, not put back in the composer
	histProjMax   = 512
	histFPBytes   = 64
	histTimeLimit = 8 * time.Second
	histSafety    = 60 * time.Second // the channel's last resort (a host that never answers)
)

type histItem struct {
	Preview string `json:"preview"`
	Text    string `json:"text,omitempty"`
	TextCut bool   `json:"text_cut,omitempty"`
	TS      int64  `json:"ts,omitempty"` // (unix ms)
	Project string `json:"project,omitempty"`
	Pasted  bool   `json:"pasted,omitempty"`
}

// histState is where a scan is: cursor, a line start (or, skipping, how far
// back the start of the line skipped has been looked for), and the skip.
type histState struct {
	Cursor int64
	Skip   string // "", "line" or "tail"
}

// histPage is what a scan found up to a point it may stop at.
type histPage struct {
	Items   []histItem
	Bytes   int
	Skipped int
	At      histState
	Done    bool // (the file's start reached)
}

var pastedRef = regexp.MustCompile(`\[Pasted text #\d+`)

// histItemOf reads one line of the history; ok false: not one of its
// records, or not what was searched for (q, lower case).
func histItemOf(line []byte, q string) (histItem, bool) {
	var rec struct {
		Display   string `json:"display"`
		Timestamp int64  `json:"timestamp"`
		Project   string `json:"project"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Display == "" {
		return histItem{}, false
	}
	if q != "" && !strings.Contains(strings.ToLower(rec.Display), q) {
		return histItem{}, false
	}
	it := histItem{Preview: clipRunes(rec.Display, histPreview), TS: rec.Timestamp, Project: clipBytes(rec.Project, histProjMax), Pasted: pastedRef.MatchString(rec.Display)}
	if len(rec.Display) <= histTextMax {
		it.Text = rec.Display
	} else {
		it.TextCut = true
	}
	return it, true
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// histScan reads the history backwards from st (read: a range of the file;
// stop: whether to stop before the next read), taking what matches q (lower
// case) into page; commit is told each point it may stop at past st, the
// page as it is there, with the block in hand (blk, read from blkAt).
func histScan(read func(off int64, n int) ([]byte, error), st histState, q string, stop func() bool, commit func(pg histPage, blk []byte, blkAt int64)) error {
	page := histPage{At: st}
	scanned := 0
	for !page.Done {
		if scanned >= histScanMax || stop() {
			return nil
		}
		c := page.At.Cursor
		if c <= 0 {
			page.Done = true
			commit(page, nil, 0)
			return nil
		}
		// (And histFPBytes more in front, unscanned: what a cursor in the
		// block is bound to is in hand, wherever in it the cursor is.)
		from := max(0, c-histBlock-1)
		pre := max(0, from-histFPBytes)
		all, err := read(pre, int(c-pre))
		if err != nil {
			return err
		}
		if int64(len(all)) != c-pre {
			return errHistChanged // (shorter than it was)
		}
		b := all[from-pre:]
		scanned += len(b) - 1
		if page.At.Skip != "" {
			// The start of the line skipped: after the last newline before c.
			i := bytes.LastIndexByte(b, '\n')
			if i < 0 && from > 0 {
				page.At.Cursor = from
				commit(page, all, pre)
				continue
			}
			if page.At.Skip == "line" {
				page.Skipped++
			}
			page.At = histState{Cursor: from + int64(i) + 1}
			page.Done = page.At.Cursor == 0
			commit(page, all, pre)
			continue
		}
		if b[len(b)-1] != '\n' {
			return errHistChanged // (the cursor is no line's start: another file)
		}
		body := b[:len(b)-1]
		first := bytes.IndexByte(body, '\n')
		if first < 0 && from > 0 {
			// The line ending at c is longer than a block.
			page.At = histState{Cursor: from, Skip: "line"}
			commit(page, all, pre)
			continue
		}
		// Lines from the end: each from after a newline (or the file's
		// start) to the next newline.
		end := len(body)
		for {
			i := bytes.LastIndexByte(body[:end], '\n')
			if i < 0 && from > 0 {
				break // (the piece in front: the next block's)
			}
			start := i + 1
			line := body[start:end]
			if it, ok := histItemOf(line, q); ok {
				n := histItemSize(it)
				if len(page.Items) >= histItemsMax || page.Bytes+n > histBytesMax {
					return nil // (full: the next page begins at this line)
				}
				page.Items = append(page.Items, it)
				page.Bytes += n
			}
			page.At = histState{Cursor: from + int64(start)}
			if page.At.Cursor == 0 {
				page.Done = true
			}
			commit(page, all, pre)
			if page.Done || len(page.Items) >= histItemsMax || stop() {
				return nil // (full, or to stop: here, between lines)
			}
			if i < 0 {
				break
			}
			end = i
		}
	}
	return nil
}

func histItemSize(it histItem) int {
	b, _ := json.Marshal(it)
	return len(b) + 1
}

var errHistChanged = errors.New("the history file changed")

// histFP is what a cursor is bound to: the file's first bytes and those
// before the cursor (see the plan's 3b: a history only added to keeps both).
func histFP(head, before []byte) string {
	h := sha256.New()
	h.Write(head)
	h.Write([]byte{0})
	h.Write(before)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// histLocks: one reading of a session's history at a time.
type histLocks struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (h *histLocks) of(id string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]chan struct{}{}
	}
	c := h.m[id]
	if c == nil {
		c = make(chan struct{}, 1)
		h.m[id] = c
	}
	return c
}

// GET /api/sessions/{id}/input-history?q=&cursor=&fp=&skip=
func (s *Server) apiInputHistory(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	qs := r.URL.Query()
	q := strings.ToLower(qs.Get("q"))
	if len(q) > 500 {
		writeError(w, http.StatusBadRequest, "bad_request", "the search is too long")
		return
	}
	st := histState{Cursor: -1, Skip: qs.Get("skip")}
	if v := qs.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "a bad cursor")
			return
		}
		st.Cursor = n
	}
	fp := qs.Get("fp")
	if st.Skip != "" && st.Skip != "line" && st.Skip != "tail" || st.Cursor >= 0 && len(fp) != 16 || st.Cursor < 0 && (fp != "" || st.Skip != "") {
		writeError(w, http.StatusBadRequest, "bad_request", "a bad cursor")
		return
	}
	deadline := time.Now().Add(histTimeLimit)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	lock := s.histLocks.of(sess.ID)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		if r.Context().Err() == nil {
			writeError(w, http.StatusServiceUnavailable, "history_busy", "the history is still being read for an earlier request")
		}
		return
	}
	// The scan goes on its own, holding the lock until its read in hand
	// returns; what is answered is what it had by the deadline.
	var (
		mu      sync.Mutex
		last    *histPage // (the last point it may stop at; none: no way past where it began)
		lastFP  string
		failure error
		absent  bool
		done    = make(chan struct{})
	)
	go func() {
		defer func() { <-lock }()
		defer close(done)
		err := s.withSFTP(context.WithoutCancel(ctx), sess, histSafety, func(c *sftplite.Client) error {
			home, err := c.RealPath(".")
			if err != nil {
				return err
			}
			p := home + "/.claude"
			for i, want := range []bool{true, false} {
				if i == 1 {
					p += "/history.jsonl"
				}
				fi, err := c.Lstat(p)
				if errors.Is(err, fs.ErrNotExist) {
					mu.Lock()
					absent = true
					mu.Unlock()
					return nil
				}
				if err != nil {
					return err
				}
				if fi.Mode()&fs.ModeSymlink != 0 || want && !fi.IsDir() || !want && !fi.Mode().IsRegular() {
					mu.Lock()
					absent = true // (not followed: a link, or not a plain file, is no history)
					mu.Unlock()
					return nil
				}
				if !want {
					size := fi.Size()
					if st.Cursor < 0 {
						st.Cursor = size
						if size > 0 {
							st.Skip = "tail" // (unless it ends in a newline: looked at below)
						}
					}
					if st.Cursor > size {
						return errHistChanged
					}
				}
			}
			if ctx.Err() != nil {
				return nil
			}
			read := func(off int64, n int) ([]byte, error) { return c.ReadRange(p, off, n) }
			head, err := read(0, histFPBytes)
			if err != nil {
				return err
			}
			// (The bytes before a cursor: from the block in hand where they
			// are in it, else read.)
			fpOf := func(at int64, blk []byte, blkAt int64) (string, error) {
				from := max(0, at-histFPBytes)
				if blk != nil && from >= blkAt && at <= blkAt+int64(len(blk)) {
					return histFP(head, blk[from-blkAt:at-blkAt]), nil
				}
				b, err := read(from, int(at-from))
				if err != nil {
					return "", err
				}
				return histFP(head, b), nil
			}
			if fp != "" {
				got, err := fpOf(st.Cursor, nil, 0)
				if err != nil {
					return err
				}
				if got != fp {
					return errHistChanged
				}
			} else if st.Skip == "tail" {
				if b, err := read(st.Cursor-1, 1); err == nil && len(b) == 1 && b[0] == '\n' {
					st.Skip = "" // (it ends in a newline: no line being written)
				}
			}
			return histScan(read, st, q, func() bool { return ctx.Err() != nil }, func(pg histPage, blk []byte, blkAt int64) {
				if ctx.Err() != nil {
					return // (answered already, or about to be: no more reading for it)
				}
				f := ""
				if !pg.Done {
					var err error
					if f, err = fpOf(pg.At.Cursor, blk, blkAt); err != nil {
						return // (a point it cannot be bound to: not one to stop at)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				pg.Items = append([]histItem(nil), pg.Items...)
				last, lastFP = &pg, f
			})
		})
		mu.Lock()
		failure = err
		mu.Unlock()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	if r.Context().Err() != nil {
		return // (the browser gave up: nothing to say)
	}
	mu.Lock()
	page, pageFP, err, none := last, lastFP, failure, absent
	mu.Unlock()
	switch {
	case none && fp != "":
		// (Gone while it was being paged through: rotated, say. Begun again.)
		writeError(w, http.StatusConflict, "history_changed", "the history changed: begin again from the latest")
	case none:
		writeJSON(w, http.StatusOK, map[string]any{"items": []histItem{}, "done": true})
	case errors.Is(err, errHistChanged):
		writeError(w, http.StatusConflict, "history_changed", "the history changed: begin again from the latest")
	case page == nil && err != nil:
		writeFileError(w, err)
	case page == nil:
		// (Nowhere it may stop yet: the host slow. Not "the end".)
		writeError(w, http.StatusGatewayTimeout, "history_timeout", "the host did not answer in time")
	default:
		items := page.Items
		if items == nil {
			items = []histItem{}
		}
		out := map[string]any{"items": items, "skipped": page.Skipped, "done": page.Done}
		if !page.Done {
			out["cursor"], out["fp"] = page.At.Cursor, pageFP
			if page.At.Skip != "" {
				out["skip"] = page.At.Skip
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}
