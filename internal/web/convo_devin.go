package web

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Devin CLI keeps no log: its sessions are rows of a SQLite database
// (…/devin/cli/sessions.db), each message a node of a forest
// (message_nodes), the session's own line through it read from its tip
// (sessions.main_chain_id) back by each node's parent; a compacted one's
// summary names the node it summed up to (metadata.summarized_from), where
// what came before goes on. It is read by a query on the host (python3's
// sqlite3, read only), as OpenCode's is.
//
// Which session the Devin in a pane is at: its agent runs in a child of
// its own ("devin acp"), which holds that session's lock
// (…/devin/cli/session_locks/<id>.lock) open; else what herdr's integration
// for it says of the pane.

// devinSessionID: a Devin session's id, words joined by hyphens ("cuddly-dime").
var devinSessionID = regexp.MustCompile(`^[a-z][a-z0-9]{0,30}(?:-[a-z0-9]{1,30}){1,5}$`)

const (
	dvTail    = 200     // rows read when a session is first opened, and each time back
	dvReadMax = 8 << 20 // what one query may print
)

// devinFdsSh: the shell functions the locating script tells a Devin by.
// dv_inter: its command line is the interactive one's (no subcommand, no
// -p): 1. dv_fds: of a process (its /proc dir), the session whose lock it
// holds, dvsid (none where it holds several), and the database it has
// open, dvdb.
const devinFdsSh = `
dv_inter() {
  tr '\0' '\001' <"/proc/$1/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001"; ok = 1 }
    NR == 1 { next }
    skip == 1 { skip = 0; next }
    skip == 2 { skip = 0; if ($0 !~ /^-/) next }
    /^--$/ { exit }
    /^(-p|--print)$/ || /^--print=/ { ok = 0; exit }
    /^(--prompt-file|--config|--permission-mode|--process-model|--model|--verbosity)$/ { skip = 1; next }
    /^(-r|--resume|--export|--respect-workspace-trust)$/ { skip = 2; next }
    /^-/ { next }
    { ok = 0; exit }
    END { if (ok) print 1 }'
}
dv_fds() {
  dvsid= dvdb= dvn=0
  for fd in "$1"/fd/*; do
    l=$(readlink "$fd" 2>/dev/null) || continue
    case $l in
    *"$nl"*) continue;;
    */devin/cli/session_locks/*.lock)
      id=${l##*/}; id=${id%.lock}
      case $id in ''|*[!a-z0-9-]*) continue;; esac
      [ "$id" = "$dvsid" ] || { dvsid=$id; dvn=$((dvn + 1)); } ;;
    */devin/cli/sessions.db) dvdb=$l;;
    esac
  done
  [ "$dvn" -le 1 ] || dvsid=
}
`

// devinLocateSh: the locating script's part for Devin: each interactive
// Devin of the user's ($devins: their /proc dirs), its session the one its
// child holds the lock of (none yet: the zero id, its database the
// default one, if there, for herdr's word on its session).
const devinLocateSh = `
for d in $devins; do
  pid=${d#/proc/}
  [ -n "$(dv_inter "$pid")" ] || continue
  sid= log=
  for k in $devins; do
    [ "$k" = "$d" ] && continue
    ppid_of "${k#/proc/}"
    [ "$pp" = "$pid" ] || continue
    dv_fds "$k"
    [ -n "$dvsid" ] || continue
    sid=$dvsid log=$dvdb
  done
  if [ -z "$sid" ]; then
    sid=00000000-0000-0000-0000-000000000000 log=
    f="${XDG_DATA_HOME:-$HOME/.local/share}/devin/cli/sessions.db"
    case $f in *"$nl"*) ;; *) [ -f "$f" ] && log=$f;; esac
  fi
  stat_of "$pid" || continue
  rec "$pid" "$st" "$fg" "$st" "$sid" "$log" devin
done
`

