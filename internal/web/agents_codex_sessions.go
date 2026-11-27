package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/sshx"
)

// GET /api/sessions/{id}/codex-sessions?dir=… → {"sessions": [{id, modified,
// title, first}]}: the Codex conversations of a folder that may be taken up
// (codex resume) from the new-agent dialog, newest first — the person's own
// (begun in its terminal or an editor's), not a subagent's nor a review's,
// none archived; as Codex's own database (state_<n>.sqlite, read only) has
// them, under the account's CODEX_HOME or ~/.codex and those its running
// Codexes were given. Nothing where the host has no python3 or no such
// database.
func (s *Server) apiCodexSessions(w http.ResponseWriter, r *http.Request) {
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
	res, err := sshx.Exec(ctx, sess.Client(), codexSessionsCommand(dir), 256<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	out := res.Out
	if errors.Is(err, sshx.ErrBusy) {
		writeSSHChannelsBusy(w)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "host_error", "the host's list could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": parseCodexSessions(out)})
}

// codexSessionsCommand: the script listing a folder's Codex conversations
// (see apiCodexSessions), dir its one argument (a full path or one under
// ~/, as agentDir took it).
func codexSessionsCommand(dir string) string {
	return `command -v python3 >/dev/null 2>&1 || exit 0
exec python3 -I -c ` + sshx.QuoteShell(codexSessionsScript) + ` ` + sshx.QuoteShell(dir)
}

const codexSessionsScript = `import json, os, re, sqlite3, sys, urllib.parse
d = sys.argv[1]
if d == '~' or d.startswith('~/'):
    d = os.path.expanduser(d)
d = d.rstrip('/') or '/'
# (Its home: the account's — CODEX_HOME, else ~/.codex — and those its
# Codexes running now were given: one set in a shell's own profile, which
# this command does not read.)
homes = [(os.environ.get('CODEX_HOME') or os.path.expanduser('~/.codex')).rstrip('/')]
uid = os.getuid()
try:
    pids = [p for p in os.listdir('/proc') if p.isdigit()]
except OSError:
    pids = []
for p in pids:
    try:
        if os.stat('/proc/' + p).st_uid != uid:
            continue
        with open('/proc/' + p + '/comm') as f:
            if f.read().strip() != 'codex':
                continue
        with open('/proc/' + p + '/environ', 'rb') as f:
            for kv in f.read().split(b'\0'):
                if kv.startswith(b'CODEX_HOME='):
                    h = kv[len(b'CODEX_HOME='):].decode('utf-8', 'replace').rstrip('/')
                    if h and h not in homes:
                        homes.append(h)
    except OSError:
        continue
out, seen = [], set()
for home in homes[:8]:
    best, n = None, -1
    try:
        names = os.listdir(home)
    except OSError:
        continue
    for name in names:
        m = re.fullmatch(r'state_(\d+)\.sqlite', name)
        if m and int(m.group(1)) > n:
            best, n = os.path.join(home, name), int(m.group(1))
    if not best:
        continue
    try:
        con = sqlite3.connect('file:' + urllib.parse.quote(best) + '?mode=ro', uri=True, timeout=2)
        for r in con.execute("SELECT id, name, title, first_user_message, updated_at FROM threads WHERE cwd = ? AND source IN ('cli', 'vscode') AND COALESCE(thread_source, '') NOT IN ('guardian_review', 'subagent') AND COALESCE(archived, 0) = 0 ORDER BY updated_at DESC LIMIT 50", (d,)):
            if r[0] in seen:
                continue
            seen.add(r[0])
            out.append({'id': r[0], 'name': (r[1] or '')[:200], 'title': (r[2] or '')[:200], 'first': (r[3] or '')[:200], 'at': r[4] or 0})
    except sqlite3.Error:
        continue
out.sort(key=lambda x: -x['at'])
sys.stdout.write(json.dumps(out[:50]))
`

// codexOld is a Codex conversation to take up, as the dialog lists it.
type codexOld struct {
	ID       string `json:"id"`
	Modified int64  `json:"modified"` // (unix ms)
	Title    string `json:"title,omitempty"`
	First    string `json:"first,omitempty"`
}

// parseCodexSessions reads codexSessionsCommand's list: only rows of a
// thread's id; its name where it has one, else its title, as its title.
func parseCodexSessions(out []byte) []codexOld {
	var rows []struct {
		ID, Name, Title, First string
		At                     int64
	}
	if json.Unmarshal(out, &rows) != nil {
		return []codexOld{}
	}
	list := make([]codexOld, 0, len(rows))
	for _, r := range rows {
		if !sessionUUID.MatchString(r.ID) {
			continue
		}
		title := strings.TrimSpace(r.Name)
		if title == "" {
			title = strings.TrimSpace(r.Title)
		}
		list = append(list, codexOld{ID: strings.ToLower(r.ID), Modified: r.At * 1000, Title: truncate(title, 200), First: truncate(strings.TrimSpace(r.First), 200)})
	}
	return list
}
