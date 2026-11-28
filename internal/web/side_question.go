package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
)

// A side question (Claude Code's /btw, from the page): a question asked of
// a Claude Code's conversation as it stands, mid-turn too, answered beside
// it and never written into it. Claude Code's own /btw answer is in neither
// its log nor anything else that can be read, so the gateway asks a copy:
// on the host, "claude -p --resume <its session> --fork-session
// --no-session-persistence" (no tools, no hooks), from where the agent runs
// and with its CLAUDE_CONFIG_DIR, so its log is the one resumed. The agent
// is not interrupted, nothing is added to its log and no session file is
// left. It is a model call of its own, on the account the agent's is on.
// (Seen on 2.1.325: --bare reads no login, so it is not used; without
// stdin closed it waits 3s for it.)

const (
	sideQuestionMax  = 2000              // runes of a question
	sideAnswerCap    = 256 << 10         // bytes of what the host prints (its JSON cut there: a failure)
	sideRunSecs      = 90                // the host's own limit on the run
	sideRequestLimit = 100 * time.Second // the gateway's, past the host's (and its kill's)
	// sideHostEnd: by when a copy begun has surely ended on the host (its
	// timeout, the kill 5s after, and some): one not seen to end is waited
	// for till then before another is asked in the session.
	sideHostEnd = sideRunSecs*time.Second + 10*time.Second
)

// sideModel: a model's name as Claude Code's --model takes it (an alias, an
// id, a Bedrock or Vertex one, a "[1m]" suffix), never one that would read
// as an option.
var sideModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// sideQuestionText: the question as it is passed (trimmed), and whether it
// may be: some words, no longer than sideQuestionMax, valid UTF-8, no
// control characters but a line break and a tab. (It is the command's
// input, never an argument: one begun with "-" is no option.)
func sideQuestionText(q string) (string, bool) {
	q = strings.TrimSpace(q)
	if q == "" || !utf8.ValidString(q) || utf8.RuneCountInString(q) > sideQuestionMax {
		return "", false
	}
	if strings.ContainsFunc(q, func(r rune) bool {
		return r != '\n' && r != '\t' && (unicode.IsControl(r) || r == ' ' || r == ' ')
	}) {
		return "", false
	}
	return q, true
}

// sideQuestionCommand asks Claude Code process pid (started at start, by
// /proc/<pid>/stat's field 22) the question given as its input (and passed
// on as the copy's) of its session sid, as a copy of the session that is
// not kept: see above. It prints "E <why>" (gone:
// the process has ended or is another; nodir; noclaude; notimeout), or
// "<nonce> OUT", the command's JSON (cut at sideAnswerCap) and
// "<nonce> RC <status>" where it ran to its end. Fixed script; pid and start
// are numbers, sid a UUID, model sideModel's and the nonce hex, each
// quoted, all positional arguments; the question (sideQuestionText's) is
// never one. Where it runs, and its environment of sideEnvNames, are the
// process's own, read from /proc on the host.
func sideQuestionCommand(pid, start int64, sid, model, nonce string) string {
	return sideQuestionRun(pid, start, sid, model, nonce, sideRunSecs)
}