// dvQueryScript prints, one JSON line each, what is read of a session (see
// convo.Devin): its line ("s", with its tip), then rows of it, and where
// the next look goes on from ({"k":"cur","tip":…}). With tail and older,
// {"k":"floor","n":…}, the first node printed, and {"k":"older","n":…}, how
// many rows are before it; since, where the tip it goes on from is no
// longer on the session's line (rewound, compacted, its line made anew),
// {"k":"reset"}, then its tail. All in one read of the database. Long
// fields are cut, and no more than BUDGET is printed: what is read goes
// over the wire.
//
//	tail  <sid> <n>          the last n rows
//	since <sid> <tip> <n>    the rows after node tip, else (reset) the last n
//	older <sid> <floor> <n>  the n rows before node floor
const dvQueryScript = `import sqlite3, json, re, sys
from urllib.parse import quote
db, mode, sid = sys.argv[1], sys.argv[2], sys.argv[3]
BUDGET = 6 << 20
BIG = 1 << 20
out = sys.stdout
def line(o):
    return json.dumps(o, ensure_ascii=False, separators=(',', ':')) + '\n'
def num(x):
    return x if isinstance(x, int) and not isinstance(x, bool) else None
def cut(s, n):
    if not isinstance(s, str):
        return ''
    return s[:n]
def ends(s, n):
    if not isinstance(s, str):
        return ''
    return s if len(s) <= n else s[:n // 2] + '\n' + s[-(n // 2):]
try:
    c = sqlite3.connect('file:' + quote(db) + '?mode=ro', uri=True, timeout=5, isolation_level=None)
    c.execute('begin')
    r = c.execute('select title, model, agent_mode, main_chain_id from sessions where id = ?', (sid,)).fetchone()
except sqlite3.Error:
    r = None
if not r:
    out.write(line({'k': 'none'})); sys.exit()
tip = num(r[3])
s = {'k': 's', 'tip': tip or 0}
for k, v in (('title', r[0]), ('model', r[1]), ('mode', r[2])):
    if isinstance(v, str) and v:
        s[k] = v[:300]
out.write(line(s))
if tip is None:
    out.write(line({'k': 'cur', 'tip': 0})); sys.exit()
if mode == 'since' and num(int(sys.argv[4])) == tip:
    out.write(line({'k': 'cur', 'tip': tip})); sys.exit()
ROLE = re.compile(r'"role"\s*:\s*"([a-z_]+)"')
nodes = {}
for n, p, md, head in c.execute('select node_id, parent_node_id, metadata, substr(chat_message, 1, 200) from message_nodes where session_id = ?', (sid,)):
    sf = None
    if isinstance(md, str) and 'summarized_from' in md:
        try:
            sf = num(json.loads(md).get('summarized_from'))
        except (ValueError, AttributeError):
            pass
    m = ROLE.search(head or '')
    nodes[n] = (num(p), sf, m.group(1) if m else '')
# The session's line, from its start: from its tip back by each node's
# parent, and from a summary to what it summed up.
chain, seen, n = [], set(), tip
while n is not None and n in nodes and n not in seen and len(chain) < 500000:
    seen.add(n); chain.append(n)
    p, sf, _ = nodes[n]
    n = sf if sf is not None and sf in nodes and sf not in seen else p
chain.reverse()
def shown(n):
    p, sf, role = nodes[n]
    return role in ('user', 'assistant', 'tool') or (role == 'system' and sf is not None)
rows = [n for n in chain if shown(n)]
def back(i):
    # (Not from a tool's result: from the answer whose call it is.)
    while i > 0 and nodes[rows[i]][2] == 'tool':
        i -= 1
    return i
def slim_args(a):
    if not isinstance(a, dict):
        return {}
    o, room = {}, 65536
    for k, v in list(a.items())[:32]:
        if isinstance(v, str):
            v = v[:16384]
        elif not isinstance(v, (int, float, bool)) and v is not None and len(json.dumps(v)) > 16384:
            continue
        size = len(json.dumps({k: v}, ensure_ascii=False).encode())
        if size > room:
            continue
        o[k] = v; room -= size
    return o
def row(n, m):
    role = m.get('role')
    md = m.get('metadata') if isinstance(m.get('metadata'), dict) else {}
    ext = md.get('extensions') if isinstance(md.get('extensions'), dict) else {}
    base = {'n': n, 'id': cut(m.get('message_id'), 128), 't': cut(md.get('created_at'), 64)}
    if role == 'user':
        if md.get('is_user_input') is not True:
            return None
        return dict(base, k='u', c=cut(m.get('content'), 65536))
    if role == 'system':
        return dict(base, k='sum', c=cut(m.get('content'), 65536))
    if role == 'assistant':
        o = dict(base, k='a', c=cut(m.get('content'), 65536))
        th = m.get('thinking')
        if isinstance(th, dict) and isinstance(th.get('thinking'), str) and th['thinking']:
            o['th'] = th['thinking'][:65536]
        tc = []
        for x in (m.get('tool_calls') or [])[:64]:
            if isinstance(x, dict):
                tc.append({'id': cut(x.get('id'), 128), 'name': cut(x.get('name'), 128), 'a': slim_args(x.get('arguments'))})
        if tc:
            o['tc'] = tc
        if isinstance(md.get('finish_reason'), str):
            o['fin'] = md['finish_reason'][:64]
        if isinstance(md.get('generation_model'), str):
            o['model'] = md['generation_model'][:120]
        mt = md.get('metrics') if isinstance(md.get('metrics'), dict) else {}
        for k, f in (('ctx', 'context_input_tokens'), ('out', 'output_tokens'), ('ms', 'total_time_ms')):
            if num(mt.get(f)) is not None:
                o[k] = mt[f]
        return o
    if role == 'tool':
        text = m.get('content') if isinstance(m.get('content'), str) else ''
        o = dict(base, k='r', call=cut(m.get('tool_call_id'), 128))
        term = ext.get('chisel/terminal_output')
        if isinstance(term, dict):
            if isinstance(term.get('text'), str):
                text = term['text']
            ex = term.get('exit') if isinstance(term.get('exit'), dict) else {}
            if num(ex.get('exit_code')) is not None:
                o['exit'] = ex['exit_code']
        o['c'] = ends(text, 65536)
        o['lines'] = text.count('\n') + 1 if text else 0
        meta = ext.get('chisel/tool_result_meta')
        if isinstance(meta, dict) and isinstance(meta.get('success'), bool):
            o['ok'] = meta['success']
        fail = ext.get('chisel/tool_failure')
        if isinstance(fail, dict) and isinstance(fail.get('reason'), str):
            o['fail'] = fail['reason'][:2048]
        ops = []
        for f in (ext.get('chisel/file_operations') or [])[:20]:
            if isinstance(f, dict):
                ops.append({'op': cut(f.get('operation'), 32), 'path': cut(f.get('file_path'), 4096), 'add': num(f.get('lines_added')) or 0, 'del': num(f.get('lines_removed')) or 0})
        if ops:
            o['ops'] = ops
        return o
    return None
def fetch(ids):
    for i in range(0, len(ids), 400):
        part = ids[i:i + 400]
        q = 'select node_id, length(chat_message) from message_nodes where session_id = ? and node_id in (' + ','.join('?' * len(part)) + ')'
        sizes = dict(c.execute(q, [sid] + part))
        for n in part:
            size = sizes.get(n)
            if size is None:
                continue
            if size > BIG:
                yield n, size, None
                continue
            r = c.execute('select chat_message from message_nodes where session_id = ? and node_id = ?', (sid, n)).fetchone()
            if not r:
                continue
            try:
                m = json.loads(r[0])
            except ValueError:
                continue
            if isinstance(m, dict):
                yield n, size, m
def emit(ids, first, cut_front):
    # (The rows of ids, at most BUDGET of them: from the end where the
    # front is cut, else from the start. The first printed, and how many
    # rows are before it.)
    ls = []
    for n, size, m in fetch(ids):
        if m is None:
            o = {'k': 'big', 'n': n, 'bytes': size, 'role': nodes.get(n, (None, None, ''))[2]}
        else:
            o = row(n, m)
        if o is not None:
            ls.append((n, line(o)))
    size, keep = 0, []
    for n, l in (reversed(ls) if cut_front else ls):
        if size + len(l.encode()) > BUDGET:
            break
        keep.append((n, l)); size += len(l.encode())
    if cut_front:
        keep.reverse()
        dropped = len(ls) - len(keep)
        if keep:
            out.write(line({'k': 'floor', 'n': keep[0][0]}))
        out.write(line({'k': 'older', 'n': first + dropped}))
    for _, l in keep:
        out.write(l)
    return len(keep) == len(ls), keep[-1][0] if keep else None
def tail(n):
    i = back(max(0, len(rows) - n))
    emit(rows[i:], i, True)
cur = tip
if mode == 'tail':
    tail(int(sys.argv[4]))
elif mode == 'since':
    old = int(sys.argv[4])
    if old in seen:
        at = chain.index(old)
        new = [n for n in chain[at + 1:] if shown(n)]
        if len(new) > int(sys.argv[5]) * 4:
            out.write(line({'k': 'reset'})); tail(int(sys.argv[5]))
        else:
            whole, last = emit(new, 0, False)
            if not whole:
                cur = last if last is not None else old
    else:
        out.write(line({'k': 'reset'})); tail(int(sys.argv[5]))
else:
    floor, n = int(sys.argv[4]), int(sys.argv[5])
    if floor not in rows:
        out.write(line({'k': 'older', 'n': 0}))
    else:
        j = rows.index(floor)
        i = back(max(0, j - n))
        emit(rows[i:j], i, True)
out.write(line({'k': 'cur', 'tip': cur}))
`

