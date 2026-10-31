package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sftplite"
)

// The person's turns in a conversation's whole log, for the page's panel of
// them (plans/6-turn-index.md): each message of the person's, when, how it
// begins, and where its line is in the log — the page reads on back to it
// where it is not read in yet. The log only grows: it is gone through once,
// what was added since read on each time; another log in its place (its
// head, or what was last read, another; the conversation's reading another)
// gone through anew.
//
// Known limit: a message the agent later took back (a rewind) stays in the
// list; gone to, it is not found among what the page shows, and that is said.

// convoTurn: one message of the person's.
type convoTurn struct {
	At      int64  `json:"at"` // where its line begins, in the log's bytes
	Time    string `json:"ts,omitempty"`
	Preview string `json:"preview"`
}

// turnIndex: what is known of a log's turns so far. sem: held by the one
// asking that reads it (taken with a deadline, its asker's end honoured).
type turnIndex struct {
	sem      chan struct{}
	refs     int // (under turnIndexes.mu) askings holding it, from get to put
	log      string
	agent    string
	gen      int64  // the conversation's reading it is of
	head     []byte // the log's first bytes, as read when it was begun
	tail     []byte // the bytes just before scanned, as read then
	scanned  int64  // read up to here (a line's start, or within a long line passed over)
	skipping bool   // within a line too long to be the person's, passed over to its end
	turns    []convoTurn
	cut      bool // more turns than are kept: the first ones let go of
}

const (
	turnChunk      = 1 << 20         // read a part at a time
	turnBudget     = 32 << 20        // read at most this much an asking
	turnTime       = 8 * time.Second // nor for longer
	turnsKept      = 20000           // the latest of them
	turnsSlack     = 2000            // let go of in batches of this many, past turnsKept
	turnPreview    = 120             // characters of each
	turnMark       = 64              // bytes of the log's head, and of what is before scanned, to tell another log
	turnIndexesMax = 256             // conversations' indexes kept at most
)

// turnLine: a line of the log that may be one of the person's messages, by
// its bytes (the rest not read as JSON): Claude Code's user rows, Codex's
// UserMessage items and its older user_message events.
func turnLine(agent string, line []byte) bool {
	switch agent {
	case "claude":
		// (As Claude Code writes it, no space; as another JSON writer may, with one.)
		return bytes.Contains(line, []byte(`"type":"user"`)) || bytes.Contains(line, []byte(`"type": "user"`))
	case "codex":
		return bytes.Contains(line, []byte(`"UserMessage"`)) || bytes.Contains(line, []byte(`"user_message"`))
	}
	return false
}

// turnOf: the person's message a line is, read as its agent's parser reads
// it (its own rules for what is the person's: not a tool's result, not a
// note of the agent's); ok false where it is not one.
func turnOf(agent string, line []byte) (ts, text string, ok bool) {
	p := convo.New(agent)
	if p == nil {
		return "", "", false
	}
	p.Feed(append(append([]byte(nil), line...), '\n'))
	for _, e := range p.Since(0) {
		if e.Role != convo.RoleUser || e.Gone {
			continue
		}
		var words []string
		for _, part := range e.Parts {
			if part.Kind == convo.KindText && strings.TrimSpace(part.Text) != "" {
				words = append(words, part.Text)
			}
		}
		if len(words) == 0 {
			continue
		}
		return e.Time, strings.Join(words, " "), true
	}
	return "", "", false
}

// preview: the first characters of a message, its white space as one.
func preview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= turnPreview {
		return text
	}
	r := []rune(text)
	return string(r[:turnPreview]) + "…"
}

// turnReader: the log read: its size; a range of it. (Each read on its own
// short while, as a conversation's read: a stuck one is given up like
// those, not holding the channel for the whole asking.)
type turnReader struct {
	size func() (int64, error)
	read func(off int64, n int) ([]byte, error)
}

