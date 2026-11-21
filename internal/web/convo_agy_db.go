package web

import (
	"bytes"
	"context"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

const (
	agyDBErrorsMax    = 512 << 10
	agyDBBusyEvery    = 2 * time.Second
	agyDBIdleEvery    = 15 * time.Second
	agyDBQueryTimeout = 5 * time.Second
)

const agyDBErrorsScript = `import os, sqlite3, sys, urllib.parse
db, lo = sys.argv[1], int(sys.argv[2])
if not os.path.isfile(db):
    sys.exit(0)
try:
    con = sqlite3.connect('file:' + urllib.parse.quote(db) + '?mode=ro', uri=True, timeout=2)
    rows = con.execute('SELECT idx, hex(step_payload) FROM steps WHERE step_type = 17 AND idx >= ? ORDER BY idx DESC LIMIT 64', (lo,)).fetchall()
    con.close()
except Exception:
    sys.exit(0)
for idx, hx in reversed(rows):
    if hx:
        print(f"{idx}\t{hx}")
`

// agyDBPathFromLog derives ~/.gemini/antigravity-cli/conversations/<sid>.db
// from st.log (~/.gemini/antigravity-cli/brain/<sid>/.system_generated/logs/transcript_full.jsonl).
func agyDBPathFromLog(logPath, sid string) string {
	sid = strings.ToLower(strings.TrimSpace(sid))
	if !convoUUID.MatchString(sid) || logPath == "" {
		return ""
	}
	needle := "/brain/" + sid + "/"
	i := strings.Index(strings.ToLower(logPath), needle)
	if i < 0 {
		return ""
	}
	return logPath[:i] + "/conversations/" + sid + ".db"
}

func agyDBErrorsCommand(dbPath string, minIdx int) string {
	if minIdx < 0 {
		minIdx = 0
	}
	return "python3 -c " + sshx.QuoteShell(agyDBErrorsScript) + " " + sshx.QuoteShell(dbPath) + " " + strconv.Itoa(minIdx)
}

func parseAgyDBErrors(out []byte) []convo.AgyErrorStep {
	var steps []convo.AgyErrorStep
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		tab := bytes.IndexByte(line, '\t')
		if tab <= 0 {
			continue
		}
		idx, err := strconv.Atoi(string(line[:tab]))
		if err != nil || idx <= 0 {
			continue
		}
		raw, err := hex.DecodeString(string(line[tab+1:]))
		if err != nil || len(raw) == 0 {
			continue
		}
		if step, ok := convo.DecodeAgyErrorStep(idx, raw); ok {
			steps = append(steps, step)
		}
	}
	return steps
}

// agyMergeDBErrors reads step_type = 17 (ERROR_MESSAGE) rows from agy's
// per-conversation SQLite database and merges them into st.parser. Antigravity
// CLI omits ERROR_MESSAGE steps from transcript_full.jsonl, so without this
// quota exhaustion, rate limits, auth/network/5xx errors, and safety refusals
// would be invisible in Chat Mode.
func (s *Server) agyMergeDBErrors(ctx context.Context, sess *term.Session, st *convoState, grew bool) {
	if st.agent != "agy" || st.sub || st.log == "" || st.sid == "" {
		return
	}
	ag, ok := st.parser.(*convo.Agy)
	if !ok {
		return
	}
	dbPath := agyDBPathFromLog(st.log, st.sid)
	if dbPath == "" {
		return
	}
	minIdx := ag.MinStepIndex()
	due := st.agyDBAt.IsZero() || grew || (minIdx < st.agyDBMinIdx)
	if !due {
		every := agyDBIdleEvery
		if st.status == "busy" || ag.Status() == "busy" {
			every = agyDBBusyEvery
		}
		if time.Since(st.agyDBAt) < every {
			return
		}
	}
	st.agyDBAt = time.Now()
	st.agyDBMinIdx = minIdx

	qctx, cancel := context.WithTimeout(ctx, agyDBQueryTimeout)
	defer cancel()
	out, err := sshx.Output(qctx, sess.Client(), agyDBErrorsCommand(dbPath, minIdx), agyDBErrorsMax)
	if err != nil || len(out) == 0 {
		return
	}
	errs := parseAgyDBErrors(out)
	if len(errs) == 0 {
		return
	}
	added, spliced := ag.MergeDBErrors(errs)
	if spliced {
		st.gen = convoGens.Add(1)
	}
	if added && ag.Status() == "idle" && st.status == "busy" {
		st.status = "idle"
	}
}

// agyRunningOnHost reconciles agy's log-tracked background tasks against the
// host process scan (cmdProber), mirroring codexRunningOnHost so tasks that
// exited or were killed without a SYSTEM_MESSAGE do not linger forever.
func (s *Server) agyRunningOnHost(sess *term.Session, st *convoState) {
	if st.agent != "agy" || st.sub || st.bind == nil || !convoUUID.MatchString(st.bind.Session) {
		return
	}
	ag, ok := st.parser.(*convo.Agy)
	if !ok {
		return
	}
	var p *cmdProber
	switch sess.Persistence {
	case "herdr":
		p = s.herdrSocks.cmdsOf(sess.ID)
	case "tmux":
		p = s.tmuxCmdsOf(sess)
	}
	if p == nil {
		return
	}
	sid := strings.ToLower(st.bind.Session)
	var ids []string
	for _, r := range ag.Running() {
		if r.Kind == "cmd" {
			ids = append(ids, r.ID)
		}
	}
	n := len(ids)
	p.readAt(sid, n > 0 && ag.Status() == "idle", strings.Join(ids, " "))
	live, ok := p.readSeen(sid)
	if !ok || n == 0 {
		return
	}
	all := ag.Running()
	ag.EndedOnHost(func(r convo.Running) bool { return agyRunGone(r, live, n, all) })
}

func agyRunGone(r convo.Running, live []bgCmd, n int, all []convo.Running) bool {
	if len(live) == 0 {
		return true
	}
	if n <= len(live) {
		return false
	}
	match := func(o convo.Running, title string) bool {
		return codexSameCmd(o.Cmd, title) || codexSameCmd(o.What, title)
	}
	for _, l := range live {
		found := false
		for _, o := range all {
			if o.Kind == "cmd" && match(o, l.Title) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		if match(r, l.Title) {
			return false
		}
	}
	return true
}
