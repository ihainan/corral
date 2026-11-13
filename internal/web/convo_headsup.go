package web

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Claude Code's notes of what the person should know (its built-in
// you-should-know plugin: a side agent that watches the work and flags what
// might be missed, "✦ Heads up · …"). The note is drawn on the terminal
// only, above the input, with keys to answer it; nothing of it goes to the
// session's log. The plugin keeps it in its own store, by session:
// <config>/plugins/store/cc-plugin-you-should-know_*.json, {"sessions":
// [{"id", "tag", "view": {"kind": "offer"|"explained", "line",
// "explanation" | "text", "shownAt"}}]}.
//
// The store keeps a note after it has gone from the screen (the process
// restarted, or the note let go), so the screen says whether there is one:
// shown only while the pane's screen draws "✦ <tag>" and the start of the
// note's line. The store gives only its words. Read on the host: the screen,
// and only when that shows a note, the store, of the one session only.
// Nothing is answered from here: the note's keys are pressed in the terminal.

const (
	headsUpFresh   = 10 * time.Second
	headsUpOutMax  = 64 << 10
	headsUpLineMax = 2000
	headsUpMoreMax = 24000
)

// A Claude Code session's id, as its log is named: the only shape that goes
// to the host.
var claudeSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type headsUpNote struct {
	Tag     string `json:"tag"`
	Kind    string `json:"kind"`           // "offer": its line, more on asking; "explained": asked, more is the answer
	Line    string `json:"line"`           // (the line drawn)
	More    string `json:"more,omitempty"` // (Markdown: the explanation, or the answer once asked)
	ShownAt int64  `json:"shown_at,omitempty"`
}

// headsUpPy reads the screen on stdin; argv: the config folder, the session.
// Prints the note as JSON, or nothing.
const headsUpPy = `import glob, json, os, sys
scr = sys.stdin.read()
cfg, sid = sys.argv[1], sys.argv[2]
flat = ''.join(scr.split())
def pick():
    for f in sorted(glob.glob(os.path.join(cfg, 'plugins', 'store', 'cc-plugin-you-should-know_*.json'))):
        if os.path.islink(f) or os.path.getsize(f) > 8 << 20:
            continue
        try:
            d = json.load(open(f, encoding='utf-8'))
        except Exception:
            continue
        for s in (d.get('sessions') or []) if isinstance(d, dict) else []:
            if isinstance(s, dict) and s.get('id') == sid:
                return s
    return None
s = pick()
v = s and s.get('view')
if not isinstance(v, dict) or v.get('kind') not in ('offer', 'explained'):
    sys.exit(0)
tag = str(s.get('tag') or 'Heads up')
line = str(v.get('line') or '')
head = ''.join(line.split())[:12]
if not line or '✦ ' + tag not in scr or head not in flat:
    sys.exit(0)
more = v.get('text') if v.get('kind') == 'explained' else v.get('explanation')
try:
    at = int(v.get('shownAt') or 0)
except Exception:
    at = 0
print(json.dumps({'tag': tag[:40], 'kind': v['kind'], 'line': line[:` + "2000" + `], 'more': str(more or '')[:` + "24000" + `], 'shown_at': at}, ensure_ascii=False))
`

// headsUpCommand: fixed script; the validated pane, the herdr socket, the
// config folder the located log lies in and the session's id are its
// arguments.
func headsUpCommand(pane, socket, cfg, sid string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
s=$(HERDR_SOCKET_PATH=$2 herdr pane read "$1" --source visible 2>/dev/null 9>&-) || exit 0
case $s in *"✦ "*) ;; *) exit 0 ;; esac
command -v python3 >/dev/null 2>&1 || exit 0
printf '%s' "$s" | python3 -I -c "$5" "$3" "$4" 2>/dev/null
exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket) + " " + sshx.QuoteShell(cfg) + " " + sshx.QuoteShell(sid) + " " + sshx.QuoteShell(headsUpPy)
}