// scan reads on from where it was, a part at a time, till the log's end,
// the budget, or the deadline (checked between parts): more says it
// stopped short of the end. (Its sem held by the caller.)
func (idx *turnIndex) scan(r turnReader, deadline time.Time) (more bool, size int64, err error) {
	if size, err = r.size(); err != nil {
		return false, 0, err
	}
	// (Another log in its place: shorter than read, another head, or other
	// bytes just before where it was read to — gone through anew.)
	same := size >= idx.scanned
	if same && idx.head != nil {
		head, err := r.read(0, int(min(size, turnMark)))
		if err != nil {
			return false, size, err
		}
		same = bytes.Equal(head, idx.head[:min(len(idx.head), len(head))])
	}
	if same && idx.tail != nil {
		tail, err := r.read(idx.scanned-int64(len(idx.tail)), len(idx.tail))
		if err != nil {
			return false, size, err
		}
		same = bytes.Equal(tail, idx.tail)
	}
	if !same {
		idx.reset(idx.log, idx.agent, idx.gen)
	}
	if idx.head == nil && size > 0 {
		if idx.head, err = r.read(0, int(min(size, turnMark))); err != nil {
			return false, size, err
		}
	}
	read := int64(0)
	for idx.scanned < size {
		if read >= turnBudget || time.Now().After(deadline) {
			return true, size, nil
		}
		n := min(int64(turnChunk), size-idx.scanned)
		data, err := r.read(idx.scanned, int(n))
		if err != nil {
			return false, size, err
		}
		read += int64(len(data))
		start := 0
		if idx.skipping {
			// (Within a line too long to be the person's: on to its end.)
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				idx.advance(data, len(data))
				continue
			}
			idx.skipping = false
			start = i + 1
		}
		end := bytes.LastIndexByte(data, '\n')
		if end < start {
			if start > 0 {
				// (The long one ended; what follows, a line of its own, read
				// from its start next — not passed over with it: review518.)
				idx.advance(data, start)
				continue
			}
			if int64(len(data)) < turnChunk {
				return false, size, nil // (a line not ended yet: read once it is)
			}
			// (Read from a line's start, a whole part without its end: a line
			// too long to be the person's, passed over to its end.)
			idx.skipping = true
			idx.advance(data, len(data))
			continue
		}
		at := idx.scanned + int64(start)
		for _, line := range bytes.SplitAfter(data[start:end+1], []byte{'\n'}) {
			if turnLine(idx.agent, line) {
				if ts, text, ok := turnOf(idx.agent, bytes.TrimRight(line, "\n")); ok {
					idx.add(convoTurn{At: at, Time: ts, Preview: preview(text)})
				}
			}
			at += int64(len(line))
		}
		idx.advance(data, end+1)
	}
	return false, size, nil
}

// advance: read to n bytes into data (read at scanned); what is just before
// where it got to kept from those very bytes — read as they were taken in,
// not read again after (review518) — to tell, next time, it is the same log.
// (An equal rewrite of the middle of a log, its head and that kept, is not
// told: a log only grows.)
func (idx *turnIndex) advance(data []byte, n int) {
	keep := append(idx.tail, data[:n]...)
	if len(keep) > turnMark {
		keep = keep[len(keep)-turnMark:]
	}
	idx.tail = append([]byte(nil), keep...)
	idx.scanned += int64(n)
}

// add keeps the latest turnsKept, the first let go of in batches (not each
// one as it comes: a copy of them all each time).
func (idx *turnIndex) add(t convoTurn) {
	idx.turns = append(idx.turns, t)
	if len(idx.turns) > turnsKept+turnsSlack {
		idx.turns = append(idx.turns[:0:0], idx.turns[len(idx.turns)-turnsKept:]...)
		idx.cut = true
	}
}

func (idx *turnIndex) reset(log, agent string, gen int64) {
	idx.log, idx.agent, idx.gen, idx.head, idx.tail, idx.scanned, idx.skipping, idx.turns, idx.cut = log, agent, gen, nil, nil, 0, false, nil, false
}

// take: the index's sem, by the deadline or the asker's end; false: not got.
func (idx *turnIndex) take(ctx context.Context, deadline time.Time) bool {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case idx.sem <- struct{}{}:
		return true
	case <-ctx.Done():
	case <-t.C:
	}
	return false
}