// sideQuestionRun is sideQuestionCommand with its time on the host, in
// seconds (tests: a short one).
func sideQuestionRun(pid, start int64, sid, model, nonce string, secs int) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`p=$1; st=$2; sid=$3; m=$4; N=$5; S=$6; ` +
		claudeConfigDirSh +
		// (Still that process — its start time after the last ") ", whatever its name — at that
		// session, as its session file says now (not one it has left by /clear or /resume
		// since it was found). Asked before and after what is read of it: what is read is its.)
		`same() { x=; { IFS= read -r x; } <"/proc/$p/stat" 2>/dev/null; x=${x##*) }; set -f; set -- $x; set +f; ` +
		`[ -n "$x" ] && [ "${20-}" = "$st" ] || return 1; ` +
		`claude_cfg_dir "/proc/$p"; ` +
		`c=$(head -c 65536 "$clcfg/sessions/$p.json" 2>/dev/null | tr -d '\n'); ` +
		`[ "$(printf '%s' "$c" | grep -o '"sessionId" *: *"[0-9a-fA-F-]*"' | head -n 1 | sed 's/.*"\([0-9a-fA-F-]*\)"$/\1/')" = "$sid" ]; }; ` +
		`same && [ -r "/proc/$p/environ" ] || { echo 'E gone'; exit 0; }; ` +
		`cwd=$(readlink "/proc/$p/cwd" 2>/dev/null); ` +
		// (Its config folder and how it signs in and where its requests go — sideEnvNames, as it
		// was started with them, none it had not: not the login shell's. Nothing else
		// of its environment: its pane's would make the copy look like the pane's agent. Each
		// variable read whole, by the NULs between them — they alone made line breaks, its own
		// line breaks made \001s: no value can pass for another variable — and one with a line
		// break or a \001 in it is not taken.)
		`ev=$(tr '\0\n' '\n\001' <"/proc/$p/environ" 2>/dev/null | awk -v x="$(printf '\001')" '/^(` + strings.ReplaceAll(sideEnvNames, " ", "|") + `)=/ && index($0, x) == 0 { print }' | head -c 16384); ` +
		// (Its PATH too, only to find Claude Code by, below: not the copy's.)
		`ap=$(tr '\0\n' '\n\001' <"/proc/$p/environ" 2>/dev/null | sed -n 's/^PATH=//p' | head -n 1); ` +
		`same || { echo 'E gone'; exit 0; }; ` +
		`[ -n "$cwd" ] && cd -- "$cwd" 2>/dev/null || { echo 'E nodir'; exit 0; }; ` +
		`unset ` + sideEnvNames + ` TMUX TMUX_PANE HERDR_PANE_ID HERDR_SOCKET_PATH; ` +
		`while IFS= read -r v; do case $v in ?*=*) export "$v";; esac; done <<CORRAL_SIDE_ENV
$ev
CORRAL_SIDE_ENV
` +
		// (The copy says what it is: the locating scan does not take it for an agent.)
		`export ` + sideCopyMark + `; ` +
		// (Claude Code as the shell finds it; else as the agent's own PATH does — one put there by
		// nvm, say: its folder, where its node is too, put first on the copy's PATH, and no more of
		// the agent's PATH, of which only folders by their whole path are looked in; else the one
		// the process runs, where that is Claude Code's own build. At each, only a file that may be
		// run: BusyBox's command -v names one that may not.)
		`run_ok() { [ -f "$1" ] && [ -x "$1" ]; }; ` +
		`C=$(command -v claude 2>/dev/null); run_ok "$C" || C=; ` +
		`if [ -z "$C" ]; then set -f; o=$IFS; IFS=:; for d in $ap; do case $d in /*) if run_ok "$d/claude"; then C=$d/claude; PATH="$d:$PATH"; break; fi;; esac; done; IFS=$o; set +f; fi; ` +
		`if [ -z "$C" ]; then e=$(readlink "/proc/$p/exe" 2>/dev/null); case $e in */claude/versions/*) run_ok "$e" && C=$e;; esac; fi; ` +
		`[ -n "$C" ] || { echo 'E noclaude'; exit 0; }; ` +
		// (Ended for sure, with all it started: run in a process group of its own (setsid), the
		// whole group killed when its time is up, and once it has ended whatever of it is left — a
		// "claude" that is a wrapper may end before the child it started, which would hold the
		// answer's pipe open. No setsid: not run, as it may never end.)
		`command -v setsid >/dev/null 2>&1 || { echo 'E notimeout'; exit 0; }; ` +
		// (No MCP server started — "--tools" alone still starts the user's and the project's —
		// no hook run, no tool to use.)
		`set -- -p --resume "$sid" --fork-session --no-session-persistence --tools "" --strict-mcp-config --mcp-config '{"mcpServers":{}}' --settings '{"disableAllHooks":true}' --output-format json; ` +
		`[ -n "$m" ] && set -- "$@" --model "$m"; ` +
		`printf '%s OUT\n' "$N"; ` +
		// (The question is this script's input, given on to it: never in a command line, which
		// any user of the host can read.)
		// (An asynchronous command's input is /dev/null unless it is given one: this script's, as 3.
		// Not a group leader, setsid makes the copy one in place — its pid its group's. The watch
		// is a group of its own too, its sleep with it: killed whole once the copy has ended, it
		// leaves nothing behind — by its pid first, then its group: the copy may end before the
		// watch's setsid has made its group (its pid killed, its sleep never begins), and the wait
		// for it must not last its whole sleep. Neither holds
		// the answer's pipe but the copy. No "--" before a group: dash's kill takes none.)
		`exec 3<&0; ` +
		`{ setsid "$C" "$@" <&3 3<&- 2>/dev/null & cp=$!; ` +
		`setsid sh -c 'sleep "$1"; kill -KILL "-$2"' sh "$S" "$cp" </dev/null >/dev/null 2>&1 3<&- & w=$!; ` +
		`wait "$cp"; rc=$?; kill -KILL "-$cp" 2>/dev/null; kill -KILL "$w" 2>/dev/null; kill -KILL "-$w" 2>/dev/null; wait "$w" 2>/dev/null; ` +
		`printf '\n%s RC %s\n' "$N" "$rc"; } | head -c ` + strconv.Itoa(sideAnswerCap) + `; exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + strconv.FormatInt(pid, 10) + " " + strconv.FormatInt(start, 10) + " " +
		sshx.QuoteShell(sid) + " " + sshx.QuoteShell(model) + " " + sshx.QuoteShell(nonce) + " " + strconv.Itoa(secs)
}

// sideEnvNames: what of a Claude Code's environment its side question's copy
// is run with, none of the login shell's: its config folder, where its
// requests go and how it signs in (claude_ep's list, an account's token
// given by setup-token, and what picks Amazon's or Google's account) — so
// the copy asks as the agent does, on its account.
// sideCopyMark: in the environment of a side question's copy (claude -p),
// which writes a session file while it runs: no agent of a pane, left out
// by the locating scan (see convoLocateWith).
const sideCopyMark = "CORRAL_SIDE_COPY=1"

const sideEnvNames = "CLAUDE_CONFIG_DIR ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN " +
	"CLAUDE_CODE_USE_BEDROCK AWS_PROFILE AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE AWS_REGION AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_BEARER_TOKEN_BEDROCK " +
	"CLAUDE_CODE_USE_VERTEX ANTHROPIC_VERTEX_PROJECT_ID CLOUD_ML_REGION GOOGLE_APPLICATION_CREDENTIALS"

// sideAnswer is what the page is told of a side question asked.
type sideAnswer struct {
	OK         bool    `json:"ok"`
	Answer     string  `json:"answer,omitempty"`
	Reason     string  `json:"reason,omitempty"` // (not ok: gone, nodir, noclaude, notimeout, timeout, cut, failed, error)
	DurationMs int64   `json:"duration_ms,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
}

