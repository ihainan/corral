package web

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// OpenCode keeps no log: its sessions are rows of a SQLite database
// (…/opencode/opencode.db), written over as they go on. Its conversation is
// read by a query on the host (python3's sqlite3: the sqlite3 command is
// often not there), rows written since the last look, fed to
// convo.OpenCode; reading back is the messages before the oldest read.
//
// Which session the OpenCode in a pane is at: herdr's integration for it
// says (the pane's agent session, "ses_…"); in tmux, or without it, the
// latest one begun where it runs and written since it started (two in one
// place are not told apart).

// ocSessionID: an OpenCode session's id.
var ocSessionID = regexp.MustCompile(`^ses_[A-Za-z0-9]{8,64}$`)

const (
	convoNoReader = "no_reader" // (OpenCode) the host has no python3 to read its database with
	ocTail        = 100         // messages read when a session is first opened, and each time back
	ocReadMax     = 8 << 20     // what one query may print
)

// ocQueryScript prints, one JSON line each, what is read of a session (see
// convo.OpenCodeRow), then {"k":"cur","u":…}: where the next look goes on
// from (the latest time a row printed was written: rows are printed in the
// order they were written, so one left out for want of room is read next
// time). With tail and older, {"k":"floor","c":…,"id":…}, the first message
// printed (what is before it is read next, by older), and {"k":"older","n":…},
// how many messages are before it. All in one read of the database (a
// snapshot: a row written meanwhile is in the next look). Long fields are
// cut and a file's data left out, and no more than ocBudget is printed:
// what is read goes over the wire.
//
//	since <sid> <cursor> <k> <id>      rows written at or after cursor (unix ms); with an id,
//	                                   in its millisecond only those after (k, id)
//	tail  <sid> <n>                    the last n messages, with their parts
//	older <sid> <c> <id> <n>           the n messages before (c, id), with theirs
//	find  <cwd> <since>                the latest top-level session begun in cwd and written since
const ocQueryScript = `import sqlite3, json, sys, time
from urllib.parse import quote
db, mode = sys.argv[1], sys.argv[2]
BUDGET = 6 << 20
c = sqlite3.connect('file:' + quote(db) + '?mode=ro', uri=True, timeout=5, isolation_level=None)
c.execute('begin')
out = sys.stdout
def cut(s, n):
    return s[:n] if isinstance(s, str) and len(s) > n else s
def slim_msg(d):
    try:
        m = json.loads(d)
    except Exception:
        return d
    if isinstance(m.get('summary'), dict):
        m['summary'].pop('diffs', None)
    return json.dumps(m, ensure_ascii=False, separators=(',', ':'))
def slim(d):
    try:
        p = json.loads(d)
    except Exception:
        return None
    t = p.get('type')
    if t in ('text', 'reasoning'):
        p['text'] = cut(p.get('text', ''), 65536)
    elif t == 'file':
        p.pop('url', None); p.pop('source', None)
    elif t == 'tool':
        st = p.get('state') or {}
        st['output'] = cut(st.get('output'), 8192)
        st['error'] = cut(st.get('error'), 8192)
        inp = st.get('input')
        if isinstance(inp, dict):
            room = 65536
            for k, v in list(inp.items()):
                if isinstance(v, str):
                    v = cut(v, 16384)
                elif not isinstance(v, (int, float, bool)) and len(json.dumps(v)) > 16384:
                    v = None
                n = len(json.dumps({k: v}, ensure_ascii=False).encode())
                if n > room:
                    del inp[k]  # (no room left in all of it)
                    continue
                inp[k] = v; room -= n
        md = st.get('metadata')
        if isinstance(md, dict):
            keep = {}
            if isinstance(md.get('exit'), int): keep['exit'] = md['exit']
            if isinstance(md.get('diff'), str): keep['diff'] = cut(md['diff'], 32768)
            fd = md.get('filediff')
            if isinstance(fd, dict) and isinstance(fd.get('patch'), str): keep['filediff'] = {'patch': cut(fd['patch'], 32768)}
            st['metadata'] = keep
        st.pop('attachments', None)
        p['state'] = st
    elif t not in ('step-start', 'step-finish'):
        return None
    return json.dumps(p, ensure_ascii=False, separators=(',', ':'))
def line(o):
    return json.dumps(o, ensure_ascii=False, separators=(',', ':')) + '\n'
if mode == 'find':
    r = c.execute('select id from session where directory = ? and parent_id is null and time_updated >= ? order by time_updated desc limit 1', (sys.argv[3], int(sys.argv[4]))).fetchone()
    out.write(line({'k': 'sid', 'id': r[0] if r else '', 'now': int(time.time() * 1000)}))
    sys.exit()
sid = sys.argv[3]
r = c.execute('select title from session where id = ?', (sid,)).fetchone()
if not r:
    out.write(line({'k': 'none'})); sys.exit()
out.write(line({'k': 's', 'title': r[0]}))
def mrow(m):
    return line({'k': 'm', 'id': m[0], 'c': m[1], 'u': m[2], 'd': slim_msg(m[3])})
def prow(p):
    d = slim(p[4])
    return line({'k': 'p', 'id': p[0], 'm': p[1], 'c': p[2], 'u': p[3], 'd': d}) if d is not None else ''
if mode == 'since':
    # (From cursor: all rows written in its millisecond and after; or, where
    # the last look stopped for want of room in one millisecond (ck, cid),
    # those after the last it printed.)
    cur, ck, cid = int(sys.argv[4]), int(sys.argv[5]), sys.argv[6]
    rows = [(m[2], 0, m[0], mrow(m)) for m in c.execute('select id, time_created, time_updated, data from message where session_id = ? and time_updated >= ?', (sid, cur))]
    rows += [(p[3], 1, p[0], prow(p)) for p in c.execute('select id, message_id, time_created, time_updated, data from part where session_id = ? and time_updated >= ?', (sid, cur))]
    rows.sort()
    if cid:
        rows = [r for r in rows if (r[0], r[1], r[2]) > (cur, ck, cid)]
    size, last, cut = 0, None, False
    for r in rows:
        n = len(r[3].encode())
        if n > BUDGET:
            last = r; continue  # (a row too long to send at all: passed over)
        if size + n > BUDGET:
            cut = True
            break
        out.write(r[3]); size += n; last = r
    top = max([cur] + [r[0] for r in rows])
    if cut or (cid and top == cur):
        # (Within a millisecond still: on from the last row read in it, not
        # all of it again.)
        last = last or (cur, ck, cid)
        out.write(line({'k': 'cur', 'u': last[0], 'ck': last[1], 'cid': last[2]}))
    else:
        out.write(line({'k': 'cur', 'u': top}))
    sys.exit()
if mode == 'tail':
    n = int(sys.argv[4])
    q = c.execute('select id, time_created, time_updated, data from message where session_id = ? order by time_created desc, id desc limit ?', (sid, n))
else:
    n = int(sys.argv[6])
    fc, fid = int(sys.argv[4]), sys.argv[5]
    q = c.execute('select id, time_created, time_updated, data from message where session_id = ? and (time_created < ? or (time_created = ? and id < ?)) order by time_created desc, id desc limit ?', (sid, fc, fc, fid, n))
took, size, cur = [], 0, 0
for m in q.fetchall():
    ls, k = [mrow(m)], 0
    for p in c.execute('select id, message_id, time_created, time_updated, data from part where message_id = ? order by id', (m[0],)):
        l = prow(p)
        if not took and k + len(l.encode()) > BUDGET:
            break  # (one message alone more than there is room for: what fits of it)
        ls.append(l); k += len(l.encode())
    k += len(ls[0].encode())
    if took and size + k > BUDGET:
        break
    took.append((m, ls)); size += k
took.reverse()
if took:
    f = took[0][0]
    out.write(line({'k': 'floor', 'c': f[1], 'id': f[0]}))
    left = c.execute('select count(*) from message where session_id = ? and (time_created < ? or (time_created = ? and id < ?))', (sid, f[1], f[1], f[0])).fetchone()[0]
else:
    left = 0
out.write(line({'k': 'older', 'n': left}))
for m, ls in took:
    for l in ls:
        out.write(l)
    cur = max(cur, m[2], *[int(json.loads(l)['u']) for l in ls if l])
out.write(line({'k': 'cur', 'u': cur}))
`

