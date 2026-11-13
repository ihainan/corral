package web

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
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
	// codexWeakFirst: a weak binding (see convoState.weak) is checked by the
	// scan beside this soon after it is made.
	codexWeakFirst = 3 * time.Second
	codexPanesTTL  = 3 * time.Second
	codexPanesMax  = 128 << 10 // (the host's answer, at most)
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
	FG         bool   // in front on its terminal: what is typed there reaches it
	Via        string // where its requests go, as the scan says it (codex_ep): the host alone; "" OpenAI's own
	viaKnown   bool   // (the look said "=": the gateway's own, kept from before)
	// Fresh: run with no subcommand, no log open, no conversation of its
	// folder begun since it started — its own not begun yet.
	Fresh bool
	// Src: what told its thread ("fd", "argv", "db"); Unexplained: how many
	// conversations of its folder were talked in since it started that no
	// Codex of the user's is told at (one begun or resumed in it since,
	// maybe: then an unnamed thread of its is not taken for sure).
	Src         string
	Unexplained int
	// SameName: other live conversations of its folder of its name (a title
	// tells none of them apart); TitleNow: the pane's title read at the look
	// (asked for: see codexPanesCommand), titleRead whether it was.
	SameName  int
	TitleNow  string
	titleRead bool
	// Several: the pane runs more than one interactive Codex (one inside
	// another's command, say): which the pane shows is not told.
	Several bool
}

// codexPanesCommand prints a record for each interactive Codex of the
// user's on the herdr server at socket (see parseCodexPanes). Nothing
// where the host has no python3.
// known: the processes ("<pid>:<start>") whose endpoint the gateway has
// already: told "=" for theirs, not worked out again.
// target: a pane whose terminal title is read now (herdr's, by herdr pane
// get), not as a snapshot had it; "" none.
func codexPanesCommand(socket string, known []string, target string) string {
	return `command -v python3 >/dev/null 2>&1 || exit 0
exec python3 -I -c ` + sshx.QuoteShell(codexPanesScript) + ` ` + sshx.QuoteShell(socket) + ` ` + sshx.QuoteShell(convoEndpointSh) + ` ` + sshx.QuoteShell(strings.Join(known, ",")) + ` ` + sshx.QuoteShell(target)
}

