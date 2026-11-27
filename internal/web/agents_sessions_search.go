package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

// agentOldSession is one resumable conversation returned by
// GET /api/sessions/{id}/agent-sessions across all folders and agent kinds.
type agentOldSession struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Dir      string `json:"dir"`
	Modified int64  `json:"modified"`
	Title    string `json:"title,omitempty"`
	First    string `json:"first,omitempty"`
}

// GET /api/sessions/{id}/agent-sessions?q=…&kind=…&dir=…
// Searches past conversations across all folders (or one optional dir) and all
// 5 agent kinds (claude, codex, opencode, agy, devin) so the New Agent dialog can
// search and resume closed conversations without first knowing their folder.
func (s *Server) apiAgentSessions(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 120 {
		q = q[:120]
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && !agentKinds[kind] {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid agent kind")
		return
	}
	dir := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dir != "" {
		var dirOK bool
		dir, dirOK = agentDir(dir)
		if !dirOK {
			writeError(w, http.StatusBadRequest, "bad_folder", "a folder's full path (or one under ~/) is required")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), agentSessionsSearchCommand(q, kind, dir), 512<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	out := res.Out
	if err != nil {
		writeError(w, http.StatusBadGateway, "host_error", "the host's session list could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": parseAgentSessions(out)})
}

func agentSessionsSearchCommand(q, kind, dir string) string {
	return `command -v python3 >/dev/null 2>&1 || exit 0
exec python3 -I -c ` + sshx.QuoteShell(agentSessionsSearchScript) + ` ` + sshx.QuoteShell(q) + ` ` + sshx.QuoteShell(kind) + ` ` + sshx.QuoteShell(dir)
}

const agentSessionsSearchScript = `import datetime, glob, json, os, re, sqlite3, sys, urllib.parse

query = (sys.argv[1] or '').strip().lower()
tokens = [t for t in query.split() if t]
want_kind = (sys.argv[2] or '').strip()
want_dir = (sys.argv[3] or '').strip()
if want_dir == '~' or want_dir.startswith('~/'):
    want_dir = os.path.expanduser(want_dir)
if want_dir:
    want_dir = want_dir.rstrip('/') or '/'

def norm_dir(p):
    if not p or not isinstance(p, str):
        return ''
    p = p.strip()
    if p.startswith('file://'):
        p = urllib.parse.unquote(p[7:])
    return p.rstrip('/') or '/'

def dir_ok(d):
    if not want_dir:
        return True
    return norm_dir(d) == want_dir

def match_query(kind, d, title, first, sid):
    if not tokens:
        return True
    hay = ' '.join((kind, d or '', title or '', first or '', sid or '')).lower()
    return all(tok in hay for tok in tokens)

def clean(s, n=200):
    if not isinstance(s, str):
        return ''
    s = ' '.join(s.split())
    return s[:n]

def parse_iso_ms(s):
    if not s:
        return 0
    s = str(s).strip()
    if '.' in s:
        head, tail = s.split('.', 1)
        tz = ''
        for sep in ('+', '-', 'Z'):
            idx = tail.find(sep)
            if idx >= 0:
                tz = tail[idx:]
                tail = tail[:idx]
                break
        s = head + '.' + tail[:6] + tz
    s = s.replace('Z', '+00:00')
    try:
        return int(datetime.datetime.fromisoformat(s).timestamp() * 1000)
    except Exception:
        return 0

results = []
uuid_re = re.compile(r'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$')

# 1. Claude Code (~/.claude/projects/*/*.jsonl)
if want_kind in ('', 'claude'):
    base = os.path.expanduser('~/.claude/projects')
    if os.path.isdir(base):
        files = []
        try:
            for proj in os.scandir(base):
                if not proj.is_dir():
                    continue
                try:
                    for ent in os.scandir(proj.path):
                        if not ent.is_file() or not ent.name.endswith('.jsonl'):
                            continue
                        sid = ent.name[:-6]
                        if not uuid_re.match(sid):
                            continue
                        try:
                            st = ent.stat()
                            if st.st_size > 0:
                                files.append((st.st_mtime, ent.path, sid))
                        except OSError:
                            pass
                except OSError:
                    pass
        except OSError:
            pass
        files.sort(reverse=True)
        scan_limit = 300 if tokens else 120
        for mtime, path, sid in files[:scan_limit]:
            cwd = ''
            title = ''
            first = ''
            try:
                with open(path, 'rb') as f:
                    head = f.read(32768).decode('utf-8', 'ignore')
                for line in head.splitlines()[:80]:
                    if not line or line[0] != '{':
                        continue
                    try:
                        obj = json.loads(line)
                    except Exception:
                        continue
                    if not cwd and isinstance(obj.get('cwd'), str) and obj['cwd']:
                        cwd = norm_dir(obj['cwd'])
                    typ = obj.get('type')
                    if typ == 'custom-title' and isinstance(obj.get('customTitle'), str):
                        title = clean(obj['customTitle'], 120)
                    elif typ == 'summary' and isinstance(obj.get('summary'), str) and not title:
                        title = clean(obj['summary'], 120)
                    elif typ == 'user' and not first and not obj.get('isMeta'):
                        msg = obj.get('message') or {}
                        c = msg.get('content')
                        if isinstance(c, str):
                            t = clean(c, 200)
                            if t and not t.startswith('<'):
                                first = t
                        elif isinstance(c, list):
                            for part in c:
                                if isinstance(part, dict) and part.get('type') == 'text' and isinstance(part.get('text'), str):
                                    t = clean(part['text'], 200)
                                    if t and not t.startswith('<'):
                                        first = t
                                        break
                # Check tail for newer custom-title rename
                try:
                    sz = os.path.getsize(path)
                    if sz > 32768:
                        with open(path, 'rb') as f:
                            f.seek(max(0, sz - 16384))
                            tail = f.read(16384).decode('utf-8', 'ignore')
                        for line in tail.splitlines():
                            if '"custom-title"' in line:
                                try:
                                    obj = json.loads(line)
                                    if obj.get('type') == 'custom-title' and isinstance(obj.get('customTitle'), str):
                                        title = clean(obj['customTitle'], 120)
                                except Exception:
                                    pass
                except OSError:
                    pass
            except OSError:
                continue
            if not cwd or not dir_ok(cwd):
                continue
            if not (title or first):
                continue
            if not match_query('claude', cwd, title, first, sid):
                continue
            results.append({'id': sid, 'kind': 'claude', 'dir': cwd, 'ms': int(mtime * 1000), 'title': title, 'first': first})

# 2. Codex (~/.codex/state_5.sqlite)
if want_kind in ('', 'codex'):
    db = os.path.expanduser(os.environ.get('CODEX_HOME') or '~/.codex')
    db_path = os.path.join(db, 'state_5.sqlite')
    if os.path.isfile(db_path):
        try:
            con = sqlite3.connect('file:' + urllib.parse.quote(db_path) + '?mode=ro', uri=True, timeout=2)
            rows = con.execute(
                """SELECT id, cwd, title, first_user_message, updated_at
                   FROM threads
                   WHERE COALESCE(archived, 0) = 0
                   ORDER BY updated_at DESC LIMIT 400"""
            )
            for sid, cwd, title, first, upd in rows:
                d = norm_dir(cwd)
                if not sid or not d or not dir_ok(d):
                    continue
                t = clean(title, 120)
                f = clean(first, 200)
                if not match_query('codex', d, t, f, sid):
                    continue
                ms = int(upd * 1000) if isinstance(upd, (int, float)) and upd < 1e12 else int(upd or 0)
                results.append({'id': sid, 'kind': 'codex', 'dir': d, 'ms': ms, 'title': t, 'first': f})
            con.close()
        except sqlite3.Error:
            pass

# 3. Antigravity CLI (~/.gemini/antigravity-cli/conversation_summaries.db)
if want_kind in ('', 'agy'):
    db_path = os.path.expanduser('~/.gemini/antigravity-cli/conversation_summaries.db')
    if os.path.isfile(db_path):
        try:
            con = sqlite3.connect('file:' + urllib.parse.quote(db_path) + '?mode=ro', uri=True, timeout=2)
            rows = con.execute(
                """SELECT conversation_id, title, preview, last_modified_time, workspace_uris
                   FROM conversation_summaries
                   WHERE COALESCE(parent_conversation_id, '') = ''
                     AND COALESCE(nesting_depth, 0) = 0
                     AND COALESCE(killed, 0) = 0
                     AND (COALESCE(app_data_dir, '') = '' OR app_data_dir = 'antigravity-cli')
                   ORDER BY last_modified_time DESC LIMIT 400"""
            )
            for sid, title, preview, mod, uris_raw in rows:
                try:
                    uris = json.loads(uris_raw) if uris_raw else []
                except Exception:
                    uris = []
                dirs = [norm_dir(u) for u in uris if isinstance(u, str) and norm_dir(u)]
                if not dirs:
                    continue
                if want_dir:
                    if want_dir not in dirs:
                        continue
                    d = want_dir
                else:
                    d = dirs[0]
                t = clean(title, 120)
                f = clean(preview, 200)
                if not match_query('agy', d, t, f, sid):
                    continue
                results.append({'id': sid, 'kind': 'agy', 'dir': d, 'ms': parse_iso_ms(mod), 'title': t, 'first': f})
            con.close()
        except sqlite3.Error:
            pass

# 4. Devin CLI (~/.local/share/devin/cli/sessions.db): its sessions, the
# first message of each from its prompt history.
if want_kind in ('', 'devin'):
    db_path = os.path.join(os.environ.get('XDG_DATA_HOME') or os.path.expanduser('~/.local/share'), 'devin', 'cli', 'sessions.db')
    if os.path.isfile(db_path):
        try:
            con = sqlite3.connect('file:' + urllib.parse.quote(db_path) + '?mode=ro', uri=True, timeout=2)
            rows = con.execute(
                """SELECT id, working_directory, title, last_activity_at,
                          (SELECT content FROM prompt_history p WHERE p.session_id = s.id AND COALESCE(p.is_shell, 0) = 0 ORDER BY p.id LIMIT 1)
                   FROM sessions s
                   WHERE COALESCE(hidden, 0) = 0
                   ORDER BY last_activity_at DESC LIMIT 300"""
            )
            for sid, cwd, title, act, first in rows:
                d = norm_dir(cwd)
                if not sid or not d or not dir_ok(d):
                    continue
                t = clean(title, 120)
                f = clean(first, 200)
                if not (t or f):
                    continue
                if not match_query('devin', d, t, f, sid):
                    continue
                ms = int(act * 1000) if isinstance(act, (int, float)) and act < 1e12 else int(act or 0)
                results.append({'id': sid, 'kind': 'devin', 'dir': d, 'ms': ms, 'title': t, 'first': f})
            con.close()
        except sqlite3.Error:
            pass

# 5. OpenCode (~/.local/share/opencode/opencode.db)
if want_kind in ('', 'opencode'):
    db_path = os.path.expanduser('~/.local/share/opencode/opencode.db')
    if os.path.isfile(db_path):
        try:
            con = sqlite3.connect('file:' + urllib.parse.quote(db_path) + '?mode=ro', uri=True, timeout=2)
            seen_oc = set()
            for tbl in ('session_v2', 'session'):
                try:
                    rows = con.execute(
                        'SELECT id, directory, title, time_updated FROM ' + tbl +
                        ' WHERE parent_id IS NULL AND time_archived IS NULL ORDER BY time_updated DESC LIMIT 300'
                    )
                except sqlite3.OperationalError:
                    continue
                for sid, cwd, title, upd in rows:
                    if not sid or sid in seen_oc:
                        continue
                    seen_oc.add(sid)
                    d = norm_dir(cwd)
                    if not d or not dir_ok(d):
                        continue
                    t = clean(title, 120)
                    if not match_query('opencode', d, t, '', sid):
                        continue
                    ms = int(upd * 1000) if isinstance(upd, (int, float)) and upd < 1e12 else int(upd or 0)
                    results.append({'id': sid, 'kind': 'opencode', 'dir': d, 'ms': ms, 'title': t, 'first': ''})
            con.close()
        except sqlite3.Error:
            pass

results.sort(key=lambda r: r.get('ms', 0), reverse=True)
sys.stdout.write(json.dumps(results[:80], ensure_ascii=False))
`

func parseAgentSessions(out []byte) []agentOldSession {
	var rows []struct {
		ID, Kind, Dir, Title, First string
		MS                          int64
	}
	if json.Unmarshal(out, &rows) != nil {
		return []agentOldSession{}
	}
	res := make([]agentOldSession, 0, len(rows))
	for _, r := range rows {
		if !agentKinds[r.Kind] {
			continue
		}
		if r.Kind == "opencode" {
			if !ocSessionID.MatchString(r.ID) {
				continue
			}
		} else if r.Kind == "devin" {
			if !devinSessionID.MatchString(r.ID) {
				continue
			}
		} else if !sessionUUID.MatchString(r.ID) {
			continue
		}
		d, ok := agentDir(r.Dir)
		if !ok {
			continue
		}
		res = append(res, agentOldSession{
			ID:       r.ID,
			Kind:     r.Kind,
			Dir:      d,
			Modified: r.MS,
			Title:    clipText(strings.TrimSpace(r.Title), 120),
			First:    clipText(strings.TrimSpace(r.First), 200),
		})
	}
	return res
}