// headsUpWhere: of a Claude Code log (<config>/projects/<folder>/<id>.jsonl),
// its config folder and the session's id; none of another shape.
func headsUpWhere(log string) (cfg, sid string, ok bool) {
	if !strings.HasSuffix(log, ".jsonl") || !filepath.IsAbs(log) {
		return "", "", false
	}
	sid = strings.TrimSuffix(filepath.Base(log), ".jsonl")
	projects := filepath.Dir(filepath.Dir(log))
	if !claudeSessionID.MatchString(sid) || filepath.Base(projects) != "projects" || filepath.Clean(log) != log {
		return "", "", false
	}
	return filepath.Dir(projects), sid, true
}

func parseHeadsUp(out []byte) *headsUpNote {
	var n headsUpNote
	if json.Unmarshal(out, &n) != nil || n.Line == "" || (n.Kind != "offer" && n.Kind != "explained") {
		return nil
	}
	n.Tag = clipRunes(n.Tag, 40)
	// (The plugin ends each line with a full stop of its own, after the
	// one a line in Chinese has already: "。.".)
	if l := strings.TrimSuffix(n.Line, "."); l != n.Line {
		if r, _ := utf8.DecodeLastRuneInString(l); strings.ContainsRune("。！？…）」』”", r) {
			n.Line = l
		}
	}
	n.Line = clipRunes(n.Line, headsUpLineMax)
	n.More = clipRunes(n.More, headsUpMoreMax)
	return &n
}

// headsUpLooks: each pane's last answer, kept headsUpFresh (every page
// open on it asks).
type headsUpLooks struct {
	mu   sync.Mutex
	seen map[string]headsUpSeen
}

type headsUpSeen struct {
	at   time.Time
	note *headsUpNote
}

func (h *headsUpLooks) get(key string, now time.Time) (*headsUpNote, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.seen[key]
	if !ok || now.Sub(e.at) >= headsUpFresh {
		return nil, false
	}
	return e.note, true
}

func (h *headsUpLooks) put(key string, n *headsUpNote, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = map[string]headsUpSeen{}
	}
	for k, e := range h.seen {
		if now.Sub(e.at) >= headsUpFresh {
			delete(h.seen, k)
		}
	}
	h.seen[key] = headsUpSeen{now, n}
}

// GET /api/sessions/{id}/conversation/headsup?pane=… → {"note": {…} | null}:
// the note Claude Code shows on the pane now, of the log the conversation
// reads there. None where the conversation is not Claude Code's, where it is
// not in herdr, or where it could not be read.
func (s *Server) apiConversationHeadsUp(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	pane := r.URL.Query().Get("pane")
	if pane != "" && !herdrPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "bad pane")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	none := map[string]any{"note": nil}
	socket := s.herdrSocks.get(sess.ID)
	if sess.Persistence != "herdr" || pane == "" || socket == "" {
		writeJSON(w, http.StatusOK, none)
		return
	}
	main := s.convos.peek(sess.ID + "\x00" + pane)
	if main == nil {
		writeJSON(w, http.StatusOK, none)
		return
	}
	main.mu.Lock()
	log, agentKind, reason, sub := main.log, main.agent, main.reason, main.sub
	main.mu.Unlock()
	cfg, sid, ok := headsUpWhere(log)
	if !ok || reason != "" || agentKind != "claude" || sub {
		writeJSON(w, http.StatusOK, none)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"note": s.headsUp(r.Context(), sess, pane, socket, cfg, sid)})
}

func (s *Server) headsUp(ctx context.Context, sess *term.Session, pane, socket, cfg, sid string) *headsUpNote {
	key := sess.ID + "\x00" + pane + "\x00" + sid
	now := time.Now()
	if n, ok := s.headsUps.get(key, now); ok {
		return n
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), headsUpCommand(pane, socket, cfg, sid), headsUpOutMax, sshx.ExecOptions{})
	if err != nil {
		return nil // (not kept: asked again next time)
	}
	n := parseHeadsUp(res.Out)
	s.headsUps.put(key, n, now)
	return n
}
