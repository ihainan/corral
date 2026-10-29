package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// A new agent from the conversation page: in a herdr session, a workspace of
// its own in the folder asked for, the agent started in its pane (herdr's
// own start, which waits until it is ready), or Claude Code taking up a
// session of before (--resume). All through herdr's socket API: the folder
// and the arguments go as they are, read by no shell. One at a time per
// person and session; a request said again (its key) gets the first one's
// answer, not a second agent.

var (
	agentKinds  = map[string]bool{"claude": true, "codex": true, "opencode": true}
	newAgentKey = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	sessionUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

const (
	newAgentWait  = 45 * time.Second // herdr's wait for the agent to be ready
	newAgentKept  = 10 * time.Minute // a request's answer, kept for it said again
	maxAgentLabel = 40
	maxAgentDir   = 1024
)

type newAgentRun struct {
	done   chan struct{}
	status int
	body   []byte
	at     time.Time
}

type newAgents struct {
	mu   sync.Mutex
	runs map[string]*newAgentRun // by person, session, key
	busy map[string]bool         // by person and session
}

// POST /api/sessions/{id}/agents {"dir", "kind", "label", "resume", "key"}
// → {"workspace", "pane"}; 409 unsupported (not herdr), in_progress (that
// key under way), 429 busy (another under way); 400 bad_folder; 422
// start_failed (herdr's own words; "closed": the workspace made for it was
// closed again).
func (s *Server) apiNewAgent(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Dir    string `json:"dir"`
		Kind   string `json:"kind"`
		Label  string `json:"label"`
		Resume string `json:"resume"`
		Key    string `json:"key"`
	}
	if err := readJSON(r, &b); err != nil || !agentKinds[b.Kind] || !newAgentKey.MatchString(b.Key) || (b.Resume != "" && (b.Kind != "claude" || !sessionUUID.MatchString(b.Resume))) {
		writeError(w, http.StatusBadRequest, "bad_request", "folder, kind and key are required")
		return
	}
	dir, ok := agentDir(b.Dir)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_folder", "a folder's full path (or one under ~/) is required")
		return
	}
	sess, socket := s.herdrSession(w, r)
	if sess == nil {
		return
	}
	u := currentUser(r)
	who := sess.ID + "\x00" + strconv.FormatInt(u.ID, 10)
	key := who + "\x00" + b.Key
	run, first, busy := s.newAgents.take(key, who)
	switch {
	case busy:
		writeError(w, http.StatusTooManyRequests, "busy", "another agent is being started in this session")
		return
	case !first:
		select {
		case <-run.done:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(run.status)
			_, _ = w.Write(run.body)
		default:
			writeError(w, http.StatusConflict, "in_progress", "this agent is still being started")
		}
		return
	}
	// (Not the asking request's: once begun, it is seen through, what it
	// made closed again where it failed, whoever is still waiting.)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), newAgentWait+30*time.Second)
	defer cancel()
	rec := &recorder{header: http.Header{}}
	s.newAgent(ctx, rec, sess, socket, dir, b.Kind, b.Label, b.Resume)
	s.newAgents.finish(key, who, run, rec.status, rec.body.Bytes())
	for k, v := range rec.header {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
	outcome := "started"
	if rec.status != http.StatusOK {
		outcome = "failed"
	}
	actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer acancel()
	s.audit(r.WithContext(actx), "agents.new", sess.HostName, map[string]any{"session": sess.ID, "kind": b.Kind, "resume": b.Resume != "", "outcome": outcome})
}

