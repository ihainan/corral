package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// What a Claude Code session's subagents are doing, for the list of what it
// set going. Its log tells only of those it sent off itself; those they sent
// off in turn are told of in theirs. So each one's log is looked at from the
// end, on the host: whether it has ended, and (with detail: only while the
// list is opened) what it does now and how much it has in hand. Read when
// asked, at most every subScanFresh; nothing of it is kept but that answer.

const (
	subScanMax     = 120      // logs looked at, the newest first
	subScanOutMax  = 6 << 20  // what a scan may print
	subScanRowsMax = 64 << 10 // of one log's last rows (detail): more is left out
	// (Of how many (detail): so that the frames and all those fit subScanOutMax.)
	subScanDetailMax = 60
	// (A command may keep a subagent quiet for long, so this is long.)
	subScanGone = 2 * time.Hour
)

var subScanFresh = map[bool]time.Duration{false: 10 * time.Second, true: 3 * time.Second}

// subScanCommand prints, for each subagent's log in the session's folder
// (not through a link, as apiConversationSubagent's), the newest first, a
// frame: \001F <id>, \001M its meta file, \001T the time on its first row,
// \001E 1 if it has ended; and with detail, of one not ended, \001R its last
// rows. A row is one line, and control characters in a row are escaped, so
// \001 starts only these. One untouched for subScanGone is not looked at, nor
// any older: taken for gone (its session stopped under it).
// Ended: its last row of its own is its handback (Claude Code's call ending a
// subagent), or an answer where it has none: not an earlier one, a subagent
// being taken up again.
func subScanCommand(home string, detail bool) string {
	script := `d=$1/subagents; det=$2
[ -L "$1" ] || [ -L "$d" ] && exit 0
cd "$d" 2>/dev/null || exit 0
now=$(date +%s); soh=$(printf '\001'); told=0
ls -t 2>/dev/null | grep -E '^agent-[A-Za-z0-9_-]+\.jsonl$' | head -n ` + strconv.Itoa(subScanMax) + ` | while IFS= read -r f; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  a=${f%.jsonl}; a=${a#agent-}
  mt=$(date -r "$f" +%s 2>/dev/null || echo 0)
  [ "$mt" -gt 0 ] && [ $((now - mt)) -gt ` + strconv.Itoa(int(subScanGone/time.Second)) + ` ] && break
  printf '\001F %s\n' "$a"
  if [ -f "agent-$a.meta.json" ] && [ ! -L "agent-$a.meta.json" ]; then printf '\001M %s\n' "$(head -c 2048 "agent-$a.meta.json" | tr -d '\n\001')"; fi
  printf '\001T %s\n' "$(head -n 1 "$f" | grep -o '"timestamp":"[^"]*"' | head -n 1)"
  e=0
  tail -c 262144 "$f" | grep '"type":"assistant"' | tail -n 1 | grep -qE '"name":"SubagentHandback"|"stop_reason":"end_turn"' && e=1
  printf '\001E %s\n' "$e"
  if [ "$det" = 1 ] && [ "$e" = 0 ] && [ "$told" -lt ` + strconv.Itoa(subScanDetailMax) + ` ]; then
    told=$((told + 1))
    tail -c 262144 "$f" | tail -n +2 | grep -E '"type":"(assistant|user)"' | tail -n 4 | tail -c ` + strconv.Itoa(subScanRowsMax) + ` | sed "s/^/${soh}R /"
  fi
done`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(home) + " " + sshx.QuoteShell(map[bool]string{false: "0", true: "1"}[detail])
}

// subScanAgent is one subagent as a scan found it.
type subScanAgent struct {
	ID     string      `json:"id"`
	What   string      `json:"what,omitempty"`   // what it was sent for
	Depth  int         `json:"depth,omitempty"`  // 1: the session's own; 2 and on: sent off by a subagent
	Time   string      `json:"ts,omitempty"`     // when it began
	Tool   *convo.Tool `json:"tool,omitempty"`   // (detail) what it does, or did last
	Tokens int64       `json:"tokens,omitempty"` // (detail) what its last request carried
	rows   []byte
	ended  bool
}

// parseSubScan reads what subScanCommand printed.
func parseSubScan(out []byte) []subScanAgent {
	var list []subScanAgent
	var cur *subScanAgent
	for _, l := range strings.Split(string(out), "\n") {
		if len(l) < 3 || l[0] != 1 || l[2] != ' ' {
			continue
		}
		v := l[3:]
		switch l[1] {
		case 'F':
			cur = nil
			if convo.SubagentID(v) {
				list = append(list, subScanAgent{ID: v, Depth: 1})
				cur = &list[len(list)-1]
			}
		case 'M':
			if cur != nil {
				var m struct {
					Description string `json:"description"`
					SpawnDepth  int    `json:"spawnDepth"`
				}
				if json.Unmarshal([]byte(v), &m) == nil {
					cur.What = strings.ToValidUTF8(clipStr(m.Description, 200), "")
					if m.SpawnDepth > 0 {
						cur.Depth = m.SpawnDepth
					}
				}
			}
		case 'T':
			if cur != nil {
				cur.Time = clipStr(strings.TrimSuffix(strings.TrimPrefix(v, `"timestamp":"`), `"`), 64)
			}
		case 'E':
			if cur != nil {
				cur.ended = v == "1"
			}
		case 'R':
			if cur != nil {
				cur.rows = append(append(cur.rows, v...), '\n')
			}
		}
	}
	going := list[:0]
	for _, a := range list {
		if a.ended {
			continue
		}
		if len(a.rows) > 0 {
			a.Tool, a.Tokens = subScanNow(a.rows)
			a.rows = nil
		}
		going = append(going, a)
	}
	return going
}