// dvQueryCommand: fixed, the database's path (one the locating found open
// by a Devin of the user's, or its default one), the mode and its
// validated arguments.
func dvQueryCommand(db, mode string, args ...string) string {
	sh := `command -v python3 >/dev/null 2>&1 || { printf '{"k":"nopy"}\n'; exit 0; }
exec python3 -I -c ` + sshx.QuoteShell(dvQueryScript) + ` "$@"`
	cmd := "sh -c " + sshx.QuoteShell(sh) + " corral " + sshx.QuoteShell(db) + " " + sshx.QuoteShell(mode)
	for _, a := range args {
		cmd += " " + sshx.QuoteShell(a)
	}
	return cmd
}

// devinSessionOf settles which session the Devin p runs in pane is at: the
// one its agent holds the lock of, else what herdr says of the pane (its
// integration for Devin), else none yet (no log: a new one, or one whose
// agent runs elsewhere, Devin's background daemon).
func (s *Server) devinSessionOf(sess *term.Session, p *convoProc, pane string) {
	if p.sid == convoNoSession && sess.Persistence == "herdr" && p.log != "" {
		if v := s.herdrSocks.sid(sess.ID, pane); devinSessionID.MatchString(v) {
			p.sid = v
		}
	}
	if !devinSessionID.MatchString(p.sid) {
		p.sid, p.log = convoNoSession, ""
	}
}