func (s *Server) newAgent(ctx context.Context, w http.ResponseWriter, sess *term.Session, socket, dir, kind, label, resume string) {
	// The folder: the home's when under ~/, a folder that is there.
	err := s.withSFTP(ctx, sess, convoReadLimit, func(c *sftplite.Client) error {
		if strings.HasPrefix(dir, "~") {
			home, err := c.RealPath(".")
			if err != nil {
				return err
			}
			dir = path.Join(home, strings.TrimPrefix(dir, "~"))
		}
		fi, err := c.Stat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return errNotDir
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotDir) {
		writeError(w, http.StatusBadRequest, "bad_folder", "no such folder")
		return
	}
	if err != nil {
		writeFileError(w, err)
		return
	}
	label = agentLabel(label, dir)
	c := herdrDial(w, ctx, sess, socket)
	if c == nil {
		return
	}
	ws, pane, err := c.CreateWorkspace(ctx, dir, label)
	c.Close()
	var apiErr *herdr.APIError
	switch {
	case errors.As(err, &apiErr):
		writeError(w, http.StatusUnprocessableEntity, "start_failed", apiErr.Message)
		return
	case err != nil || ws == "" || !herdrPaneID.MatchString(pane):
		// (Made or not, or made in part, it cannot be told: not closed on a guess.)
		writeErrorExtra(w, http.StatusBadGateway, "unknown", "herdr did not say what it made", map[string]any{"workspace": ws})
		return
	}
	// The agent: started by herdr's own start (which waits until herdr
	// finds it ready, not just started), told plainly whether it failed.
	failed, why := s.startAgent(ctx, sess, socket, pane, agentName(label, kind), kind, resume)
	switch {
	case failed:
		closed := s.closeWorkspace(sess, socket, ws) == nil
		writeErrorExtra(w, http.StatusUnprocessableEntity, "start_failed", why, map[string]any{"closed": closed, "workspace": ws})
	case why != "":
		// (Started or not, it cannot be told: left as it is, for the person to see.)
		writeErrorExtra(w, http.StatusBadGateway, "unknown", why, map[string]any{"workspace": ws, "pane": pane})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"workspace": ws, "pane": pane})
	}
}

// startAgent runs herdr's own `agent start` on the host, against the
// session's herdr: failed (with why) where herdr said it failed; why alone
// where what became of it is not known.
func (s *Server) startAgent(ctx context.Context, sess *term.Session, socket, pane, name, kind, resume string) (failed bool, why string) {
	q := sshx.QuoteShell
	cmd := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH" HERDR_SOCKET_PATH=` + q(socket) +
		` herdr agent start ` + q(name) + ` --kind ` + q(kind) + ` --pane ` + q(pane) + ` --timeout ` + strconv.FormatInt(newAgentWait.Milliseconds(), 10)
	if resume != "" {
		cmd += ` -- --resume ` + q(resume)
	}
	cmd += ` 2>&1`
	out, err := sshx.Output(ctx, sess.Client(), cmd, 64<<10)
	var res struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	line := bytes.TrimSpace(out)
	if i := bytes.LastIndexByte(line, '\n'); i >= 0 {
		line = line[i+1:] // (its answer: the last line)
	}
	if json.Unmarshal(line, &res) != nil {
		if err == nil {
			return false, "herdr said nothing that can be read"
		}
		return false, "herdr did not answer"
	}
	if res.Error != nil {
		why := clipText(res.Error.Message, 300)
		// (herdr lost hold of it, or of its name, on the way: started or not,
		// it cannot be told.)
		if strings.Contains(res.Error.Code, "transport") || res.Error.Code == "agent_name_not_found" {
			return false, why
		}
		return true, why
	}
	if err != nil || len(res.Result) == 0 {
		return false, "herdr did not say whether it started"
	}
	return false, ""
}

// closeWorkspace closes the workspace made for an agent that did not
// start: by its id, on a context of its own.
func (s *Server) closeWorkspace(sess *term.Session, socket, ws string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := herdr.Dial(ctx, sess.Client(), socket)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.CloseWorkspace(ctx, ws)
}

func (a *newAgents) take(key, who string) (run *newAgentRun, first, busy bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs == nil {
		a.runs, a.busy = map[string]*newAgentRun{}, map[string]bool{}
	}
	for k, r := range a.runs {
		select {
		case <-r.done:
			if time.Since(r.at) > newAgentKept {
				delete(a.runs, k)
			}
		default:
		}
	}
	if r, ok := a.runs[key]; ok {
		return r, false, false
	}
	if a.busy[who] {
		return nil, false, true
	}
	r := &newAgentRun{done: make(chan struct{}), at: time.Now()}
	a.runs[key], a.busy[who] = r, true
	return r, true, false
}

