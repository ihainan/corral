package web

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// What the host says of the Codexes in a herdr server's panes, in one
// short look (codexPanesScript): each one's process, the folder it runs in,
// the thread it is at — the one it was resumed at (`codex resume <id>`) or
// the log it has open — that thread's log, and what Codex's own database
// has of its name. A Codex run against its app-server daemon keeps no log
// open and is told nothing of by herdr (its SessionStart hook runs in the
// daemon, with no pane), so the conversation it is at was found only by a
// scan of every process on the host and the daemon's logs, seconds a
// time; this is a few milliseconds there. Kept a few seconds (codexPanesTTL)
// per terminal session; looks at once share one.

const (
	codexPanesTTL = 3 * time.Second
	codexPanesMax = 128 << 10 // (the host's answer, at most)
)

// codexPane is one interactive Codex of a pane, as the host told it.
type codexPane struct {
	PID, Start int64 // its process and when it began (clock ticks since boot, /proc/<pid>/stat)
	Started    int64 // when it began, unix ms (0: not known)
	Cwd        string
	Thread     string // the thread it was resumed at or has the log of open ("" none known)
	Path       string // that thread's log ("" none found)
	Name       string // Codex's database: the thread's name, title, first message ("" none)
	Title      string
	First      string
	// Several: the pane runs more than one interactive Codex (one inside
	// another's command, say): which the pane shows is not told.
	Several bool
}

// codexPanesCommand prints a record for each interactive Codex of the
// user's on the herdr server at socket (see parseCodexPanes). Nothing
// where the host has no python3.
func codexPanesCommand(socket string) string {
	return `command -v python3 >/dev/null 2>&1 || exit 0
exec python3 -I -c ` + sshx.QuoteShell(codexPanesScript) + ` ` + sshx.QuoteShell(socket)
}