// (A record: pane, pid, start, started ms, cwd, thread, log, name, title,
// first message, in front ("1": its group the terminal's foreground),
// fresh ("1": no subcommand, no log open, no conversation of its folder
// begun since it started), where the thread was told by ("fd": a log it has
// open; "argv": its resume argument; "db": the one conversation of its
// folder begun since it started), how many conversations of its folder
// were talked in since it started that neither it nor any other Codex of
// the user's is told at (unexplained), where its requests go (codex_ep:
// the host alone, "" OpenAI's own; "=" one the gateway named as known);
// split by \x1f, ended by \0. The database: the newest
// state_<n>.sqlite of its CODEX_HOME, read only; the log, the one it has
// open, else the database's rollout_path, else the newest of
// sessions/*/*/*/rollout-*-<thread>.jsonl.)
const codexPanesScript = `import glob, json, os, re, sqlite3, subprocess, sys
sock = sys.argv[1]
endpoint_sh = sys.argv[2]  # (codex_ep and what it uses: the scan's own, run for each)
known = set(sys.argv[3].split(',')) if len(sys.argv) > 3 and sys.argv[3] else set()
target = sys.argv[4] if len(sys.argv) > 4 else ''  # (a pane whose title is read now, as herdr has it: a press's)
def title_now(pane):
    env = dict(os.environ, HERDR_SOCKET_PATH=sock, PATH=os.path.expanduser('~/.local/bin') + ':' + os.path.expanduser('~/bin') + ':/usr/local/bin:' + os.environ.get('PATH', ''))
    try:
        r = subprocess.run(['herdr', 'pane', 'get', pane], capture_output=True, timeout=5, env=env)
        j = json.loads(r.stdout)
        res = j.get('result') or {}
        p = res.get('pane') or res
        t = p.get('terminal_title_stripped')
        return t if isinstance(t, str) else None
    except Exception:
        return None
uid = os.getuid()
UUID = re.compile(r'[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}')
TAKES = re.compile(r'^(-[cmpsaCi]|--(config|model|profile|sandbox|ask-for-approval|cd|image|add-dir|local-provider|remote|remote-auth-token-env|enable|disable))$')
TALKED = "source IN ('cli', 'vscode') AND COALESCE(thread_source, '') NOT IN ('guardian_review', 'subagent')"
try:
    hz = os.sysconf('SC_CLK_TCK')
    btime = int([l.split()[1] for l in open('/proc/stat') if l.startswith('btime ')][0])
except Exception:
    hz, btime = 0, 0
def endpoint(pid, start):
    if pid + ':' + start in known:
        return '='
    try:
        r = subprocess.run(['sh', '-c', endpoint_sh + '\ncodex_ep "$1"; printf %s "$ep"', 'sh', pid], capture_output=True, timeout=3)
        return r.stdout.decode('utf-8', 'replace')[:300]
    except Exception:
        return ''
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
def under_herdr(pid):
    for _ in range(40):
        try:
            with open('/proc/' + pid + '/stat') as f:
                st = f.read()
            pid = st[st.rindex(') ') + 2:].split()[1]
            if pid in ('0', '1'):
                return False
            with open('/proc/' + pid + '/comm') as f:
                if f.read().strip() == 'herdr':
                    return True
        except (OSError, ValueError, IndexError):
            return False
    return False
dbs = {}
def con_of(home):
    if home in dbs:
        return dbs[home]
    best, n = None, -1
    try:
        names = os.listdir(home)
    except OSError:
        names = []
    for name in names:
        m = re.fullmatch(r'state_(\d+)\.sqlite', name)
        if m and int(m.group(1)) > n:
            best, n = os.path.join(home, name), int(m.group(1))
    try:
        dbs[home] = sqlite3.connect('file:' + best + '?mode=ro', uri=True, timeout=2) if best else None
    except sqlite3.Error:
        dbs[home] = None
    return dbs[home]
def row_of(con, tid):
    if con is None:
        return None
    try:
        return con.execute('SELECT name, title, first_user_message, rollout_path, source, thread_source FROM threads WHERE id = ?', (tid,)).fetchone()
    except sqlite3.Error:
        return None
def subagent(con, tid, log):
    # (By the database; where it has no row, by the log's head, as the scan reads it.)
    row = row_of(con, tid)
    if row:
        return (row[4] or '').startswith('{') or (row[5] or '') in ('guardian_review', 'subagent')
    try:
        with open(log, 'rb') as f:
            head = f.read(8192).split(b'\n', 1)[0]
        meta = json.loads(head).get('payload') or {}
        src = meta.get('source')
        return isinstance(src, dict) or meta.get('thread_source') in ('guardian_review', 'subagent')
    except Exception:
        return True  # (not told: not taken for its own)
def environ(p):
    with open(p + '/environ', 'rb') as f:
        return dict(x.split(b'=', 1) for x in f.read().split(b'\0') if b'=' in x)
# Every interactive Codex of the user's, in any pane or none: the threads it
# is told at (its resume argument, the logs of its own it has open) — what
# a thread touched in a folder is accounted for by.
procs = []
held = set()  # (home, thread): those whose logs a Codex of the user's — a daemon too — holds open: live now
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
        for fd in os.listdir(p + '/fd'):
            try:
                l = os.readlink(p + '/fd/' + fd)
            except OSError:
                continue
            if '/rollout-' in l and l.endswith('.jsonl') and '/sessions/' in l:
                m = UUID.search(os.path.basename(l))
                if m:
                    held.add((l.rsplit('/sessions/', 1)[0], m.group(0)))
        env = environ(p)
        with open(p + '/cmdline', 'rb') as f:
            args = [a.decode('utf-8', 'replace') for a in f.read().split(b'\0')[1:]]
        with open(p + '/stat') as f:
            st = f.read()
        rest = st[st.rindex(') ') + 2:].split()
        start = int(rest[19])
        fg = '1' if rest[2] == rest[5] else '0'  # (its group the terminal's in front)
        cwd = os.readlink(p + '/cwd')
    except (OSError, ValueError, IndexError):
        continue
    sub, tid = sub_and_id(args)
    if sub not in ('', 'resume'):
        continue
    home = env.get(b'CODEX_HOME', b'').decode('utf-8', 'replace') or os.path.join(env.get(b'HOME', b'').decode('utf-8', 'replace') or os.path.expanduser('~'), '.codex')
    home = home.rstrip('/')
    opened, any_log = [], False
    try:
        fds = os.listdir(p + '/fd')
    except OSError:
        fds = []
    for fd in fds:
        try:
            l = os.readlink(p + '/fd/' + fd)
        except OSError:
            continue
        if '/rollout-' in l and l.endswith('.jsonl'):
            any_log = True
        if l.startswith(home + '/sessions/') and l.endswith('.jsonl'):
            m = UUID.search(os.path.basename(l))
            try:
                t = os.stat(l).st_mtime
            except OSError:
                continue
            if m:
                opened.append((t, m.group(0), l))
    started = (btime * 1000 + start * 1000 // hz) if hz and btime else 0
    mine = env.get(b'HERDR_SOCKET_PATH', b'').decode('utf-8', 'replace') == sock
    pane = env.get(b'HERDR_PANE_ID', b'').decode('utf-8', 'replace') if mine else ''
    procs.append({'pid': d, 'start': str(start), 'started': started, 'cwd': cwd, 'home': home, 'argv': tid, 'opened': opened,
                  'fg': fg, 'sub': sub, 'any': any_log, 'pane': pane})
claims = {}
for q in procs:
    con = con_of(q['home'])
    own = {(q['home'], tid) for _, tid, l in q['opened'] if not subagent(con, tid, l)}
    if q['argv']:
        own.add((q['home'], q['argv']))
    q['own'] = own
def clean(s):
    return (s or '')[:2000].replace('\0', ' ').replace('\x1f', ' ')
out = sys.stdout.buffer
for q in sorted(procs, key=lambda q: (q['pane'], q['pid'])):
    pane = q['pane']
    if not pane or len(pane) > 64 or not under_herdr(q['pid']):
        continue
    con = con_of(q['home'])
    tid, log, src = '', '', ''
    # (Its own: the newest log of its own it has open — a review's, a spawned
    # agent's left out — else the thread it was resumed at.)
    for t, otid, l in sorted(q['opened'], reverse=True):
        if not subagent(con, otid, l):
            tid, log, src = otid, l, 'fd'
            break
    if not tid and q['argv']:
        tid, src = q['argv'], 'argv'
    # Conversations of its folder talked in since it started (begun, or
    # written to: a resume writes), live now: those no other Codex is told
    # at, but its own, are unexplained — one begun or resumed in it since,
    # maybe.
    # (No database, or no time it started: not known — not none.)
    touched, begun = None, None
    if con is not None and q['started'] > 0:
        try:
            since = q['started'] // 1000 - 5
            touched = [r[0] for r in con.execute('SELECT id, created_at FROM threads WHERE cwd = ? AND (created_at >= ? OR updated_at >= ?) AND ' + TALKED, (q['cwd'], since, since))]
            begun = [r[0] for r in con.execute('SELECT id FROM threads WHERE cwd = ? AND created_at >= ? AND ' + TALKED, (q['cwd'], since))]
        except sqlite3.Error:
            touched, begun = None, None
    others = set()
    for o in procs:
        if o is not q:
            others |= o['own']
    # (Begun with no thread told — run against the daemon: the one
    # conversation of its folder begun since it started, where there is one.)
    if not tid and begun is not None and len(begun) == 1 and (q['home'], begun[0]) not in others:
        tid, src = begun[0], 'db'
    # (Only those live now — their logs held: one this Codex was resumed at
    # or begun in since is; one of a Codex that has ended since is not.)
    unexplained = 99 if touched is None else len([x for x in touched if x != tid and (q['home'], x) not in others and (q['home'], x) in held])
    name = title = first = ''
    if tid:
        row = row_of(con, tid)
        if row:
            name, title, first = row[0] or '', row[1] or '', row[2] or ''
            if not log and row[3] and os.path.isfile(row[3]):
                log = row[3]
        if not log:
            logs = glob.glob(glob.escape(q['home']) + '/sessions/*/*/*/rollout-*-' + tid + '.jsonl')
            if logs:
                log = max(logs, key=lambda l: os.stat(l).st_mtime)
    # (Live conversations of its folder no Codex is told at — one it was
    # resumed in, maybe, whenever begun: by what is held now, not by time.)
    live_other = None
    if con is not None:
        try:
            live_other = 0
            for h, x in held:
                if h == q['home'] and x != tid and (h, x) not in others:
                    r = con.execute('SELECT cwd FROM threads WHERE id = ? AND ' + TALKED, (x,)).fetchone()
                    if r and r[0] == q['cwd']:
                        live_other += 1
        except sqlite3.Error:
            live_other = None
    fresh = '1' if q['sub'] == '' and not q['any'] and begun is not None and len(begun) == 0 and live_other == 0 else '0'
    # (Of its name, other live conversations of its folder: a title is no word on which of them.)
    same = 0
    if name:
        try:
            same = len([r[0] for r in con.execute('SELECT id FROM threads WHERE cwd = ? AND name = ? AND id != ? AND ' + TALKED, (q['cwd'], name, tid)) if (q['home'], r[0]) in held])
        except sqlite3.Error:
            same = 99
    now_title = title_now(pane) if pane == target else None
    out.write('\x1f'.join([pane, q['pid'], q['start'], str(q['started']), clean(q['cwd']), tid, clean(log), clean(name), clean(title), clean(first),
                           q['fg'], fresh, src, str(unexplained), clean(endpoint(q['pid'], q['start'])), str(same),
                           '\x02' if now_title is None else clean(now_title)]).encode('utf-8', 'replace') + b'\0')
`

