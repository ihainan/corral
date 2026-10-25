package web

// The conversation view: Claude Code's own log of the session running in the
// terminal's focused pane (herdr) or active pane (tmux), read over the
// session's SSH connection and shown as turns. Nothing is installed on the
// host: the pane is matched to its Claude Code process by the environment
// herdr and tmux give the processes they start, the process to its session
// by the file Claude Code keeps for each running process
// (~/.claude/sessions/<pid>.json), and the session to its log by name.
//
// Only the session's owner reads it. Someone watching a shared session sees
// the screen; the log holds the whole history, which is more than that.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

const (
	convoLocateTTL = 10 * time.Second // a pane's binding to a log is checked again after this
	convoIdle      = 10 * time.Minute // a log nobody reads is forgotten after this
	convoTail      = 4 << 20          // bytes read from the end of a log when it is first opened
	convoStep      = 2 << 20          // bytes read at most by one request after that
	convoLocateMax = 256 << 10        // output of the locating script
	convoStatesMax = 64               // conversation states kept at once
	convoReadLimit = 15 * time.Second // a read the host has not answered by then is given up
)

// convoLocateCommand prints, for each Claude Code process of the user that
// is still running, a NUL-terminated record of key=value lines: its pid, its
// start time (/proc/<pid>/stat field 22), whether it is in front on its
// terminal (its process group is the terminal's foreground one), the start
// time its session file
// records (a file left by a process that ended, whose pid was then reused,
// does not match), the session id, the log found for it, and the herdr or
// tmux pane it runs in. With mode "tmux" a first record gives the active
// pane of the named tmux session, its server's pid and the pane's title. Linux only (/proc):
// elsewhere it prints the record "noproc". Fixed script; the mode and the
// validated tmux session name are its only arguments.
func convoLocateCommand(mode, tmuxName string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then
  t=$(tmux -u display-message -p -t "=$2:" '#{pane_id} #{pid} #{pane_title}' 2>/dev/null | head -c 600 | tr -d '\n')
  printf 'tmux=%s\0' "$t"
fi
[ -r /proc/self/stat ] || { printf 'noproc\0'; exit 0; }
nl=$(printf '\nx'); nl=${nl%x}
btime=$(sed -n 's/^btime \([0-9]*\)$/\1/p' /proc/stat 2>/dev/null); hz=$(getconf CLK_TCK 2>/dev/null)
case $hz in ''|*[!0-9]*|0) hz=;; esac
for f in "$HOME"/.claude/sessions/*.json; do
  [ -f "$f" ] || continue
  pid=${f##*/}; pid=${pid%.json}
  case $pid in ''|*[!0-9]*) continue;; esac
  [ -r "/proc/$pid/environ" ] || continue
  stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null)
  start=$(printf '%s' "${stat##*) }" | cut -d' ' -f20)
  case $start in ''|*[!0-9]*) continue;; esac
  pgrp=$(printf '%s' "${stat##*) }" | cut -d' ' -f3); tpgid=$(printf '%s' "${stat##*) }" | cut -d' ' -f6)
  fg=0; [ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] && fg=1
  c=$(head -c 65536 "$f" 2>/dev/null | tr -d '\n')
  sid=$(printf '%s' "$c" | grep -o '"sessionId" *: *"[0-9a-fA-F-]*"' | head -n 1 | sed 's/.*"\([0-9a-fA-F-]*\)"$/\1/')
  ps=$(printf '%s' "$c" | grep -o '"procStart" *: *"*[0-9]*' | head -n 1 | sed 's/[^0-9]*\([0-9]*\)$/\1/')
  case $ps in ''|*[!0-9]*) continue;; esac
  case $sid in *[!0-9a-fA-F-]*) continue;; ????????-????-????-????-????????????) ;; *) continue;; esac
  log=
  for l in "$HOME"/.claude/projects/*/"$sid".jsonl; do
    case $l in *"$nl"*) continue;; esac
    [ -f "$l" ] && { log=$l; break; }
  done
  env=$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | grep -E '^(HERDR_PANE_ID|HERDR_SOCKET_PATH|TMUX|TMUX_PANE)=' | head -c 4096)
  printf 'pid=%s\nstart=%s\nbtime=%s\nhz=%s\nfg=%s\nprocstart=%s\nsid=%s\nlog=%s\n%s\0' "$pid" "$start" "$btime" "$hz" "$fg" "$ps" "$sid" "$log" "$env"
done
exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(tmuxName)
}

type convoProc struct {
	pid, start    int64
	fg            bool  // in front on its terminal: what is typed there reaches it
	started       int64 // when it started, unix milliseconds (0 when not known; within the second the host's boot time is known to)
	sid, log      string
	herdrPane     string
	herdrSocket   string
	tmuxSocket    string // (TMUX is "<socket>,<server pid>,<session index>")
	tmuxServerPID string
	tmuxPane      string
}

var (
	convoUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	herdrPane = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,64}$`)
	tmuxPane  = regexp.MustCompile(`^%[0-9]{1,9}$`)
)

// parseConvoLocate reads convoLocateCommand's output: the processes whose
// start time matches their session file, the tmux pane and server pid if
// asked, and whether the host has /proc at all.
func parseConvoLocate(out string) (procs []convoProc, tmuxActive, tmuxPID, tmuxTitle string, noProc bool) {
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "noproc" {
			noProc = true
			continue
		}
		if v, ok := strings.CutPrefix(rec, "tmux="); ok {
			// One query, so the pane, its server and its title go together.
			f := strings.SplitN(v, " ", 3)
			if len(f) >= 2 && tmuxPane.MatchString(f[0]) && isDigits(f[1]) {
				tmuxActive, tmuxPID = f[0], f[1]
				if len(f) == 3 {
					tmuxTitle = truncate(strings.Map(func(r rune) rune {
						if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
							return -1
						}
						return r
					}, strings.ToValidUTF8(f[2], "")), 120)
				}
			}
			continue
		}
		var p convoProc
		var procStart int64 = -1
		var btime, hz int64 // (when the host booted, unix seconds; its clock's ticks a second)
		seen := map[string]bool{}
		for _, line := range strings.Split(rec, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok || seen[k] {
				continue // (the first value of a key counts: a later line cannot replace it)
			}
			seen[k] = true
			switch k {
			case "pid":
				p.pid, _ = strconv.ParseInt(v, 10, 64)
			case "start":
				p.start, _ = strconv.ParseInt(v, 10, 64)
			case "fg":
				p.fg = v == "1"
			case "btime":
				btime, _ = strconv.ParseInt(v, 10, 64)
			case "hz":
				hz, _ = strconv.ParseInt(v, 10, 64)
			case "procstart":
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					procStart = n
				}
			case "sid":
				p.sid = v
			case "log":
				p.log = v
			case "HERDR_PANE_ID":
				p.herdrPane = v
			case "HERDR_SOCKET_PATH":
				p.herdrSocket = v
			case "TMUX":
				parts := strings.Split(v, ",")
				if len(parts) == 3 {
					p.tmuxSocket, p.tmuxServerPID = parts[0], parts[1]
				}
			case "TMUX_PANE":
				p.tmuxPane = v
			}
		}
		if p.pid <= 0 || p.start <= 0 || procStart != p.start || !convoUUID.MatchString(p.sid) {
			continue
		}
		// (Here, not in the host's shell, whose numbers may be 32 bits.)
		if btime > 0 && hz > 0 && hz <= 100000 && p.start < math.MaxInt64/1000 {
			p.started = btime*1000 + p.start*1000/hz
		}
		if l, ok := cleanRemotePath(p.log); !ok || !strings.HasSuffix(l, "/"+p.sid+".jsonl") {
			p.log = ""
		}
		procs = append(procs, p)
	}
	return procs, tmuxActive, tmuxPID, tmuxTitle, noProc
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pickConvoProc chooses the process running in the pane: for herdr, the
// focused pane of the herdr server the session is attached to; for tmux,
// the active pane of the session's tmux server. Of several (an agent
// started from another's shell), the one started last.
func pickConvoProc(procs []convoProc, persistence, pane, herdrSocket, tmuxActive, tmuxPID string) *convoProc {
	var best *convoProc
	for i := range procs {
		p := &procs[i]
		switch persistence {
		case "herdr":
			if pane == "" || p.herdrPane != pane || p.herdrSocket != herdrSocket {
				continue
			}
		case "tmux":
			if tmuxActive == "" || p.tmuxPane != tmuxActive || p.tmuxServerPID != tmuxPID {
				continue
			}
		default:
			continue
		}
		if best == nil || p.start > best.start {
			best = p
		}
	}
	return best
}

// convoGens numbers generations across all states, so a state made anew
// (the old one forgotten while a browser still held its numbers) never
// repeats one.
var convoGens atomic.Int64

// convoState is what the gateway knows of one session's conversation.
type convoState struct {
	used     atomic.Int64 // unix nanoseconds of the last request
	inUse    atomic.Int32 // requests holding it now: it is not let go meanwhile
	mu       sync.Mutex
	located  time.Time
	pane     string // (herdr) the pane it was located for
	reason   string // why there is nothing to read, when there is not
	sid, log string
	gen      int64      // changes when another log is read: what was shown does not apply
	bind     *convoBind // the process the log is of, while there is one
	parser   *convo.Claude
	offset   int64 // read up to here
	size     int64 // the log's size when last looked at
	mtime    time.Time
	older    bool   // the log has more before what was read
	more     bool   // the last read stopped before the end
	status   string // (tmux) the process's own word: "busy", "idle", or "" when not known
	skipping bool   // in a line too long to keep, until its end
}

type convoStates struct {
	mu sync.Mutex
	m  map[string]*convoState
}

func (c *convoStates) get(id string) *convoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*convoState{}
	}
	for k, st := range c.m {
		if k != id && st.idle() {
			delete(c.m, k)
		}
	}
	st, ok := c.m[id]
	// At most convoStatesMax, across sessions and users: the one used
	// longest ago gives way (its reader starts over if it asks again). One a
	// request holds stays (dropping it would make a second state read the
	// same log beside it); past the limit while all were in use, every later
	// request brings the count back down.
	room := convoStatesMax
	if ok {
		room++ // (this one stays)
	}
	for len(c.m) >= room {
		old, at := "", int64(0)
		for k, s := range c.m {
			if u := s.used.Load(); k != id && s.inUse.Load() == 0 && (old == "" || u < at) {
				old, at = k, u
			}
		}
		if old == "" {
			break // (all in use: as many as requests under way, for a moment)
		}
		delete(c.m, old)
	}
	if !ok {
		st = &convoState{gen: convoGens.Add(1)}
		c.m[id] = st
	}
	st.used.Store(time.Now().UnixNano())
	st.inUse.Add(1) // (the caller lets go with done)
	return st
}

func (st *convoState) done() { st.inUse.Add(-1) }

func (st *convoState) idle() bool {
	return time.Since(time.Unix(0, st.used.Load())) > convoIdle
}

// Reasons there is nothing to show.
const (
	convoNoAgent     = "no_agent"    // no Claude Code in the pane
	convoNoLog       = "no_log"      // it has not written its log yet (a new conversation)
	convoUnsupported = "unsupported" // the host has no /proc, or the session is a plain shell
)

// convoBind says what the entries are of: the pane, the Claude Code process
// in it (its pid and start time) and its session, so the browser can tell
// whether what it shows, and what it would act on, is still that.
type convoBind struct {
	Pane    string `json:"pane"`
	PID     int64  `json:"pid"`
	Start   int64  `json:"start"`
	Session string `json:"session"`
	Title   string `json:"title,omitempty"` // (tmux: the pane's title)
	// started is when the process started (unix milliseconds; 0 when not
	// known): what was queued before was another process's.
	started int64
}

type convoAnswer struct {
	Available bool       `json:"available"`
	Reason    string     `json:"reason,omitempty"`
	Gen       int64      `json:"gen"`
	Rev       int        `json:"rev"`
	Reset     bool       `json:"reset,omitempty"` // drop what was shown: these are all the entries
	Older     bool       `json:"older,omitempty"` // the conversation began before the first entry
	More      bool       `json:"more,omitempty"`  // more of the log is to be read: ask again soon
	Bind      *convoBind `json:"bind,omitempty"`
	// AgentStatus (tmux, where herdr does not say): "busy" or "idle" as
	// Claude Code's file for the process says; absent when not known.
	AgentStatus string `json:"agent_status,omitempty"`
	// Title is the session's own (Claude Code's, or the person's);
	// Tasks the agent's task list as the log has it.
	Title string       `json:"title,omitempty"`
	Tasks []convo.Task `json:"tasks,omitempty"`
	Meta  *convo.Meta  `json:"meta,omitempty"` // what it runs with, as the log says
	// Queued is what the person sent that the agent has not taken in yet.
	Queued  []convo.Queued `json:"queued,omitempty"`
	Entries []convo.Entry  `json:"entries"`
}

// GET /api/sessions/{id}/conversation?pane=<herdr pane>&gen=<n>&rev=<n>
//
// The entries changed since revision rev of generation gen; all of them when
// gen is not the current one (then reset is set).
func (s *Server) apiConversation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pane := q.Get("pane")
	if pane != "" && !herdrPane.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "bad pane")
		return
	}
	gen, _ := strconv.ParseInt(q.Get("gen"), 10, 64)
	rev, _ := strconv.Atoi(q.Get("rev"))
	w.Header().Set("Cache-Control", "no-store")
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeJSON(w, http.StatusOK, convoAnswer{Reason: convoUnsupported, Entries: []convo.Entry{}})
		return
	}
	// One state per pane read (herdr's pane asked for; tmux has its active
	// one): a tab and another page reading other panes do not take turns
	// rebuilding a single one.
	key := sess.ID + "\x00" // (tmux reads its active pane: the pane asked for does not matter)
	if sess.Persistence == "herdr" {
		key += pane
	}
	st := s.convos.get(key)
	defer st.done()
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if time.Since(st.located) > convoLocateTTL || (sess.Persistence == "herdr" && pane != st.pane) {
		if err := s.locateConvo(ctx, sess, st, pane); err != nil {
			if errors.Is(err, sshx.ErrBusy) {
				writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
			} else {
				writeError(w, http.StatusBadGateway, "host_error", "the host did not answer")
			}
			return
		}
	}
	ans := convoAnswer{Gen: st.gen, Bind: st.bind, Entries: []convo.Entry{}}
	if st.reason != "" {
		ans.Reason = st.reason
		writeJSON(w, http.StatusOK, ans)
		return
	}
	more, err := s.readConvo(sess, st)
	if err != nil {
		writeFileError(w, err)
		return
	}
	ans.Available, ans.Gen, ans.Rev, ans.Older, ans.More, ans.AgentStatus = true, st.gen, st.parser.Rev(), st.older, more, st.status
	if gen != st.gen {
		ans.Reset = true
		rev = 0
	}
	ans.Entries = st.parser.Since(rev)
	ans.Title, ans.Tasks = st.parser.Title(), st.parser.Tasks()
	if m := st.parser.Meta(); m != (convo.Meta{}) {
		ans.Meta = &m
	}
	ans.Queued = st.parser.Queued()
	writeJSON(w, http.StatusOK, ans)
}

// locateConvo finds the log of the Claude Code session in the pane; a
// different one from before starts a new generation.
func (s *Server) locateConvo(ctx context.Context, sess *term.Session, st *convoState, pane string) error {
	out, err := sshx.Output(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession), convoLocateMax)
	if err != nil && (errors.Is(err, sshx.ErrBusy) || len(out) == 0) {
		return err
	}
	procs, tmuxActive, tmuxPID, tmuxTitle, noProc := parseConvoLocate(string(out))
	st.located, st.pane = time.Now(), pane
	sid, log, reason := "", "", ""
	switch {
	case noProc:
		reason = convoUnsupported
	default:
		p := pickConvoProc(procs, sess.Persistence, pane, s.herdrSocks.get(sess.ID), tmuxActive, tmuxPID)
		st.bind = nil
		if p != nil {
			b := &convoBind{PID: p.pid, Start: p.start, Session: p.sid, Pane: p.herdrPane}
			if sess.Persistence == "tmux" {
				b.Pane, b.Title = p.tmuxPane, tmuxTitle
			}
			// (Another process of the same session, resumed: what the one
			// before had queued is not this one's.)
			b.started = p.started
			st.bind = b
		}
		switch {
		case p == nil:
			reason = convoNoAgent
		case p.log == "":
			reason, sid = convoNoLog, p.sid
		default:
			sid, log = p.sid, p.log
		}
	}
	if sid != st.sid || log != st.log {
		st.gen = convoGens.Add(1)
		st.sid, st.log = sid, log
		st.parser = convo.NewClaude()
		st.offset, st.size, st.mtime, st.older, st.skipping = 0, -1, time.Time{}, false, false
	}
	st.reason = reason
	// (What was queued to a process of the same session before this one: not this one's.)
	if st.parser != nil && st.bind != nil {
		st.parser.QueueSince(st.bind.started)
	}
	return nil
}

// readConvo reads what the log gained since the last read: from the end at
// first, then forward. A log that got shorter was replaced and is read
// again. more says one request did not take all there is.
func (s *Server) readConvo(sess *term.Session, st *convoState) (more bool, err error) {
	err = s.withSFTP(context.Background(), sess, convoReadLimit, func(c *sftplite.Client) error {
		if sess.Persistence == "tmux" {
			st.status = convoProcStatus(c, st)
		}
		fi, err := c.Stat(st.log)
		if err != nil {
			return err
		}
		size := fi.Size()
		if size == st.size && fi.ModTime().Equal(st.mtime) && !st.more {
			return nil
		}
		if size < st.offset || size < st.size {
			st.gen = convoGens.Add(1)
			st.parser = convo.NewClaude()
			st.offset, st.older, st.skipping = 0, false, false
			if st.bind != nil {
				st.parser.QueueSince(st.bind.started) // (as locateConvo says it)
			}
		}
		limit := int64(convoStep)
		if st.size < 0 { // first read: the end of a long log
			limit = convoTail
			if size > convoTail {
				st.offset, st.older, st.skipping = size-convoTail, true, true
			}
		}
		n := min(size-st.offset, limit)
		data, err := c.ReadRange(st.log, st.offset, int(n))
		if err != nil {
			return err
		}
		st.size, st.mtime = size, fi.ModTime()
		st.more = st.offset+int64(len(data)) < size
		pos := 0
		if st.skipping { // (the rest of a line begun before what was read)
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				st.offset += int64(len(data))
				return nil
			}
			pos, st.skipping = i+1, false
		}
		end := bytes.LastIndexByte(data[pos:], '\n')
		switch {
		case end >= 0:
			st.parser.Feed(data[pos : pos+end+1])
			st.offset += int64(pos + end + 1)
		case int64(len(data)-pos) >= limit:
			// A line longer than a whole read (a pasted image): left out.
			st.offset += int64(len(data))
			st.skipping = true
		default:
			st.offset += int64(pos) // (the last line is still being written)
		}
		return nil
	})
	return st.more, err
}

// convoProcStatus reads what Claude Code's file for the bound process says it
// is doing ("busy", "idle"); "" when it cannot tell (no file, another
// process's file under a reused pid, another word). It never fails a read.
func convoProcStatus(c *sftplite.Client, st *convoState) string {
	if st.bind == nil {
		return ""
	}
	i := strings.LastIndex(st.log, "/.claude/projects/")
	if i < 0 {
		return ""
	}
	base := st.log[:i]
	b, err := c.ReadRange(base+"/.claude/sessions/"+strconv.FormatInt(st.bind.PID, 10)+".json", 0, 64<<10)
	if err != nil {
		return ""
	}
	var f struct {
		Status    string          `json:"status"`
		SessionID string          `json:"sessionId"`
		ProcStart json.RawMessage `json:"procStart"`
	}
	if json.Unmarshal(b, &f) != nil || f.SessionID != st.bind.Session || strings.Trim(string(f.ProcStart), `"`) != strconv.FormatInt(st.bind.Start, 10) {
		return ""
	}
	switch f.Status {
	case "busy", "idle":
		return f.Status
	}
	return ""
}

// tmuxKeysCommand presses keys in one tmux pane (by its id, "%N"), only if
// that pane is in the named tmux session: else it exits 3, pressing
// nothing. Fixed script; the validated session name, the pane id and keys
// of a fixed set (convoKeys) are its only arguments.
func tmuxKeysCommand(session, pane string, keys []string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
[ "$(tmux -u display-message -p -t "$2" '#{session_name}' 2>/dev/null)" = "$1" ] || exit 3
p=$2; shift 2
exec tmux send-keys -t "$p" "$@"`
	cmd := "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(session) + " " + sshx.QuoteShell(pane)
	for _, k := range keys {
		cmd += " " + sshx.QuoteShell(k)
	}
	return cmd
}

// convoSendCommand types the text on its input into the agent's pane and
// presses Enter, on the host, in one go: just before typing it checks again
// that the process asked for (pid, its start in clock ticks) is still the
// one in front on its terminal, and in that pane (its HERDR_PANE_ID or
// TMUX_PANE): the gateway's own check came a moment before, and text and
// Enter reaching a shell would run as a command. The text is read whole
// first (its length in bytes must be as said: a cut short input types
// nothing) and goes in as one bracketed paste, so its newlines and tabs are
// text, not keys: herdr types it as given (the markers included, for the
// program in front, which asked for them), tmux wraps it when the program
// asked (paste-buffer -p, its newlines kept with -r).
//
// One message at a time to a pane on the host too, where flock and the
// user's own XDG_RUNTIME_DIR are there (a lock the kernel lets go of with
// the script: the gateway may give up on a send whose script goes on; not
// in a folder others can write to, as /tmp), tried for 10 seconds (-n:
// BusyBox's has no -w). Without them, the gateway's turn is what there is.
//
// Exits 3: not that agent in front in that pane now (or, tmux, the pane is
// not in this session); 4: nothing typed; 5: the text typed, not Enter; 6:
// typing it failed part way, or may have (so does a signal once typing began). Fixed script; the mode, the
// validated tmux session name, pane id, pid, start, length and the herdr
// socket (from discovery) are its arguments, the text its input.
func convoSendCommand(mode, session, pane, socket string, pid, start int64, length int) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
mode=$1 sess=$2 p=$3 pid=$4 start=$5 len=$6 sock=$7
b=corral-send-$$ typing=0
f=$(mktemp) || exit 4
trap 'rm -f "$f"; [ "$mode" = tmux ] && tmux delete-buffer -b "$b" 2>/dev/null 9>&-' EXIT
trap '[ "$typing" = 1 ] && exit 6; exit 4' HUP INT TERM
cat >"$f" || exit 4
l=${XDG_RUNTIME_DIR:-}/corral-send-$(printf '%s' "$mode-$p" | tr -c 'A-Za-z0-9-' _)
if [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -d "$XDG_RUNTIME_DIR" ] && [ ! -L "$l" ] && command -v flock >/dev/null 2>&1 && (exec 9>>"$l") 2>/dev/null; then
  exec 9>>"$l"
  n=0
  until flock -n 9; do
    n=$((n + 1)); [ "$n" -ge 100 ] && exit 4
    sleep 0.1
  done
fi
[ "$(wc -c <"$f" | tr -d ' ')" = "$len" ] || exit 4
front() {
  stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null) || return 1
  rest=${stat##*) }
  [ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$start" ] || return 1
  pgrp=$(printf '%s' "$rest" | cut -d' ' -f3); tpgid=$(printf '%s' "$rest" | cut -d' ' -f6)
  [ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] || return 1
  if [ "$mode" = tmux ]; then k=TMUX_PANE; else k=HERDR_PANE_ID; fi
  tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "$k=$p"
}
front || exit 3
if [ "$mode" = tmux ]; then
  [ "$(tmux -u display-message -p -t "$p" '#{session_name}' 2>/dev/null 9>&-)" = "$sess" ] || exit 3
  tmux load-buffer -b "$b" "$f" 9>&- || exit 4
  front || exit 3
  typing=1
  tmux paste-buffer -p -r -d -b "$b" -t "$p" 9>&- || exit 6
  tmux send-keys -t "$p" Enter 9>&- || exit 5
else
  t=$(cat "$f" && printf x) || exit 4
  [ "${t%x}" != "$t" ] || exit 4
  t=${t%x}
  e=$(printf '\033')
  front || exit 3
  typing=1
  HERDR_SOCKET_PATH=$sock herdr pane send-text "$p" "$e[200~$t$e[201~" >/dev/null 2>&1 9>&- || exit 6
  HERDR_SOCKET_PATH=$sock herdr pane send-keys "$p" enter >/dev/null 2>&1 9>&- || exit 5
fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(session) + " " + sshx.QuoteShell(pane) + " " +
		strconv.FormatInt(pid, 10) + " " + strconv.FormatInt(start, 10) + " " + strconv.Itoa(length) + " " + sshx.QuoteShell(socket)
}

// convoSending lets one message at a time go to a pane: two typed at once
// would interleave (one's text, the other's, then both Enters: one message).
// A turn is given back once the host's command has really ended, not when
// the request gives up; a pane no one is sending to is forgotten.
var convoSending = paneTurns{m: map[string]*paneTurn{}}

type paneTurns struct {
	mu sync.Mutex
	m  map[string]*paneTurn
}

type paneTurn struct {
	ch chan struct{} // (of one: whose turn it is)
	n  int           // having it or waiting for it
}

func (p *paneTurns) take(ctx context.Context, key string) (release func(), ok bool) {
	p.mu.Lock()
	t := p.m[key]
	if t == nil {
		t = &paneTurn{ch: make(chan struct{}, 1)}
		p.m[key] = t
	}
	t.n++
	p.mu.Unlock()
	leave := func() {
		p.mu.Lock()
		if t.n--; t.n == 0 {
			delete(p.m, key)
		}
		p.mu.Unlock()
	}
	select {
	case t.ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-t.ch; leave() }) }, true
	case <-ctx.Done():
		leave()
		return nil, false
	}
}

// What the conversation presses in the agent's pane, in herdr's names and
// tmux's: stop (Escape), send a queued message now (Claude Code's Ctrl+X
// Ctrl+S, which stops what it was doing to take the message in), and send
// a message (its text, then Enter: see sendText).
var convoKeys = map[string]struct {
	audit string
	herdr []string
	tmux  []string
}{
	"interrupt": {"convo.interrupt", []string{"esc"}, []string{"Escape"}},
	"send_now":  {"convo.send_now", []string{"ctrl+x", "ctrl+s"}, []string{"C-x", "C-s"}},
	"send":      {"convo.send", nil, nil}, // (typed by convoSendCommand)
}

// maxSendText is the most a message sent from the agents page may hold (in
// bytes): herdr takes far more in one request (120 KB tried).
const maxSendText = 100 << 10

// sendText is a message as it may be typed into a pane: valid UTF-8, its
// line ends as "\n", and no other control character than a tab (an Escape
// or a Ctrl+B in it would be pressed, not typed). ok false: not so.
func sendText(s string) (string, bool) {
	if s == "" || len(s) > maxSendText || !utf8.ValidString(s) {
		return "", false
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return "", false
		}
	}
	return s, true
}

// POST /api/sessions/{id}/conversation/interrupt {"pane","pid","start","session"}
// POST /api/sessions/{id}/conversation/send-now (the same)
// POST /api/sessions/{id}/conversation/send (the same, and "text")
//
// Presses Escape in the pane (what Claude Code takes as "stop"), or Ctrl+X
// Ctrl+S (send the messages queued now), for the Claude Code process the
// conversation shows. The process is looked for again first: if that pane
// no longer runs it (another program, a restart), nothing is pressed and
// the answer is 409. Not with the terminal's own input: that goes to
// whatever pane is in front by the time it arrives.
func (s *Server) apiConversationInterrupt(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "interrupt")
}

func (s *Server) apiConversationSendNow(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "send_now")
}

// Send types a message into the agent's pane and presses Enter (Claude Code
// queues it while it works), as the terminal's composer would, but into that
// pane only: what herdr has in front, or the terminal shows, is not changed.
func (s *Server) apiConversationSend(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "send")
}

func (s *Server) convoPress(w http.ResponseWriter, r *http.Request, what string) {
	keys := convoKeys[what]
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Pane    string `json:"pane"`
		PID     int64  `json:"pid"`
		Start   int64  `json:"start"`
		Session string `json:"session"`
		Text    string `json:"text"` // (send)
	}
	err := readJSON(r, &b)
	text, textOK := sendText(b.Text)
	if what != "send" {
		textOK = b.Text == ""
	}
	if err != nil || !textOK {
		writeError(w, http.StatusBadRequest, "bad_request", "the pane, the process (and, to send, a message) are required")
		return
	}
	if b.PID <= 0 || b.Start <= 0 || !convoUUID.MatchString(b.Session) ||
		(sess.Persistence == "herdr" && !herdrPane.MatchString(b.Pane)) || (sess.Persistence == "tmux" && !tmuxPane.MatchString(b.Pane)) {
		writeError(w, http.StatusBadRequest, "bad_request", "the pane and the process are required")
		return
	}
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeError(w, http.StatusConflict, "unsupported", "only in tmux and herdr sessions")
		return
	}
	// Every attempt is audited, with what it was for and what became of it
	// ("typed", "refused" or "unknown"; why, when nothing was pressed).
	outcome, reason := "refused", ""
	defer func() {
		actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer acancel()
		detail := map[string]any{"session": sess.ID, "pane": b.Pane, "pid": b.PID, "start": b.Start, "claude_session": b.Session, "outcome": outcome}
		if what == "send" {
			detail["length"] = len(text) // (what was said is not kept)
		}
		if reason != "" {
			detail["reason"] = reason
		}
		s.audit(r.WithContext(actx), keys.audit, sess.HostName, detail)
	}()
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		reason = "signed_out"
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	ctx, cancelStep := context.WithTimeout(ctx, 15*time.Second)
	defer cancelStep()
	untrack := s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()
	// A login ended meanwhile cancels the request: say that, not "the host".
	signedOut := func() bool {
		if _, err := s.auth.CheckLogin(context.WithoutCancel(r.Context()), loginHash(r)); err != nil {
			reason = "signed_out"
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
			return true
		}
		return false
	}
	// (A message: one at a time to a pane, from the check to its Enter;
	// once its command is on its way, given back when that has ended.)
	turn := func() {}
	if what == "send" {
		release, ok := convoSending.take(ctx, sess.ID+"\x00"+b.Pane)
		if !ok {
			reason = "busy"
			writeError(w, http.StatusTooManyRequests, "busy", "another message to this agent is still being sent")
			return
		}
		turn = release
	}
	defer func() { turn() }()

	out, err := sshx.Output(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession), convoLocateMax)
	if errors.Is(err, sshx.ErrBusy) {
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
		return
	}
	// (A scan cut short, by its size or a failure, may leave out the agent
	// started last: what it found does not decide where keys go.)
	if err != nil {
		if signedOut() {
			return
		}
		reason = "host_error"
		writeError(w, http.StatusBadGateway, "host_error", "the host did not answer")
		return
	}
	// The process must be the one in front in that pane now, the one the
	// pane's conversation shows (the latest started there): a Claude Code
	// started inside it since is not the one the stop was asked for.
	procs, _, tmuxPID, _, _ := parseConvoLocate(string(out))
	socket := s.herdrSocks.get(sess.ID)
	var front *convoProc
	for i := range procs {
		p := &procs[i]
		inPane := (sess.Persistence == "herdr" && socket != "" && p.herdrPane == b.Pane && p.herdrSocket == socket) ||
			(sess.Persistence == "tmux" && tmuxPID != "" && p.tmuxPane == b.Pane && p.tmuxServerPID == tmuxPID)
		if inPane && (front == nil || p.start > front.start) {
			front = p
		}
	}
	if front == nil || front.pid != b.PID || front.start != b.Start || front.sid != b.Session {
		reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	}
	// (Stopped with Ctrl+Z, or another program in front of it: keys typed
	// there would go to that.)
	if !front.fg {
		reason = "not_in_front"
		writeError(w, http.StatusConflict, "not_in_front", "the agent is not in front in its pane")
		return
	}
	if signedOut() {
		return
	}
	if what == "send" {
		// The check again and the typing, on the host in one go (see convoSendCommand).
		release := turn
		turn = func() {} // (Exec gives it back, once the channel has ended)
		_, err = sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, b.Pane, socket, b.PID, b.Start, len(text)), 4096,
			sshx.ExecOptions{Stdin: strings.NewReader(text), Release: release})
		var exitErr *sshx.ExitError
		switch {
		case err == nil:
			outcome = "typed"
		case errors.Is(err, sshx.ErrBusy):
			reason = "busy" // (not started: nothing was typed)
		case errors.As(err, &exitErr) && exitErr.Status == 3:
			reason = "agent_changed"
			writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
			return
		case errors.As(err, &exitErr) && exitErr.Status == 4:
			reason = "send_failed" // (nothing typed)
		default:
			outcome = "unknown" // (typed in part, or all of it but Enter, or it is not known)
		}
	} else if sess.Persistence == "herdr" {
		c, derr := herdr.Dial(ctx, sess.Client(), socket)
		if derr != nil {
			if signedOut() {
				return
			}
			reason = "herdr_error"
			writeHerdrError(w, derr)
			return
		}
		err = c.SendKeys(ctx, b.Pane, keys.herdr)
		c.Close()
		var apiErr *herdr.APIError
		switch {
		case err == nil:
			outcome = "typed"
		case errors.As(err, &apiErr):
			outcome, reason = "refused", "herdr_refused"
		default:
			outcome = "unknown"
		}
	} else {
		// (The script checks the pane is in this session's tmux session, and presses.)
		_, err = sshx.Output(ctx, sess.Client(), tmuxKeysCommand(sess.TmuxSession, b.Pane, keys.tmux), 4096)
		var exitErr *sshx.ExitError
		switch {
		case err == nil:
			outcome = "typed"
		case errors.Is(err, sshx.ErrBusy):
			reason = "busy" // (not started: nothing was pressed)
		case errors.As(err, &exitErr) && exitErr.Status == 3:
			reason = "not_in_session"
		case errors.As(err, &exitErr):
			reason = "send_failed" // (tmux said no: the pane went, say)
		default:
			outcome = "unknown"
		}
	}
	switch {
	case outcome == "typed":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case reason == "busy":
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case outcome == "refused":
		writeError(w, http.StatusConflict, "refused", "the pane did not take the key")
	default:
		if signedOut() {
			return
		}
		writeError(w, http.StatusBadGateway, "unknown", "it is not known whether the key arrived")
	}
}