// (A record: pane, pid, start, started ms, cwd, thread, log, name, title,
// first message; split by \x1f, ended by \0. The database: the newest
// state_<n>.sqlite of its CODEX_HOME, read only; the log, the one it has
// open, else the database's rollout_path, else the newest of
// sessions/*/*/*/rollout-*-<thread>.jsonl.)
const codexPanesScript = `import glob, os, re, sqlite3, sys
sock = sys.argv[1]
uid = os.getuid()
UUID = re.compile(r'[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}')
TAKES = re.compile(r'^(-[cmpsaCi]|--(config|model|profile|sandbox|ask-for-approval|cd|image|add-dir|local-provider|remote|remote-auth-token-env|enable|disable))$')
try:
    hz = os.sysconf('SC_CLK_TCK')
    btime = int([l.split()[1] for l in open('/proc/stat') if l.startswith('btime ')][0])
except Exception:
    hz, btime = 0, 0
def sub_and_id(args):
    pos, skip = [], False
    for a in args:
        if skip:
            skip = False
            continue
        if a == '--':
            break
        if a.startswith('-'):
            skip = bool(TAKES.match(a))
            continue
        pos.append(a)
        if len(pos) == 2:
            break
    sub = pos[0] if pos else ''
    tid = pos[1] if sub == 'resume' and len(pos) > 1 and UUID.fullmatch(pos[1]) else ''
    return sub, tid
found = []
for d in os.listdir('/proc'):
    if not d.isdigit():
        continue
    p = '/proc/' + d
    try:
        if os.stat(p).st_uid != uid:
            continue
        with open(p + '/comm') as f:
            if f.read().strip() != 'codex':
                continue
        with open(p + '/environ', 'rb') as f:
            env = dict(x.split(b'=', 1) for x in f.read().split(b'\0') if b'=' in x)
        with open(p + '/cmdline', 'rb') as f:
            args = [a.decode('utf-8', 'replace') for a in f.read().split(b'\0')[1:]]
        with open(p + '/stat') as f:
            st = f.read()
        start = int(st[st.rindex(') ') + 2:].split()[19])
        cwd = os.readlink(p + '/cwd')
    except (OSError, ValueError, IndexError):
        continue
    if env.get(b'HERDR_SOCKET_PATH', b'').decode('utf-8', 'replace') != sock:
        continue
    pane = env.get(b'HERDR_PANE_ID', b'').decode('utf-8', 'replace')
    if not pane or len(pane) > 64:
        continue
    sub, tid = sub_and_id(args)
    if sub not in ('', 'resume'):
        continue
    home = env.get(b'CODEX_HOME', b'').decode('utf-8', 'replace') or os.path.join(env.get(b'HOME', b'').decode('utf-8', 'replace') or os.path.expanduser('~'), '.codex')
    home = home.rstrip('/')
    opened = []
    try:
        fds = os.listdir(p + '/fd')
    except OSError:
        fds = []
    for fd in fds:
        try:
            l = os.readlink(p + '/fd/' + fd)
        except OSError:
            continue
        if l.startswith(home + '/sessions/') and l.endswith('.jsonl'):
            m = UUID.search(os.path.basename(l))
            try:
                t = os.stat(l).st_mtime
            except OSError:
                continue
            if m:
                opened.append((t, m.group(0), l))
    started = (btime * 1000 + start * 1000 // hz) if hz and btime else 0
    found.append([pane, d, str(start), str(started), cwd, home, tid, opened])
def db_of(home):
    best, n = None, -1
    try:
        names = os.listdir(home)
    except OSError:
        return None
    for name in names:
        m = re.fullmatch(r'state_(\d+)\.sqlite', name)
        if m and int(m.group(1)) > n:
            best, n = os.path.join(home, name), int(m.group(1))
    return best
def clean(s):
    return (s or '')[:2000].replace('\0', ' ').replace('\x1f', ' ')
out = sys.stdout.buffer
dbs = {}
def row_of(con, tid):
    if con is None:
        return None
    try:
        return con.execute('SELECT name, title, first_user_message, rollout_path, source, thread_source FROM threads WHERE id = ?', (tid,)).fetchone()
    except sqlite3.Error:
        return None
def subagent(row):
    return bool(row) and ((row[4] or '').startswith('{') or (row[5] or '') in ('guardian_review', 'subagent'))
for pane, pid, start, started, cwd, home, tid, opened in sorted(found, key=lambda f: f[:6]):
    name = title = first = log = ''
    if home not in dbs:
        db = db_of(home)
        try:
            dbs[home] = sqlite3.connect('file:' + db + '?mode=ro', uri=True, timeout=2) if db else None
        except sqlite3.Error:
            dbs[home] = None
    con = dbs[home]
    # (Of the logs it has open, its own: the newest not a subagent's — a
    # review's, a spawned agent's — else the thread it was resumed at.)
    for t, otid, l in sorted(opened, reverse=True):
        if not subagent(row_of(con, otid)):
            tid, log = otid, l
            break
    # (Begun with no thread named and none open — run against the daemon:
    # the thread of its folder begun since it started that was written to
    # last, not a subagent's. The title, compared by the gateway, says if it
    # is the one shown.)
    if not tid and con is not None and int(started) > 0:
        try:
            for cand in con.execute("SELECT id, source, thread_source FROM threads WHERE cwd = ? AND created_at >= ? ORDER BY updated_at DESC LIMIT 20", (cwd, int(started) // 1000 - 5)):
                if not ((cand[1] or '').startswith('{') or (cand[2] or '') in ('guardian_review', 'subagent')):
                    tid = cand[0]
                    break
        except sqlite3.Error:
            pass
    if tid:
        row = row_of(con, tid)
        if row:
            name, title, first = row[0] or '', row[1] or '', row[2] or ''
            if not log and row[3] and os.path.isfile(row[3]):
                log = row[3]
        if not log:
            logs = glob.glob(glob.escape(home) + '/sessions/*/*/*/rollout-*-' + tid + '.jsonl')
            if logs:
                log = max(logs, key=lambda l: os.stat(l).st_mtime)
    out.write('\x1f'.join([pane, pid, start, started, clean(cwd), tid, clean(log), clean(name), clean(title), clean(first)]).encode('utf-8', 'replace') + b'\0')
`

// codexLogOf: a thread's log as the host named it, where it is one: an
// absolute path, of that thread's rollout (a segment of it too), nothing
// that leaves its folder. Else "".
var codexLogName = regexp.MustCompile(`/rollout-[^/]*-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:_\d+)?\.jsonl$`)

func codexLogOf(path, thread string) string {
	m := codexLogName.FindStringSubmatch(path)
	if m == nil || !strings.HasPrefix(path, "/") || strings.Contains(path, "/../") || strings.Contains(path, "/./") || !strings.EqualFold(m[1], thread) {
		return ""
	}
	return path
}

// parseCodexPanes reads codexPanesCommand's records: pane → its Codex.
func parseCodexPanes(out string) map[string]codexPane {
	got := map[string]codexPane{}
	for _, rec := range strings.Split(out, "\x00") {
		f := strings.Split(rec, "\x1f")
		if len(f) != 10 || !herdrPaneID.MatchString(f[0]) {
			continue
		}
		pid, err1 := strconv.ParseInt(f[1], 10, 64)
		start, err2 := strconv.ParseInt(f[2], 10, 64)
		started, _ := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil || pid <= 0 || start <= 0 {
			continue
		}
		c := codexPane{PID: pid, Start: start, Started: max(started, 0), Cwd: f[4], Name: f[7], Title: f[8], First: f[9]}
		if convoUUID.MatchString(f[5]) {
			c.Thread = f[5]
			c.Path = codexLogOf(f[6], c.Thread)
		}
		if _, ok := got[f[0]]; ok {
			c = codexPane{Several: true}
		}
		got[f[0]] = c
	}
	return got
}