func (idx *turnIndex) give() { <-idx.sem }

// turnIndexes: one a conversation (a session's pane), its log's.
type turnIndexes struct {
	mu sync.Mutex
	m  map[string]*turnIndex
}

// get: the conversation's index, held (refs) till put; nil where none can be
// had — all those kept held, none to let go of (review518: one let go of
// between its get and its take was made anew beside it, two reading the
// same log; and the bound was passed).
func (t *turnIndexes) get(key string) *turnIndex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*turnIndex{}
	}
	idx := t.m[key]
	if idx == nil {
		if len(t.m) >= turnIndexesMax {
			for k, x := range t.m { // (one let go of, none held: an index is only a cache)
				if x.refs == 0 {
					delete(t.m, k)
					break
				}
			}
			if len(t.m) >= turnIndexesMax {
				return nil
			}
		}
		idx = &turnIndex{sem: make(chan struct{}, 1)}
		t.m[key] = idx
	}
	idx.refs++
	return idx
}

func (t *turnIndexes) put(idx *turnIndex) {
	t.mu.Lock()
	idx.refs--
	t.mu.Unlock()
}

// GET /api/sessions/{id}/conversation/turns?pane= — the person's turns in
// the conversation's whole log, as read so far (more: not all of it yet;
// asked again, it reads on); from: where what the page has read in begins.
func (s *Server) apiConversationTurns(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	deadline := time.Now().Add(turnTime)
	pane := r.URL.Query().Get("pane")
	if pane != "" && !herdrPane.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "bad pane")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	key := sess.ID + "\x00"
	if sess.Persistence == "herdr" {
		key += pane
	}
	main := s.convos.peek(key)
	if main == nil {
		writeError(w, http.StatusConflict, "not_located", "the conversation is not read yet")
		return
	}
	// (Its reading's lock waited for by the deadline only: a read of it under
	// way may hold it a while — review518.)
	for !main.mu.TryLock() {
		if time.Now().After(deadline) || r.Context().Err() != nil {
			writeError(w, http.StatusServiceUnavailable, "turns_busy", "the conversation is being read: ask again")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log, agent, reason, gen, sub, from := main.log, main.agent, main.reason, main.gen, main.sub, main.from
	main.mu.Unlock()
	if log == "" || reason != "" || sub || (agent != "claude" && agent != "codex") {
		writeError(w, http.StatusConflict, "unsupported", "no log of Claude Code's or Codex's read here")
		return
	}
	idx := s.turns.get(key)
	if idx == nil {
		writeError(w, http.StatusServiceUnavailable, "turns_busy", "too many conversations' turns are being read: ask again")
		return
	}
	defer s.turns.put(idx)
	if !idx.take(r.Context(), deadline) {
		writeError(w, http.StatusServiceUnavailable, "turns_busy", "the turns are still being read: ask again")
		return
	}
	if idx.log != log || idx.agent != agent || idx.gen != gen {
		idx.reset(log, agent, gen)
	}
	// (Each part read on its own short while, as a conversation's read is —
	// its channel had by the deadline: review518. A read stuck past that
	// while is given up as a conversation's is, its channel with it.)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	reader := turnReader{
		size: func() (size int64, err error) {
			err = s.withSFTP(ctx, sess, convoReadLimit, func(c *sftplite.Client) error {
				fi, err := c.Lstat(log)
				if err == nil {
					size = fi.Size()
				}
				return err
			})
			return size, err
		},
		read: func(off int64, n int) (data []byte, err error) {
			err = s.withSFTP(ctx, sess, convoReadLimit, func(c *sftplite.Client) error {
				data, err = c.ReadRange(log, off, n)
				return err
			})
			return data, err
		},
	}
	more, size, err := idx.scan(reader, deadline)
	turns := append([]convoTurn{}, idx.turns...)
	scanned, cut := idx.scanned, idx.cut
	idx.give() // (before the answer goes: a slow one does not hold the next asking)
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSONCompressed(w, r, http.StatusOK, map[string]any{"gen": gen, "turns": turns, "more": more, "scanned": scanned, "size": size, "cut": cut, "from": from})
}