func (a *newAgents) finish(key, who string, r *newAgentRun, status int, body []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.status, r.body, r.at = status, append([]byte(nil), body...), time.Now()
	close(r.done)
	delete(a.busy, who)
}

// agentDir: a folder as asked for, cleaned: a full path, or one under ~
// (the home's); none with control characters, or too long.
func agentDir(d string) (string, bool) {
	d = strings.TrimSpace(d)
	if d == "" || len(d) > maxAgentDir || !utf8.ValidString(d) || strings.ContainsFunc(d, unicode.IsControl) {
		return "", false
	}
	switch {
	case d == "~":
		return "~", true
	case strings.HasPrefix(d, "~/"):
		return "~" + path.Clean("/"+d[2:]), true
	case strings.HasPrefix(d, "/"):
		return path.Clean(d), true
	}
	return "", false
}

// agentLabel: the workspace's name, as asked for (without control
// characters, at most maxAgentLabel), else the folder's own.
func agentLabel(l, dir string) string {
	l = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, l))
	if l == "" {
		l = path.Base(dir)
	}
	if utf8.RuneCountInString(l) > maxAgentLabel {
		l = string([]rune(l)[:maxAgentLabel])
	}
	return l
}

// agentName: the name herdr knows the agent by (a lowercase letter first,
// then letters, digits and dashes, at most 32): the label's, else the
// kind, with a few letters of its own after (names are herdr's, one each).
func agentName(label, kind string) string {
	base := agentNameBase(label, kind)
	if len(base) > 26 {
		base = strings.TrimRight(base[:26], "-")
	}
	return base + "-" + randomLetters(5)
}

func agentNameBase(label, kind string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 32 {
			break
		}
	}
	n := strings.Trim(b.String(), "-")
	if n == "" {
		return kind
	}
	if n[0] < 'a' || n[0] > 'z' {
		n = kind + "-" + n
	}
	return n
}