// ocQueryCommand: fixed, the database's path (one locating found open by
// an OpenCode of the user's), the mode and its validated arguments.
func ocQueryCommand(db, mode string, args ...string) string {
	sh := `command -v python3 >/dev/null 2>&1 || { printf '{"k":"nopy"}\n'; exit 0; }
exec python3 -c ` + sshx.QuoteShell(ocQueryScript) + ` "$@"`
	cmd := "sh -c " + sshx.QuoteShell(sh) + " corral " + sshx.QuoteShell(db) + " " + sshx.QuoteShell(mode)
	for _, a := range args {
		cmd += " " + sshx.QuoteShell(a)
	}
	return cmd
}

// ocSessionOf is the session the OpenCode p runs in pane is at: what
// herdr says of the pane, else the latest begun where it runs since it
// started; convoNoSession while it has none (a new one, nothing sent yet).
// why, where it cannot be told: convoNoReader (no python3 on the host), or
// convoNotTold (another OpenCode of the user's runs in the same place, and
// herdr does not say: which of the sessions there is whose cannot be told).
// l is what locating found of the host's processes.
func (s *Server) ocSessionOf(ctx context.Context, sess *term.Session, p *convoProc, pane string, l convoLocated) (sid, why string) {
	if sess.Persistence == "herdr" {
		if v := s.herdrSocks.sid(sess.ID, pane); ocSessionID.MatchString(v) {
			return v, ""
		}
	}
	if p.cwd == "" || strings.ContainsAny(p.cwd, "\n\x00") {
		return convoNoSession, ""
	}
	key := strconv.FormatInt(sess.HostID, 10) + "\x00" + p.cwd
	for _, q := range l.procs {
		if q.agent == "opencode" && q.pid != p.pid && q.cwd == p.cwd {
			s.ocContested.note(key, true)
			return convoNoSession, convoNotTold
		}
	}
	since := p.started - 2000 // (the host's clock, read twice)
	if p.started <= 0 {
		since = 0
	}
	// (Another was there till lately: what was written before it went may
	// be its own. Only what is written since; just gone, nothing yet: when
	// is read off the host.)
	ended, justEnded, seen := s.ocContested.note(key, false)
	since = max(since, ended)
	out, err := sshx.Output(ctx, sess.Client(), ocQueryCommand(p.log, "find", p.cwd, strconv.FormatInt(since, 10)), 4096)
	if err != nil {
		return convoNoSession, ""
	}
	var r struct {
		K   string `json:"k"`
		ID  string `json:"id"`
		Now int64  `json:"now"`
	}
	switch {
	case json.Unmarshal(bytes.TrimSpace(out), &r) != nil:
		return convoNoSession, ""
	case r.K == "nopy":
		return convoNoSession, convoNoReader
	case justEnded:
		if r.Now > 0 {
			s.ocContested.ended(key, r.Now, seen)
		}
		return convoNoSession, ""
	case r.K != "sid" || !ocSessionID.MatchString(r.ID):
		return convoNoSession, ""
	}
	return r.ID, ""
}