// subScanNow: from a subagent's last rows, the call it made last (as the
// conversation shows a call: only what says what it is) and what its last
// request carried, as Claude Code counts one: all it read and wrote.
func subScanNow(rows []byte) (*convo.Tool, int64) {
	var tokens int64
	for _, l := range strings.Split(string(rows), "\n") {
		var r struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					In     int64 `json:"input_tokens"`
					Cached int64 `json:"cache_read_input_tokens"`
					Made   int64 `json:"cache_creation_input_tokens"`
					Out    int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(l), &r) == nil && r.Type == "assistant" {
			if n := r.Message.Usage.In + r.Message.Usage.Cached + r.Message.Usage.Made + r.Message.Usage.Out; n > 0 {
				tokens = n
			}
		}
	}
	p := convo.NewClaudeSub()
	p.Feed(rows)
	entries := p.Since(-1)
	for i := len(entries) - 1; i >= 0; i-- {
		parts := entries[i].Parts
		for j := len(parts) - 1; j >= 0; j-- {
			if t := parts[j].Tool; t != nil {
				return &convo.Tool{Name: t.Name, Title: t.Title, Detail: strings.ToValidUTF8(clipStr(t.Detail, 300), ""), Done: t.Done}, tokens
			}
		}
	}
	return nil, tokens
}

// subScanSeen keeps a scan's answer for a moment: several tabs, or a tab
// reading again, do not each go to the host.
type subScanSeen struct {
	mu    sync.Mutex
	seen  map[string]subScanEntry
	going map[string]*subScanCall
}

// subScanCall is a scan under way, which others asking meanwhile wait for: each
// holds one of the connection's few places, wanted for the keys pressed too.
type subScanCall struct {
	done chan struct{}
	list []subScanAgent
	err  error
}

type subScanEntry struct {
	at   time.Time
	list []subScanAgent
}

func (c *subScanSeen) get(key string, fresh time.Duration, now time.Time) ([]subScanAgent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.seen[key]
	if !ok || now.Sub(e.at) >= fresh {
		return nil, false
	}
	return e.list, true
}

// join: the scan under way for key, or a new one to make (true).
func (c *subScanSeen) join(key string) (*subScanCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g, ok := c.going[key]; ok {
		return g, false
	}
	if c.going == nil {
		c.going = map[string]*subScanCall{}
	}
	g := &subScanCall{done: make(chan struct{})}
	c.going[key] = g
	return g, true
}

func (c *subScanSeen) leave(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.going, key)
}

func (c *subScanSeen) put(key string, list []subScanAgent, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]subScanEntry{}
	}
	for k, e := range c.seen {
		if now.Sub(e.at) > time.Minute {
			delete(c.seen, k)
		}
	}
	c.seen[key] = subScanEntry{now, list}
}

// GET /api/sessions/{id}/conversation/subagents?pane=…&detail=1 → {"agents": […]}:
// the subagents of the session the conversation reads now, none where it is
// not Claude Code's. Only of that log, as apiConversationSubagent.
func (s *Server) apiConversationSubagents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pane, detail := q.Get("pane"), q.Get("detail") == "1"
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
		writeJSON(w, http.StatusOK, map[string]any{"agents": []subScanAgent{}})
		return
	}
	main.mu.Lock()
	root, agentKind, reason := main.log, main.agent, main.reason
	sub := main.sub
	main.mu.Unlock()
	if root == "" || reason != "" || agentKind != "claude" || sub || !strings.HasSuffix(root, ".jsonl") {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []subScanAgent{}})
		return
	}
	list, err := s.subScan(r.Context(), sess, root, detail)
	if err != nil {
		writeError(w, http.StatusBadGateway, "no_scan", "the subagents' logs could not be read")
		return
	}
	if list == nil {
		list = []subScanAgent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

func (s *Server) subScan(ctx context.Context, sess *term.Session, root string, detail bool) ([]subScanAgent, error) {
	key := root + "\x00" + strconv.FormatBool(detail) + "\x00" + sess.ID
	now := time.Now()
	if list, ok := s.subScans.get(key, subScanFresh[detail], now); ok {
		return list, nil
	}
	c, first := s.subScans.join(key)
	if first {
		// (Not of the one asking only: others wait on it, whoever goes away.)
		go func() {
			// (One made meanwhile, between looking and joining: that one.)
			if list, ok := s.subScans.get(key, subScanFresh[detail], time.Now()); ok {
				c.list = list
				s.subScans.leave(key)
				close(c.done)
				return
			}
			xctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			res, err := sshx.Exec(xctx, sess.Client(), subScanCommand(strings.TrimSuffix(root, ".jsonl"), detail), subScanOutMax, sshx.ExecOptions{})
			if err == nil {
				c.list = parseSubScan(res.Out)
				s.subScans.put(key, c.list, now)
			}
			c.err = err
			s.subScans.leave(key)
			close(c.done)
		}()
	}
	select {
	case <-c.done:
		return c.list, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