// codexLogOf: a thread's log as the host named it, where it is one: an
// absolute path, of that thread's rollout (a segment of it too), nothing
// that leaves its folder. Else "".
var codexLogName = regexp.MustCompile(`/rollout-[^/]*-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:_\d+)?\.jsonl$`)

// hostOnly: a host as codex_ep gives it (a name or address, a port).
var hostOnly = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]{1,253}$`)

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
		if len(f) != 17 || !herdrPaneID.MatchString(f[0]) {
			continue
		}
		pid, err1 := strconv.ParseInt(f[1], 10, 64)
		start, err2 := strconv.ParseInt(f[2], 10, 64)
		started, _ := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil || pid <= 0 || start <= 0 {
			continue
		}
		unexplained, err3 := strconv.Atoi(f[13])
		if err3 != nil || unexplained < 0 {
			unexplained = 99
		}
		c := codexPane{PID: pid, Start: start, Started: max(started, 0), Cwd: f[4], Name: f[7], Title: f[8], First: f[9], FG: f[10] == "1", Fresh: f[11] == "1", Src: f[12], Unexplained: unexplained}
		if same, err := strconv.Atoi(f[15]); err == nil && same >= 0 {
			c.SameName = same
		} else {
			c.SameName = 99
		}
		if f[16] != "\x02" {
			c.TitleNow, c.titleRead = f[16], true
		}
		if f[14] == "=" {
			c.viaKnown = true
		} else if hostOnly.MatchString(f[14]) {
			c.Via = f[14]
		}
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
	// via: per terminal session, where each Codex process's requests go,
	// as a look worked it out (a process's own, for its life), by
	// "<pid>:<start>"; let go of once a look finds it gone.
	via map[string]map[string]string
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
	return c.since(ctx, sess, socket, time.Now().Add(-codexPanesTTL))
}

// since: as of, from a look begun no earlier than after (one begun now
// where the last was before it).
func (c *codexPaneLooks) since(ctx context.Context, sess *term.Session, socket string, after time.Time) (map[string]codexPane, error) {
	c.mu.Lock()
	if c.by == nil {
		c.by = map[string]*codexPaneLook{}
	}
	l := c.by[sess.ID]
	if l == nil || l.at.Before(after) {
		l = &codexPaneLook{at: time.Now(), done: make(chan struct{})}
		c.by[sess.ID] = l
		// (Not the asker's context: another may share it.)
		go func() {
			lctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			got, err := c.run(lctx, sess, socket, "")
			l.got = got
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

// run: one look (target: a pane whose title is read now, "" none), the
// endpoints the gateway keeps named to it and kept from it.
func (c *codexPaneLooks) run(ctx context.Context, sess *term.Session, socket, target string) (map[string]codexPane, error) {
	c.mu.Lock()
	kept := c.via[sess.ID]
	known := make([]string, 0, len(kept))
	for k := range kept {
		known = append(known, k)
	}
	c.mu.Unlock()
	out, err := sshx.Output(ctx, sess.Client(), codexPanesCommand(socket, known, target), codexPanesMax)
	if err != nil {
		return nil, err
	}
	got := parseCodexPanes(string(out))
	now := map[string]string{}
	for p, x := range got {
		k := strconv.FormatInt(x.PID, 10) + ":" + strconv.FormatInt(x.Start, 10)
		if x.viaKnown {
			x.Via = kept[k]
			got[p] = x
		}
		if !x.Several {
			now[k] = x.Via
		}
	}
	c.mu.Lock()
	if c.via == nil {
		c.via = map[string]map[string]string{}
	}
	c.via[sess.ID] = now // (those not found now: gone)
	c.mu.Unlock()
	return got, nil
}

// forget: what was kept of a terminal session (closed).
func (c *codexPaneLooks) forget(id string) {
	c.mu.Lock()
	delete(c.by, id)
	delete(c.via, id)
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
// codexPaneSure: pane's Codex, as a look found it, where it tells it for
// sure (see locateCodexFast): one interactive Codex there, at a thread no
// other pane is at, with its log; the pane's title (hp, herdr's) of that
// thread.
func codexPaneSure(panes map[string]codexPane, pane string, hp herdr.Pane) (codexPane, bool) {
	c, ok := panes[pane]
	if !ok || c.Several || c.Thread == "" || c.Path == "" || !codexTitleAgrees(hp.Title, c.Cwd, c.Name) {
		return codexPane{}, false
	}
	// (A thread with no name, its title the folder alone, is no word on which
	// it is: taken only where nothing else of its folder was talked in since
	// it started — not one resumed or begun in it since — and never on the
	// folder's say alone (db).)
	if c.Name == "" && (c.Src == "db" || c.Unexplained > 0) {
		return codexPane{}, false
	}
	for p, o := range panes {
		if p != pane && o.Thread == c.Thread {
			return codexPane{}, false // (two at one thread: which is which, the scan tells)
		}
	}
	return c, true
}

// codexPressSure: a pane's Codex as a look found it is sure enough for
// keys to go to without the scan: it has its own log open (a Codex not run
// against the daemon writes its own: the thread it is at), or its thread has
// a name its pane's title shows right now (read at the press, not as a
// snapshot had it) and no other live conversation of its folder is of that
// name. A thread
// told by its resume argument or its folder alone is not: the process it
// was resumed in may be at another since, which nothing but its screen
// tells — the scan's.
func codexPressSure(c codexPane) bool {
	if c.Src == "fd" {
		return true
	}
	// (Its title is its thread's now: one gone to since, named or not, would
	// show its own name or the folder alone. The name whole, not cut short:
	// two names of one beginning cut alike.)
	if c.Name == "" || !c.titleRead || c.SameName != 0 {
		return false
	}
	thread, bare := codexTitleParts("codex", c.TitleNow, c.Cwd)
	return !bare && thread == c.Name
}

// codexFront: the process a press to pane's Codex goes to, told by a look
// begun now (not one kept from before the press), where it is told for
// sure; nil, the scan tells it.
func (s *Server) codexFront(ctx context.Context, sess *term.Session, socket, pane string) *convoProc {
	hp, ok := s.herdrSocks.paneOf(sess.ID, pane)
	if !ok || !isCodexAgent(hp.Agent) || socket == "" || !herdrPaneID.MatchString(pane) {
		return nil
	}
	panes, err := s.codexPanes.run(ctx, sess, socket, pane)
	if err != nil {
		return nil
	}
	c, ok := codexPaneSure(panes, pane, hp)
	if !ok || !codexPressSure(c) {
		return nil
	}
	return &convoProc{pid: c.PID, start: c.Start, fg: c.FG, started: c.Started, sid: c.Thread, log: c.Path, agent: "codex", herdrPane: pane, herdrSocket: socket, cwd: c.Cwd, ep: c.Via}
}

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
	c, ok := codexPaneSure(panes, pane, hp)
	if !ok {
		// (A Codex just started, its conversation not begun: none to read,
		// as the scan would find — told so without it, rather than by a scan
		// at each read till its first message.)
		f, ok := panes[pane]
		if !ok || f.Several || !f.Fresh || f.Thread != "" || f.Unexplained > 0 || !codexTitleAgrees(hp.Title, f.Cwd, "") {
			return false
		}
		b := &convoBind{PID: f.PID, Start: f.Start, Session: convoNoSession, Agent: "codex", Pane: pane, started: f.Started, Via: f.Via}
		st.bind = b
		st.weak = true // (looked for again at each read till its first message: see apiConversation's fresh)
		st.located, st.pane = time.Now(), pane
		st.guessed = false
		st.adopt(convoNoSession, "", "codex", convoNoLog)
		return true
	}
	b := &convoBind{PID: c.PID, Start: c.Start, Session: c.Thread, Agent: "codex", Pane: pane, started: c.Started, Via: c.Via}
	st.bind = b
	st.weak = c.Src != "fd" // (see convoState.weak)
	st.located, st.pane = time.Now(), pane
	if st.weak {
		st.located = time.Now().Add(codexWeakFirst - convoWeakTTL) // (checked by the scan beside soon, not in 30 s)
	}
	st.guessed = false
	st.adopt(c.Thread, c.Path, "codex", "")
	return true
}