// ocContests: the folders where two OpenCodes of a user ran at once (by
// host and folder): while they do, and when that was seen to end, by the
// host's own clock (what the database's times are by). Kept for the
// gateway's life: a session left by one that went is never taken for the
// other's, however long after.
type ocContests struct {
	mu sync.Mutex
	m  map[string]ocContest
}

type ocContest struct {
	open  bool
	ended int64 // (unix ms, the host's clock; 0 until it is read)
	seen  int64 // (each time it is seen contested: an end read before that is not its end)
}

// note says the folder is contested now, or not; it returns when the last
// contest there ended (0: none), and open, it has just ended and when is to
// be read off the host (see ended), as of seen.
func (c *ocContests) note(key string, now bool) (ended int64, open bool, seen int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ocContest{}
	}
	e, ok := c.m[key]
	if now {
		c.m[key] = ocContest{open: true, seen: e.seen + 1}
		return 0, false, 0
	}
	if !ok {
		return 0, false, 0
	}
	return e.ended, e.open, e.seen
}

// ended says when the contest there, as of seen, was seen to end, by the
// host's clock. (Contested again since: not this one's end.)
func (c *ocContests) ended(key string, at, seen int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && e.open && e.seen == seen {
		c.m[key] = ocContest{ended: at, seen: e.seen}
	}
}