var errSideOutput = errors.New("side question: output not understood")

// parseSideAnswer reads sideQuestionCommand's output.
func parseSideAnswer(out, nonce string) (sideAnswer, error) {
	if why, ok := strings.CutPrefix(out, "E "); ok {
		why = strings.TrimSpace(why)
		switch why {
		case "gone", "nodir", "noclaude", "notimeout":
			return sideAnswer{Reason: why}, nil
		}
		return sideAnswer{}, errSideOutput
	}
	body, ok := strings.CutPrefix(out, nonce+" OUT\n")
	if !ok {
		return sideAnswer{}, errSideOutput
	}
	i := strings.LastIndex(body, "\n"+nonce+" RC ")
	if i < 0 {
		return sideAnswer{Reason: "cut"}, nil // (past the cap: the end not reached)
	}
	rc, err := strconv.Atoi(strings.TrimSpace(body[i+len("\n"+nonce+" RC "):]))
	if err != nil {
		return sideAnswer{}, errSideOutput
	}
	if rc == 124 || rc == 137 || rc == 143 {
		return sideAnswer{Reason: "timeout"}, nil
	}
	var res struct {
		Type     string   `json:"type"`
		Subtype  string   `json:"subtype"`
		IsError  bool     `json:"is_error"`
		Result   *string  `json:"result"`
		Duration int64    `json:"duration_ms"`
		Cost     *float64 `json:"total_cost_usd"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body[:i])), &res) != nil || res.Type != "result" {
		return sideAnswer{Reason: "failed"}, nil // (it said nothing that can be read: not logged in, say)
	}
	a := sideAnswer{DurationMs: max(res.Duration, 0)}
	if res.Cost != nil && *res.Cost > 0 {
		a.CostUSD = *res.Cost
	}
	if res.IsError || res.Subtype != "success" || res.Result == nil || rc != 0 {
		a.Reason = "error"
		if res.Result != nil {
			a.Answer = *res.Result // (Claude Code's own word on it: "Not logged in", a limit reached)
		}
		return a, nil
	}
	a.OK, a.Answer = true, *res.Result
	return a, nil
}

// sideAsking: the Corral sessions a side question is under way in — one at
// a time to each, as it holds one of the host connection's few channels
// for up to a minute and a half.
var sideAsking = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func sideTake(id string) bool {
	sideAsking.Lock()
	defer sideAsking.Unlock()
	if sideAsking.m[id] {
		return false
	}
	sideAsking.m[id] = true
	return true
}

func sideGive(id string) {
	sideAsking.Lock()
	delete(sideAsking.m, id)
	sideAsking.Unlock()
}

// POST /api/sessions/{id}/conversation/side {pane, pid, start, session,
// model?, question}: the question asked of that Claude Code's conversation
// beside it (see above) — the process the pane runs now, at that session,
// as a press checks it. Its answer, or why there is none (sideAnswer).
// Each is audited, without its words.
func (s *Server) apiConversationSide(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Pane     string `json:"pane"`
		PID      int64  `json:"pid"`
		Start    int64  `json:"start"`
		Session  string `json:"session"`
		Model    string `json:"model"`
		Question string `json:"question"`
	}
	err := readJSON(r, &b)
	q, qOK := sideQuestionText(b.Question)
	if err != nil || !qOK || (b.Model != "" && !sideModel.MatchString(b.Model)) || b.PID <= 0 || b.Start <= 0 ||
		!convoUUID.MatchString(b.Session) || b.Session == convoNoSession ||
		(sess.Persistence == "herdr" && !herdrPane.MatchString(b.Pane)) || (sess.Persistence == "tmux" && !tmuxPane.MatchString(b.Pane)) {
		writeError(w, http.StatusBadRequest, "bad_request", "the pane, the process, its session and a question are required")
		return
	}
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeError(w, http.StatusConflict, "unsupported", "only in tmux and herdr sessions")
		return
	}
	outcome, reason := "refused", ""
	defer func() {
		actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer acancel()
		detail := map[string]any{"session": sess.ID, "pane": b.Pane, "pid": b.PID, "start": b.Start, "claude_session": b.Session, "length": utf8.RuneCountInString(q), "outcome": outcome}
		if b.Model != "" {
			detail["model"] = b.Model
		}
		if reason != "" {
			detail["reason"] = reason
		}
		s.audit(r.WithContext(actx), "conversation.side", sess.HostName, detail)
	}()
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		reason = "signed_out"
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	if !sideTake(sess.ID) {
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another side question is still being answered here")
		return
	}
	// (Given back once the copy has ended — where it was not seen to end (this
	// request given up on it, the connection lost), once its own timeout on
	// the host has surely ended it: a question asked again before would be a
	// second at once, the first still running there.)
	held := true
	defer func() {
		if held {
			sideGive(sess.ID)
		}
	}()
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	untrack := s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()

	// The pane's agent is that process, a Claude Code, at that session — as
	// the pane's conversation is read and a press checks it.
	tmuxTarget := ""
	if sess.Persistence == "tmux" {
		tmuxTarget = b.Pane
	}
	socket := s.herdrSocks.get(sess.ID)
	wantSID := s.herdrSocks.sid(sess.ID, b.Pane)
	var fastPlan herdrFastPlan
	if sess.Persistence == "herdr" && b.Pane != "" && socket != "" {
		hp, hasHP := s.herdrSocks.paneOf(sess.ID, b.Pane)
		fastPlan = planHerdrFastLocate(s.herdrPaneProcessInfo(ctx, sess, socket, b.Pane), b.Pane, hp, hasHP)
	}
	var front *convoProc
	for attempt := 0; attempt < 2; attempt++ {
		tmuxName := sess.CurrentTmuxSession()
		params := proto.AgentLocateParams{Mode: sess.Persistence, TmuxName: tmuxName, TmuxPane: tmuxTarget}
		cmd := convoLocateCommand(sess.Persistence, tmuxName, tmuxTarget)
		fastTry := attempt == 0 && fastPlan.mode == herdrFastPIDs
		if fastTry {
			params = proto.AgentLocateParams{Mode: "herdr", ShellPID: fastPlan.shellPID, PIDs: fastPlan.pids}
			cmd = convoLocatePidsCommand(fastPlan.shellPID, fastPlan.pids)
		}
		op := "locate.full"
		if fastTry {
			op = "locate.fast"
		}
		lctx, lcancel := context.WithTimeout(ctx, 15*time.Second)
		tLoc := time.Now()
		var out []byte
		if cout, ok := s.tryCorraldLocate(lctx, sess, params); ok {
			lcancel()
			out = cout
		} else {
			lr, err := sshx.Exec(lctx, sess.Client(), cmd, convoLocateMax, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
			lcancel()
			if err != nil {
				s.perfLocate(op, tLoc, err, 0, sess.HostID)
			}
			if errors.Is(err, sshx.ErrBusy) {
				reason = "ssh_channels_busy"
				writeSSHChannelsBusy(w)
				return
			}
			if err != nil && fastTry {
				continue
			}
			switch {
			case errors.Is(err, sshx.ErrBusy):
				reason = "ssh_channels_busy"
				writeSSHChannelsBusy(w)
				return
			case err != nil:
				reason = "host_error"
				writeError(w, http.StatusBadGateway, "host_error", "the host did not answer")
				return
			}
			out = lr.Out
		}
		l := parseConvoLocate(string(out))
		if !l.noProc {
			coveredPane := ""
			if fastTry {
				coveredPane = b.Pane
			}
			s.recordLocatedProcs(sess, l, !fastTry, coveredPane, tLoc)
		}
		s.perfLocate(op, tLoc, nil, len(l.procs), sess.HostID)
		if fastTry {
			if p, ok := fastLocateAccept(l, b.Pane, socket, wantSID); ok && !l.noProc && p != nil && p.agent == "claude" && p.pid == b.PID && p.start == b.Start && p.sid == b.Session {
				front = p
				break
			}
			continue
		}
		front = pickConvoProc(l.procs, sess.Persistence, b.Pane, socket, l.tmuxPID, l.paneTTY, wantSID)
		break
	}
	if front == nil || front.agent != "claude" || front.pid != b.PID || front.start != b.Start || front.sid != b.Session {
		reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	}

	// (The run's own time from now: not the scan's taken out of it.)
	ctx, cancelRun := context.WithTimeout(ctx, sideRequestLimit)
	defer cancelRun()
	nonce := gitNonce()
	res, err := sshx.Exec(ctx, sess.Client(), sideQuestionCommand(b.PID, b.Start, b.Session, b.Model, nonce), sideAnswerCap+256, sshx.ExecOptions{Stdin: strings.NewReader(q), Interactive: true, AwaitSlot: 2 * time.Second})
	if !res.Ended && !errors.Is(err, sshx.ErrBusy) {
		held = false
		id := sess.ID
		// (From now: the copy may have begun on the host as late as this.)
		time.AfterFunc(sideHostEnd, func() { sideGive(id) })
	}
	var a sideAnswer
	if err == nil {
		a, err = parseSideAnswer(string(res.Out), nonce)
	}
	switch {
	case errors.Is(err, sshx.ErrBusy):
		reason = "ssh_channels_busy"
		writeSSHChannelsBusy(w)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil && time.Now().Before(expiry):
		reason = "timeout"
		writeError(w, http.StatusGatewayTimeout, "timeout", "no answer in time")
	case err != nil:
		reason = "host_error"
		writeError(w, http.StatusBadGateway, "host_error", "the host did not say")
	default:
		outcome = "answered"
		if !a.OK {
			outcome, reason = "failed", a.Reason
		}
		writeJSON(w, http.StatusOK, a)
	}
}
