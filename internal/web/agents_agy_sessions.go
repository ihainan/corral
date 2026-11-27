package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

// GET /api/sessions/{id}/agy-sessions?dir=… → {"sessions": [{id, modified,
// title, first}]}: Antigravity CLI conversations of a folder that may be
// resumed (agy --conversation <id>) from the new-agent dialog, newest first,
// read from ~/.gemini/antigravity-cli/conversation_summaries.db (read-only).
func (s *Server) apiAgySessions(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	dir, ok := agentDir(r.URL.Query().Get("dir"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_folder", "a folder's full path (or one under ~/) is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := sshx.Exec(ctx, sess.Client(), agySessionsCommand(dir), 256<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	out := res.Out
	if err != nil {
		writeError(w, http.StatusBadGateway, "host_error", "the host's list could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": parseAgySessions(out)})
}

func agySessionsCommand(dir string) string {
	return `command -v python3 >/dev/null 2>&1 || exit 0
exec python3 -I -c ` + sshx.QuoteShell(agySessionsScript) + ` ` + sshx.QuoteShell(dir)
}

const agySessionsScript = `import datetime, json, os, sqlite3, sys, urllib.parse
d = sys.argv[1]
if d == '~' or d.startswith('~/'):
    d = os.path.expanduser(d)
d = d.rstrip('/') or '/'
db = os.path.expanduser('~/.gemini/antigravity-cli/conversation_summaries.db')
if not os.path.isfile(db):
    sys.stdout.write('[]')
    sys.exit(0)
def parse_ms(s):
    if not s:
        return 0
    s = str(s).strip()
    # Trim nanoseconds to microseconds for Python fromisoformat compatibility
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
out = []
try:
    con = sqlite3.connect('file:' + urllib.parse.quote(db) + '?mode=ro', uri=True, timeout=2)
    q = """SELECT conversation_id, title, preview, last_modified_time, workspace_uris
           FROM conversation_summaries
           WHERE COALESCE(parent_conversation_id, '') = ''
             AND COALESCE(nesting_depth, 0) = 0
             AND COALESCE(killed, 0) = 0
             AND (COALESCE(app_data_dir, '') = '' OR app_data_dir = 'antigravity-cli')
           ORDER BY last_modified_time DESC LIMIT 200"""
    for cid, title, preview, mod, uris_raw in con.execute(q):
        try:
            uris = json.loads(uris_raw) if uris_raw else []
        except Exception:
            uris = []
        matched = False
        for u in uris:
            if not isinstance(u, str):
                continue
            p = urllib.parse.unquote(u[7:]) if u.startswith('file://') else u
            if (p.rstrip('/') or '/') == d:
                matched = True
                break
        if not matched:
            continue
        out.append({'id': cid, 'title': (title or '')[:200], 'first': (preview or '')[:200], 'ms': parse_ms(mod)})
        if len(out) >= 50:
            break
except sqlite3.Error:
    pass
sys.stdout.write(json.dumps(out))
`

func parseAgySessions(out []byte) []codexOld {
	var rows []struct {
		ID, Title, First string
		MS               int64
	}
	if json.Unmarshal(out, &rows) != nil {
		return []codexOld{}
	}
	res := make([]codexOld, 0, len(rows))
	for _, r := range rows {
		if !sessionUUID.MatchString(r.ID) {
			continue
		}
		res = append(res, codexOld{
			ID:       r.ID,
			Modified: r.MS,
			Title:    clipText(strings.TrimSpace(r.Title), 120),
			First:    clipText(strings.TrimSpace(r.First), 200),
		})
	}
	return res
}