// readOpenCode reads what is new of the session st is of: at first its last
// ocTail messages, then the rows written since the last look.
func (s *Server) readOpenCode(sess *term.Session, st *convoState) error {
	if !ocSessionID.MatchString(st.sid) {
		return nil
	}
	if st.size < 0 {
		return s.ocRead(sess, st, "tail", strconv.Itoa(ocTail))
	}
	return s.ocRead(sess, st, "since", strconv.FormatInt(st.ocCursor, 10), strconv.Itoa(st.ocCursorK), st.ocCursorID)
}

// readOlderOpenCode reads in the messages before the first read.
func (s *Server) readOlderOpenCode(sess *term.Session, st *convoState) error {
	oc, ok := st.parser.(*convo.OpenCode)
	if !st.older || !ok || oc.Trimmed() || !ocSessionID.MatchString(st.sid) {
		return nil
	}
	fc, fid, ok := oc.Floor()
	if !ok || !ocMessageID.MatchString(fid) {
		return nil
	}
	return s.ocRead(sess, st, "older", strconv.FormatInt(fc, 10), fid, strconv.Itoa(ocTail))
}

// ocMessageID: an OpenCode message's id; ocRowID: a message's or a part's.
var (
	ocMessageID = regexp.MustCompile(`^msg_[A-Za-z0-9]{8,64}$`)
	ocRowID     = regexp.MustCompile(`^(msg|prt)_[A-Za-z0-9]{8,64}$`)
)

func (s *Server) ocRead(sess *term.Session, st *convoState, mode string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), convoReadLimit)
	defer cancel()
	out, err := sshx.Output(ctx, sess.Client(), ocQueryCommand(st.log, mode, append([]string{st.sid}, args...)...), ocReadMax)
	if err != nil {
		return err
	}
	var rows bytes.Buffer
	cursor, cursorK, cursorID := st.ocCursor, st.ocCursorK, st.ocCursorID
	var floorC int64
	floorID := ""
	for _, line := range bytes.Split(out, []byte("\n")) {
		var r struct {
			K   string `json:"k"`
			U   int64  `json:"u"`
			N   int64  `json:"n"`
			C   int64  `json:"c"`
			ID  string `json:"id"`
			CK  int    `json:"ck"`
			CID string `json:"cid"`
		}
		if len(line) == 0 || json.Unmarshal(line, &r) != nil {
			continue
		}
		switch r.K {
		case "nopy":
			st.reason, st.noReaderAt = convoNoReader, time.Now()
			return nil
		case "cur":
			// (Stopped for want of room within a millisecond: on from the
			// last row printed in it.)
			if mode != "older" && (r.U > cursor || (r.U == cursor && r.CID != "")) && (r.CID == "" || ocRowID.MatchString(r.CID)) {
				cursor, cursorK, cursorID = r.U, r.CK, r.CID
			}
		case "floor":
			floorC, floorID = r.C, r.ID
		case "older":
			st.older = r.N > 0
		case "s", "m", "p":
			rows.Write(line)
			rows.WriteByte('\n')
		}
	}
	// (The first message read, before what is fed: one before it, written
	// over meanwhile, is read back in its turn, not taken for new.)
	if mode == "older" {
		o := convo.NewOpenCode()
		if floorID != "" {
			o.SetFloor(floorC, floorID)
		}
		o.Feed(rows.Bytes())
		st.parser.Prepend(o)
	} else {
		if oc, ok := st.parser.(*convo.OpenCode); ok && floorID != "" {
			oc.SetFloor(floorC, floorID)
		}
		if rows.Len() > 0 {
			st.parser.Feed(rows.Bytes())
		}
	}
	if st.size < 0 {
		st.size = 0 // (the first read made)
	}
	st.ocCursor, st.ocCursorK, st.ocCursorID, st.mtime = cursor, cursorK, cursorID, time.Now()
	if oc, ok := st.parser.(*convo.OpenCode); ok {
		st.status = oc.Status()
	}
	return nil
}