func randomLetters(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// recorder keeps an answer to give it again (see apiNewAgent).
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(s int) {
	if r.status == 0 {
		r.status = s
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// Claude Code's sessions of a folder, the latest first, to take one up again.

type claudeSession struct {
	ID       string `json:"id"`
	Modified int64  `json:"modified"` // (unix ms)
	Size     int64  `json:"size"`
	Title    string `json:"title,omitempty"`
	First    string `json:"first,omitempty"` // the first thing the person said
}

const (
	maxSessionsListed = 30
	maxSessionsLooked = 2000 // (folder entries looked at)
	sessionHead       = 16 << 10
)

type sessionLists struct {
	mu sync.Mutex
	m  map[string]sessionList
}

type sessionList struct {
	at      time.Time
	list    []claudeSession
	partial bool
}

// GET /api/sessions/{id}/claude-sessions?dir= → {"sessions": [...], "partial"}:
// of the folder's sessions in ~/.claude/projects (its name made of the
// folder's as Claude Code makes it), the latest maxSessionsListed by when
// they were written, each only if its log says it is of that very folder.
func (s *Server) apiClaudeSessions(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	dir, ok := agentDir(r.URL.Query().Get("dir"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_folder", "a folder's full path (or one under ~/) is required")
		return
	}
	u := currentUser(r)
	key := sess.ID + "\x00" + strconv.FormatInt(u.ID, 10) + "\x00" + dir
	s.claudeLists.mu.Lock()
	if l, ok := s.claudeLists.m[key]; ok && time.Since(l.at) < time.Minute {
		s.claudeLists.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"sessions": l.list, "partial": l.partial})
		return
	}
	s.claudeLists.mu.Unlock()
	var list []claudeSession
	partial := false
	// (All of it within its time, the connection's making too: what was
	// read by then is given.)
	until := time.Now().Add(8 * time.Second)
	err := s.withSFTPUntil(r.Context(), sess, time.Now().Add(12*time.Second), func(c *sftplite.Client) error {
		home, err := c.RealPath(".")
		if err != nil {
			return err
		}
		if strings.HasPrefix(dir, "~") {
			dir = path.Join(home, strings.TrimPrefix(dir, "~"))
		}
		projects := home + "/.claude/projects"
		folder := projects + "/" + claudeProjectName(dir)
		for _, p := range []string{home + "/.claude", projects, folder} {
			fi, err := c.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil // (none of that folder)
			}
			if err != nil {
				return err
			}
			if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
				return nil // (not followed: a link is no folder of sessions)
			}
		}
		entries, more, err := c.ReadDirLimit(folder, maxSessionsLooked)
		if err != nil {
			return err
		}
		partial = more
		var logs []fs.FileInfo
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".jsonl")
			if ok && sessionUUID.MatchString(id) && e.Mode().IsRegular() {
				logs = append(logs, e)
			}
		}
		sort.Slice(logs, func(i, j int) bool { return logs[i].ModTime().After(logs[j].ModTime()) })
		if len(logs) > maxSessionsListed {
			logs = logs[:maxSessionsListed]
		}
		for _, e := range logs {
			if time.Now().After(until) {
				partial = true
				break
			}
			head, err := c.ReadRange(folder+"/"+e.Name(), 0, sessionHead)
			if err != nil {
				partial = true // (one not read: the list is not whole)
				continue
			}
			cs, cwd := readSessionHead(head)
			if cwd != dir {
				continue // (another folder's, named alike; or not told)
			}
			cs.ID, cs.Modified, cs.Size = strings.TrimSuffix(e.Name(), ".jsonl"), e.ModTime().UnixMilli(), e.Size()
			list = append(list, cs)
		}
		return nil
	})
	if err != nil && len(list) == 0 {
		writeFileError(w, err)
		return
	}
	if err != nil {
		partial = true // (cut short: what was read is given)
	}
	if list == nil {
		list = []claudeSession{}
	}
	s.claudeLists.mu.Lock()
	if s.claudeLists.m == nil {
		s.claudeLists.m = map[string]sessionList{}
	}
	for k, l := range s.claudeLists.m {
		if time.Since(l.at) > time.Minute {
			delete(s.claudeLists.m, k)
		}
	}
	s.claudeLists.m[key] = sessionList{at: time.Now(), list: list, partial: partial}
	s.claudeLists.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list, "partial": partial})
}

// claudeProjectName: the name of a folder's sessions in ~/.claude/projects,
// as Claude Code makes it (each character not a letter or digit a dash):
// more than one folder may have it.
func claudeProjectName(dir string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return '-'
	}, dir)
}

// readSessionHead reads the start of a session's log: its folder (cwd),
// its title, the first thing the person said.
func readSessionHead(head []byte) (cs claudeSession, cwd string) {
	for _, line := range bytes.Split(head, []byte("\n")) {
		var r struct {
			Type        string          `json:"type"`
			Cwd         string          `json:"cwd"`
			IsMeta      bool            `json:"isMeta"`
			IsSidechain bool            `json:"isSidechain"`
			AITitle     string          `json:"aiTitle"`
			CustomTitle string          `json:"customTitle"`
			Message     json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		if cwd == "" && r.Cwd != "" && !r.IsSidechain {
			cwd = r.Cwd
		}
		switch r.Type {
		case "custom-title":
			cs.Title = clipText(r.CustomTitle, 120)
		case "ai-title":
			if cs.Title == "" {
				cs.Title = clipText(r.AITitle, 120)
			}
		case "user":
			if cs.First != "" || r.IsMeta || r.IsSidechain {
				continue
			}
			var m struct {
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(r.Message, &m) != nil {
				continue
			}
			var text string
			if json.Unmarshal(m.Content, &text) != nil {
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(m.Content, &blocks) == nil && len(blocks) > 0 && blocks[0].Type == "text" {
					text = blocks[0].Text
				}
			}
			if t := strings.TrimSpace(text); t != "" && !strings.HasPrefix(t, "<") {
				cs.First = clipText(t, 200)
			}
		}
	}
	return cs, cwd
}

func clipText(s string, n int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}
