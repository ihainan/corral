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
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
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
	convoReadLimit = 15 * time.Second // a read the host has not answered by then is given up
)

// convoLocateCommand prints, for each Claude Code process of the user that
// is still running, a NUL-terminated record of key=value lines: its pid, its
// start time (/proc/<pid>/stat field 22) and the one its session file
// records (a file left by a process that ended, whose pid was then reused,
// does not match), the session id, the log found for it, and the herdr or
// tmux pane it runs in. With mode "tmux" a first record gives the active
// pane of the named tmux session and its server's pid. Linux only (/proc):
// elsewhere it prints the record "noproc". Fixed script; the mode and the
// validated tmux session name are its only arguments.
func convoLocateCommand(mode, tmuxName string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then
  t=$(tmux -u display-message -p -t "=$2:" '#{pane_id} #{pid}' 2>/dev/null)
  printf 'tmux=%s\0' "$t"
fi
[ -r /proc/self/stat ] || { printf 'noproc\0'; exit 0; }
for f in "$HOME"/.claude/sessions/*.json; do
  [ -f "$f" ] || continue
  pid=${f##*/}; pid=${pid%.json}
  case $pid in ''|*[!0-9]*) continue;; esac
  [ -r "/proc/$pid/environ" ] || continue
  start=$(sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null | cut -d' ' -f20)
  c=$(head -c 65536 "$f" 2>/dev/null | tr -d '\n')
  sid=$(printf '%s' "$c" | grep -o '"sessionId" *: *"[0-9a-fA-F-]*"' | head -n 1 | sed 's/.*"\([0-9a-fA-F-]*\)"$/\1/')
  ps=$(printf '%s' "$c" | grep -o '"procStart" *: *"*[0-9]*' | head -n 1 | sed 's/[^0-9]*\([0-9]*\)$/\1/')
  case $sid in ????????-????-????-????-????????????) ;; *) continue;; esac
  log=
  for l in "$HOME"/.claude/projects/*/"$sid".jsonl; do [ -f "$l" ] && { log=$l; break; }; done
  env=$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | grep -E '^(HERDR_PANE_ID|HERDR_SOCKET_PATH|TMUX|TMUX_PANE)=' | head -c 4096)
  printf 'pid=%s\nstart=%s\nprocstart=%s\nsid=%s\nlog=%s\n%s\0' "$pid" "$start" "$ps" "$sid" "$log" "$env"
done
exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(tmuxName)
}

type convoProc struct {
	pid, start    int64
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
func parseConvoLocate(out string) (procs []convoProc, tmuxActive, tmuxPID string, noProc bool) {
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "noproc" {
			noProc = true
			continue
		}
		if v, ok := strings.CutPrefix(rec, "tmux="); ok {
			if pane, pid, ok := strings.Cut(strings.TrimSpace(v), " "); ok && tmuxPane.MatchString(pane) && isDigits(pid) {
				tmuxActive, tmuxPID = pane, pid
			}
			continue
		}
		var p convoProc
		var procStart int64 = -1
		for _, line := range strings.Split(rec, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch k {
			case "pid":
				p.pid, _ = strconv.ParseInt(v, 10, 64)
			case "start":
				p.start, _ = strconv.ParseInt(v, 10, 64)
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
		if l, ok := cleanRemotePath(p.log); !ok || !strings.HasSuffix(l, "/"+p.sid+".jsonl") {
			p.log = ""
		}
		procs = append(procs, p)
	}
	return procs, tmuxActive, tmuxPID, noProc
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

// convoState is what the gateway knows of one session's conversation.
type convoState struct {
	mu       sync.Mutex
	used     time.Time
	located  time.Time
	pane     string // (herdr) the pane it was located for
	reason   string // why there is nothing to read, when there is not
	sid, log string
	gen      int // changes when another log is read: what was shown does not apply
	parser   *convo.Claude
	offset   int64 // read up to here
	size     int64 // the log's size when last looked at
	mtime    time.Time
	older    bool // the log has more before what was read
	more     bool // the last read stopped before the end
	skipping bool // in a line too long to keep, until its end
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
	if !ok {
		st = &convoState{}
		c.m[id] = st
	}
	return st
}

func (st *convoState) idle() bool {
	if !st.mu.TryLock() {
		return false
	}
	defer st.mu.Unlock()
	return time.Since(st.used) > convoIdle
}

func (c *convoStates) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}

// Reasons there is nothing to show.
const (
	convoNoAgent     = "no_agent"    // no Claude Code in the pane
	convoNoLog       = "no_log"      // it has not written its log yet (a new conversation)
	convoUnsupported = "unsupported" // the host has no /proc, or the session is a plain shell
)

type convoAnswer struct {
	Available bool          `json:"available"`
	Reason    string        `json:"reason,omitempty"`
	Gen       int           `json:"gen"`
	Rev       int           `json:"rev"`
	Reset     bool          `json:"reset,omitempty"` // drop what was shown: these are all the entries
	Older     bool          `json:"older,omitempty"` // the conversation began before the first entry
	More      bool          `json:"more,omitempty"`  // more of the log is to be read: ask again soon
	Entries   []convo.Entry `json:"entries"`
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
	gen, _ := strconv.Atoi(q.Get("gen"))
	rev, _ := strconv.Atoi(q.Get("rev"))
	w.Header().Set("Cache-Control", "no-store")
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeJSON(w, http.StatusOK, convoAnswer{Reason: convoUnsupported, Entries: []convo.Entry{}})
		return
	}
	st := s.convos.get(sess.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.used = time.Now()
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
	ans := convoAnswer{Gen: st.gen, Entries: []convo.Entry{}}
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
	ans.Available, ans.Gen, ans.Rev, ans.Older, ans.More = true, st.gen, st.parser.Rev(), st.older, more
	if gen != st.gen {
		ans.Reset = true
		rev = 0
	}
	ans.Entries = st.parser.Since(rev)
	writeJSON(w, http.StatusOK, ans)
}

// locateConvo finds the log of the Claude Code session in the pane; a
// different one from before starts a new generation.
func (s *Server) locateConvo(ctx context.Context, sess *term.Session, st *convoState, pane string) error {
	out, err := sshx.Output(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession), convoLocateMax)
	if err != nil && (errors.Is(err, sshx.ErrBusy) || len(out) == 0) {
		return err
	}
	procs, tmuxActive, tmuxPID, noProc := parseConvoLocate(string(out))
	st.located, st.pane = time.Now(), pane
	sid, log, reason := "", "", ""
	switch {
	case noProc:
		reason = convoUnsupported
	default:
		p := pickConvoProc(procs, sess.Persistence, pane, s.herdrSocks.get(sess.ID), tmuxActive, tmuxPID)
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
		st.gen++
		st.sid, st.log = sid, log
		st.parser = convo.NewClaude()
		st.offset, st.size, st.mtime, st.older, st.skipping = 0, -1, time.Time{}, false, false
	}
	st.reason = reason
	return nil
}

// readConvo reads what the log gained since the last read: from the end at
// first, then forward. A log that got shorter was replaced and is read
// again. more says one request did not take all there is.
func (s *Server) readConvo(sess *term.Session, st *convoState) (more bool, err error) {
	err = s.withSFTP(context.Background(), sess, convoReadLimit, func(c *sftplite.Client) error {
		fi, err := c.Stat(st.log)
		if err != nil {
			return err
		}
		size := fi.Size()
		if size == st.size && fi.ModTime().Equal(st.mtime) && !st.more {
			return nil
		}
		if size < st.offset {
			st.gen++
			st.parser = convo.NewClaude()
			st.offset, st.older, st.skipping = 0, false, false
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
