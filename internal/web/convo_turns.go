package web

import (
	"bytes"
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
// head another) is gone through anew.

// convoTurn: one message of the person's.
type convoTurn struct {
	At      int64  `json:"at"` // where its line begins, in the log's bytes
	Time    string `json:"ts,omitempty"`
	Preview string `json:"preview"`
}

// turnIndex: what is known of a log's turns so far.
type turnIndex struct {
	mu      sync.Mutex
	log     string
	agent   string
	head    []byte // the log's first bytes, as read when it was begun
	scanned int64  // read up to here (a line's start)
	turns   []convoTurn
	cut     bool // more turns than are kept: the first ones let go of
}

const (
	turnChunk    = 1 << 20         // read a part at a time
	turnLineMax  = 1 << 20         // a line longer: not one of the person's (passed over)
	turnBudget   = 32 << 20        // read at most this much an asking
	turnTime     = 8 * time.Second // nor for longer
	turnsKept    = 20000           // the latest of them
	turnPreview  = 120             // characters of each
	turnHeadSize = 64              // the log's first bytes, to tell another log
)

// turnLine: a line of the log that may be one of the person's messages, by
// its bytes (the rest not read as JSON): Claude Code's user rows, Codex's
// UserMessage items.
func turnLine(agent string, line []byte) bool {
	switch agent {
	case "claude":
		// (As Claude Code writes it, no space; as another JSON writer may, with one.)
		return bytes.Contains(line, []byte(`"type":"user"`)) || bytes.Contains(line, []byte(`"type": "user"`))
	case "codex":
		return bytes.Contains(line, []byte(`"UserMessage"`))
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

// scan reads on from where it was, a part at a time, till the log's end,
// the budget, or the deadline: more says it stopped short of the end.
// (Its lock held by the caller.)
func (idx *turnIndex) scan(c *sftplite.Client, deadline time.Time) (more bool, size int64, err error) {
	fi, err := c.Lstat(idx.log)
	if err != nil {
		return false, 0, err
	}
	size = fi.Size()
	if size < idx.scanned {
		// (Shorter than read: not the same log, gone through anew.)
		idx.reset(idx.log, idx.agent)
	}
	head, err := c.ReadRange(idx.log, 0, int(min(size, turnHeadSize)))
	if err != nil {
		return false, size, err
	}
	if idx.head != nil && !bytes.Equal(head, idx.head[:min(len(idx.head), len(head))]) {
		idx.reset(idx.log, idx.agent)
	}
	if len(idx.head) < turnHeadSize {
		idx.head = head
	}
	read := int64(0)
	for idx.scanned < size {
		if read >= turnBudget || time.Now().After(deadline) {
			return true, size, nil
		}
		n := min(int64(turnChunk), size-idx.scanned)
		data, err := c.ReadRange(idx.log, idx.scanned, int(n))
		if err != nil {
			return false, size, err
		}
		read += int64(len(data))
		end := bytes.LastIndexByte(data, '\n')
		if end < 0 {
			if int64(len(data)) < turnChunk {
				return false, size, nil // (a line not ended yet: read once it is)
			}
			// (A line longer than a part: not one of the person's; passed
			// over to its end.)
			idx.scanned += int64(len(data))
			continue
		}
		at := idx.scanned
		for _, line := range bytes.SplitAfter(data[:end+1], []byte{'\n'}) {
			if len(line) <= turnLineMax && turnLine(idx.agent, line) {
				if ts, text, ok := turnOf(idx.agent, bytes.TrimRight(line, "\n")); ok {
					idx.add(convoTurn{At: at, Time: ts, Preview: preview(text)})
				}
			}
			at += int64(len(line))
		}
		idx.scanned += int64(end + 1)
	}
	return false, size, nil
}

func (idx *turnIndex) add(t convoTurn) {
	if len(idx.turns) >= turnsKept {
		idx.turns = append(idx.turns[:0:0], idx.turns[len(idx.turns)-turnsKept+1:]...)
		idx.cut = true
	}
	idx.turns = append(idx.turns, t)
}

func (idx *turnIndex) reset(log, agent string) {
	idx.log, idx.agent, idx.head, idx.scanned, idx.turns, idx.cut = log, agent, nil, 0, nil, false
}

// turnIndexes: one a conversation (a session's pane), its log's.
type turnIndexes struct {
	mu sync.Mutex
	m  map[string]*turnIndex
}

func (t *turnIndexes) get(key, log, agent string) *turnIndex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*turnIndex{}
	}
	idx := t.m[key]
	if idx == nil {
		if len(t.m) >= 256 {
			for k := range t.m { // (one let go of: an index is only a cache)
				delete(t.m, k)
				break
			}
		}
		idx = &turnIndex{}
		t.m[key] = idx
	}
	return idx
}

// GET /api/sessions/{id}/conversation/turns?pane= — the person's turns in
// the conversation's whole log, as read so far (more: not all of it yet;
// asked again, it reads on).
func (s *Server) apiConversationTurns(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
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
	main.mu.Lock()
	log, agent, reason, gen, sub := main.log, main.agent, main.reason, main.gen, main.sub
	main.mu.Unlock()
	if log == "" || reason != "" || sub || (agent != "claude" && agent != "codex") {
		writeError(w, http.StatusConflict, "unsupported", "no log of Claude Code's or Codex's read here")
		return
	}
	idx := s.turns.get(key, log, agent)
	// (One asking at a time an index; another waits a while, then is told to ask again.)
	deadline := time.Now().Add(turnTime)
	got := make(chan struct{})
	go func() { idx.mu.Lock(); close(got) }()
	select {
	case <-got:
	case <-time.After(turnTime):
		go func() { <-got; idx.mu.Unlock() }()
		writeError(w, http.StatusServiceUnavailable, "turns_busy", "the turns are still being read: ask again")
		return
	}
	defer idx.mu.Unlock()
	if idx.log != log || idx.agent != agent {
		idx.reset(log, agent)
	}
	var more bool
	var size int64
	err := s.withSFTPUntil(r.Context(), sess, deadline.Add(2*time.Second), func(c *sftplite.Client) error {
		var err error
		more, size, err = idx.scan(c, deadline)
		return err
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	turns := idx.turns
	if turns == nil {
		turns = []convoTurn{}
	}
	writeJSONCompressed(w, r, http.StatusOK, map[string]any{"gen": gen, "turns": turns, "more": more, "scanned": idx.scanned, "size": size, "cut": idx.cut})
}