// readDevin reads what is new of the session st is of: at first its last
// dvTail rows, then those after the tip last read.
func (s *Server) readDevin(sess *term.Session, st *convoState) error {
	if !devinSessionID.MatchString(st.sid) {
		return nil
	}
	if st.size < 0 {
		return s.dvRead(sess, st, "tail", strconv.Itoa(dvTail))
	}
	return s.dvRead(sess, st, "since", strconv.FormatInt(st.dvTip, 10), strconv.Itoa(dvTail))
}

// readOlderDevin reads in the rows before the first read.
func (s *Server) readOlderDevin(sess *term.Session, st *convoState) error {
	dv, ok := st.parser.(*convo.Devin)
	if !st.older || !ok || dv.Trimmed() || !devinSessionID.MatchString(st.sid) || dv.Floor() <= 0 {
		return nil
	}
	return s.dvRead(sess, st, "older", strconv.FormatInt(dv.Floor(), 10), strconv.Itoa(dvTail))
}

func (s *Server) dvRead(sess *term.Session, st *convoState, mode string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), convoReadLimit)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), dvQueryCommand(st.log, mode, append([]string{st.sid}, args...)...), dvReadMax, sshx.ExecOptions{Interactive: mode == "older"})
	out := res.Out
	if err != nil {
		return err
	}
	rows, tip, reset, ok := dvRows(out)
	if !ok {
		st.reason, st.noReaderAt = convoNoReader, time.Now()
		return nil
	}
	if mode == "older" {
		o := convo.NewDevin()
		o.SetFloor(rows.floor)
		o.Feed(rows.lines)
		st.parser.Prepend(o)
		st.older = rows.older > 0
		return nil
	}
	if reset {
		// (Its line not the one read: read anew, from its end.)
		st.gen = convoGens.Add(1)
		st.parser = st.newParser()
	}
	dv, _ := st.parser.(*convo.Devin)
	if dv != nil && rows.floor > 0 {
		dv.SetFloor(rows.floor)
	}
	if rows.hasOlder {
		st.older = rows.older > 0
	}
	if len(rows.lines) > 0 {
		st.parser.Feed(rows.lines)
	}
	if mode == "since" && tip != st.dvTip {
		st.grewAt = time.Now()
	}
	if st.size < 0 {
		st.size = 0
		st.grewAt = time.Now()
	}
	st.dvTip, st.mtime = tip, time.Now()
	if dv != nil {
		st.status = dv.Status()
	}
	return nil
}

type dvRead struct {
	lines    []byte
	floor    int64
	older    int64
	hasOlder bool
}

// dvRows reads dvQueryScript's output: the rows to feed, the tip, whether
// the line read before is gone; ok false where the host has no python3.
func dvRows(out []byte) (r dvRead, tip int64, reset, ok bool) {
	var b bytes.Buffer
	var head []byte // (the session's own line: kept over a reset)
	for _, l := range bytes.Split(out, []byte("\n")) {
		var x struct {
			K   string `json:"k"`
			N   int64  `json:"n"`
			Tip int64  `json:"tip"`
		}
		if len(l) == 0 || json.Unmarshal(l, &x) != nil {
			continue
		}
		switch x.K {
		case "nopy":
			return r, 0, false, false
		case "cur":
			if x.Tip >= 0 {
				tip = x.Tip
			}
		case "reset":
			reset = true
			b.Reset()
		case "floor":
			r.floor = x.N
		case "older":
			r.older, r.hasOlder = x.N, true
		case "s":
			head = append(append([]byte(nil), l...), '\n')
		case "u", "a", "r", "sum", "big":
			b.Write(l)
			b.WriteByte('\n')
		}
	}
	r.lines = append(head, b.Bytes()...)
	return r, tip, reset, true
}