// codexNames: pane → the name its thread is shown by (see codexName).
func codexNames(panes map[string]codexPane) map[string]string {
	got := map[string]string{}
	for p, c := range panes {
		if c.Several || c.Thread == "" {
			continue
		}
		if n := codexName(c.Name, c.Title, c.First); n != "" {
			got[p] = n
		}
	}
	return got
}

// codexPaneLooks keeps, per terminal session, the last look (see above).
type codexPaneLooks struct {
	mu sync.Mutex
	by map[string]*codexPaneLook
}

type codexPaneLook struct {
	at   time.Time
	done chan struct{}
	got  map[string]codexPane
	err  error
}

// of: what the host says of the Codexes of sess's herdr server (socket),
// from a look begun within codexPanesTTL, or one begun now.
func (c *codexPaneLooks) of(ctx context.Context, sess *term.Session, socket string) (map[string]codexPane, error) {
	c.mu.Lock()
	if c.by == nil {
		c.by = map[string]*codexPaneLook{}
	}
	l := c.by[sess.ID]
	if l == nil || time.Since(l.at) > codexPanesTTL {
		l = &codexPaneLook{at: time.Now(), done: make(chan struct{})}
		c.by[sess.ID] = l
		// (Not the asker's context: another may share it.)
		go func() {
			lctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			out, err := sshx.Output(lctx, sess.Client(), codexPanesCommand(socket), codexPanesMax)
			if err == nil {
				l.got = parseCodexPanes(string(out))
			}
			l.err = err
			if err != nil {
				c.mu.Lock()
				if c.by[sess.ID] == l {
					delete(c.by, sess.ID) // (not kept: the next asks again)
				}
				c.mu.Unlock()
			}
			close(l.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-l.done:
		return l.got, l.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// forget: what was kept of a terminal session (closed).
func (c *codexPaneLooks) forget(id string) {
	c.mu.Lock()
	delete(c.by, id)
	c.mu.Unlock()
}

// codexTitleAgrees: a Codex pane's terminal title is of the thread named
// name (Codex's default title: "<name> | <folder>", or the folder alone
// while the thread has no name). A title of another form, or of another
// name — a thread begun (/new) or resumed (/resume) in it since it was
// started — does not.
func codexTitleAgrees(title, cwd, name string) bool {
	thread, bare := codexTitleParts("codex", title, cwd)
	if bare {
		return name == ""
	}
	if thread == "" || name == "" {
		return false
	}
	if thread == name {
		return true
	}
	for _, cut := range []string{"...", "…"} {
		if head, ok := strings.CutSuffix(thread, cut); ok && head != "" && strings.HasPrefix(name, head) {
			return true
		}
	}
	return false
}

// locateCodexFast binds pane to its Codex as the host told it in a short
// look (see above), where that tells it for sure: herdr says the pane runs a
// Codex; the host finds one interactive Codex there, at a thread whose log
// it found, no other pane at that thread; the pane's title is of that
// thread (not of one begun or resumed in it since). Else false: the scan
// tells it, as before.
func (s *Server) locateCodexFast(ctx context.Context, sess *term.Session, st *convoState, pane string) bool {
	hp, ok := s.herdrSocks.paneOf(sess.ID, pane)
	socket := s.herdrSocks.get(sess.ID)
	if !ok || !isCodexAgent(hp.Agent) || socket == "" {
		return false
	}
	panes, err := s.codexPanes.of(ctx, sess, socket)
	if err != nil {
		return false
	}
	c, ok := panes[pane]
	if !ok || c.Several || c.Thread == "" || c.Path == "" || !codexTitleAgrees(hp.Title, c.Cwd, c.Name) {
		return false
	}
	for p, o := range panes {
		if p != pane && o.Thread == c.Thread {
			return false // (two at one thread: which is which, the scan tells)
		}
	}
	b := &convoBind{PID: c.PID, Start: c.Start, Session: c.Thread, Agent: "codex", Pane: pane, started: c.Started}
	if was := st.bind; was != nil && was.PID == b.PID && was.Start == b.Start {
		b.Via, b.Auth = was.Via, was.Auth // (as the scan found them: not told by this look)
	}
	st.bind = b
	st.located, st.pane = time.Now(), pane
	st.guessed = false
	st.adopt(c.Thread, c.Path, "codex", "")
	return true
}
