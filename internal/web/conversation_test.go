package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const sidA = "194b7874-ce3d-46b3-b901-f53bd5747ce0"
const sidB = "607ffb6d-81da-4b8d-9d09-cad35b7a2ab4"

func convoRec(fields ...string) string { return strings.Join(fields, "\n") + "\x00" }

func TestParseConvoLocate(t *testing.T) {
	out := "tmux=%3 4242 /dev/pts/7 \x00" +
		convoRec("pid=100", "start=500", "fg=1", "procstart=500", "sid="+sidA, "log=/home/u/.claude/projects/-w/"+sidA+".jsonl",
			"tty=/dev/pts/4", "anc=90:zsh,80:herdr,", "HERDR_PANE_ID=w7:p1", "HERDR_SOCKET_PATH=/home/u/.config/herdr/herdr.sock") +
		// The pid was reused: the session file is a dead process's.
		convoRec("pid=101", "start=900", "procstart=600", "sid="+sidB, "log=") +
		convoRec("pid=102", "start=700", "procstart=700", "sid="+sidB, "log=", "TMUX=/tmp/tmux-1000/default,4242,0", "TMUX_PANE=%3") +
		// A log path that is not this session's is not read.
		convoRec("pid=103", "start=800", "procstart=800", "sid="+sidB, "log=/etc/passwd") +
		convoRec("pid=104", "start=800", "procstart=800", "sid=../../x", "log=") +
		convoRec("garbage")
	l := parseConvoLocate(out)
	procs := l.procs
	if l.tmuxActive != "%3" || l.tmuxPID != "4242" || l.tmuxTTY != "/dev/pts/7" || l.noProc {
		t.Fatalf("tmux %+v", l)
	}
	if len(procs) != 3 || !procs[0].fg || procs[1].fg {
		t.Fatalf("procs: %+v", procs)
	}
	if p := procs[0]; p.pid != 100 || p.sid != sidA || p.herdrPane != "w7:p1" || !strings.HasSuffix(p.log, sidA+".jsonl") ||
		p.tty != "/dev/pts/4" || len(p.anc) != 2 || p.anc[1] != (convoAnc{80, "herdr"}) {
		t.Errorf("herdr proc: %+v", p)
	}
	if p := procs[1]; p.pid != 102 || p.tmuxPane != "%3" || p.tmuxServerPID != "4242" || p.tmuxSocket != "/tmp/tmux-1000/default" {
		t.Errorf("tmux proc: %+v", p)
	}
	if p := procs[2]; p.pid != 103 || p.log != "" {
		t.Errorf("foreign log kept: %+v", p)
	}
	// When its log was last written: its last record's time over the
	// file's (Claude Code touches its log every hour, nothing written);
	// the file's where no record said, or what it said is no time.
	rec := func(f ...string) convoProc {
		r := parseConvoLocate(convoRec(append([]string{"pid=100", "start=500", "procstart=500", "sid=" + sidA, "log=/home/u/.claude/projects/-w/" + sidA + ".jsonl"}, f...)...)).procs
		if len(r) != 1 {
			t.Fatalf("no proc: %v", f)
		}
		return r[0]
	}
	if w := rec("logmtime=1761640746", "logts=2026-10-27T13:44:10.013Z").wrote; w != 1793108650013 {
		t.Errorf("by its last record: %d", w)
	}
	if w := rec("logts=2026-10-27T13:44:10.013Z", "logmtime=1761640746").wrote; w != 1793108650013 {
		t.Errorf("by its last record, told first: %d", w)
	}
	if w := rec("logmtime=1761640746", "logts=").wrote; w != 1761640746000 {
		t.Errorf("by the file: %d", w)
	}
	if w := rec("logmtime=1761640746", "logts=yesterday").wrote; w != 1761640746000 {
		t.Errorf("no time: %d", w)
	}
	if !parseConvoLocate("noproc\x00").noProc {
		t.Error("noproc not seen")
	}
	if l := parseConvoLocate("tmux=%3; rm -rf / 1 /dev/x\x00panetty=/dev/x y\x00"); l.tmuxActive != "" || l.tmuxTTY != "" || l.paneTTY != "" {
		t.Errorf("bad tmux answer accepted: %+v", l)
	}
}

func TestPickConvoProc(t *testing.T) {
	sock := "/home/u/.config/herdr/herdr.sock"
	inHerdr := []convoAnc{{90, "zsh"}, {80, "herdr"}}
	procs := []convoProc{
		{pid: 1, start: 10, sid: sidA, herdrPane: "w1:p1", herdrSocket: sock, anc: inHerdr},
		{pid: 2, start: 30, sid: sidB, herdrPane: "w1:p1", herdrSocket: sock, anc: inHerdr}, // started later in the same pane
		{pid: 3, start: 20, sid: sidA, herdrPane: "w2:p1", herdrSocket: "/other/herdr.sock", anc: inHerdr},
		{pid: 4, start: 40, sid: sidA, tmuxPane: "%3", tmuxServerPID: "4242", tty: "/dev/pts/3"},
		{pid: 5, start: 50, sid: sidB, tmuxPane: "%3", tmuxServerPID: "9999", tty: "/dev/pts/9"}, // another tmux server
	}
	if p := pickConvoProc(procs, "herdr", "w1:p1", sock, "", "", ""); p == nil || p.pid != 2 {
		t.Errorf("herdr: %+v", p)
	}
	// (herdr names the pane's session: that one, though another started later.)
	if p := pickConvoProc(procs, "herdr", "w1:p1", sock, "", "", sidA); p == nil || p.pid != 1 {
		t.Errorf("herdr, its session named: %+v", p)
	}
	if p := pickConvoProc(procs, "herdr", "w1:p1", sock, "", "", "00000000-0000-0000-0000-000000000000"); p == nil || p.pid != 2 {
		t.Errorf("herdr, a session named that none runs: %+v", p)
	}
	if p := pickConvoProc(procs, "herdr", "w2:p1", sock, "", "", ""); p != nil {
		t.Errorf("another herdr server's pane matched: %+v", p)
	}
	if p := pickConvoProc(procs, "herdr", "", sock, "", "", ""); p != nil {
		t.Errorf("no pane matched: %+v", p)
	}
	if p := pickConvoProc(procs, "herdr", "w1:p1", "", "", "", ""); p != nil {
		t.Errorf("no socket known, matched: %+v", p)
	}
	if p := pickConvoProc(procs, "tmux", "%3", "", "4242", "/dev/pts/3", ""); p == nil || p.pid != 4 {
		t.Errorf("tmux: %+v", p)
	}
	if p := pickConvoProc(procs, "none", "w1:p1", sock, "4242", "", ""); p != nil {
		t.Errorf("plain shell matched: %+v", p)
	}
}

// A Claude Code the agent started has the pane's variables: the agent's own
// is chosen, whether herdr's word on the session is current or not.
func TestPickConvoProcStartedInside(t *testing.T) {
	sock := "/s"
	agent := convoProc{pid: 100, start: 10, sid: sidA, herdrPane: "w1:p1", herdrSocket: sock, tty: "/dev/pts/4", anc: []convoAnc{{90, "zsh"}, {80, "herdr"}}}
	// In a tmux of its own: not under herdr.
	inTmux := convoProc{pid: 300, start: 50, sid: sidB, herdrPane: "w1:p1", herdrSocket: sock, tty: "/dev/pts/9", anc: []convoAnc{{290, "zsh"}, {280, "tmux__server"}}}
	// Run as a command: under the agent (and under herdr, through it).
	child := convoProc{pid: 400, start: 60, sid: sidB, herdrPane: "w1:p1", herdrSocket: sock, anc: []convoAnc{{390, "bash"}, {100, "claude"}, {90, "zsh"}, {80, "herdr"}}}
	for _, other := range []convoProc{inTmux, child} {
		for _, want := range []string{"", "00000000-0000-0000-0000-000000000000"} {
			if p := pickConvoProc([]convoProc{agent, other}, "herdr", "w1:p1", sock, "", "", want); p == nil || p.pid != 100 {
				t.Errorf("herdr, with %d, herdr naming %q: %+v", other.pid, want, p)
			}
		}
	}
	// tmux: the one whose input is the pane's terminal.
	tAgent := convoProc{pid: 100, start: 10, sid: sidA, tmuxPane: "%3", tmuxServerPID: "42", tty: "/dev/pts/3"}
	tOther := convoProc{pid: 300, start: 50, sid: sidB, tmuxPane: "%3", tmuxServerPID: "42", tty: "/dev/pts/8"}
	if p := pickConvoProc([]convoProc{tAgent, tOther}, "tmux", "%3", "", "42", "/dev/pts/3", ""); p == nil || p.pid != 100 {
		t.Errorf("tmux: %+v", p)
	}
	// Only one there, and not the pane's own (the agent gone, what it ran
	// left): none, herdr or tmux.
	if p := pickConvoProc([]convoProc{inTmux}, "herdr", "w1:p1", sock, "", "", "00000000-0000-0000-0000-000000000000"); p != nil {
		t.Errorf("herdr, only one not under herdr: %+v", p)
	}
	if p := pickConvoProc([]convoProc{tOther}, "tmux", "%3", "", "42", "/dev/pts/3", ""); p != nil {
		t.Errorf("tmux, only one on another terminal: %+v", p)
	}
	// (Its terminal not known: the one started last, as before.)
	if p := pickConvoProc([]convoProc{tAgent, tOther}, "tmux", "%3", "", "42", "", ""); p == nil || p.pid != 300 {
		t.Errorf("tmux, no terminal known: %+v", p)
	}
}

func TestConvoLocateCommandQuotesArguments(t *testing.T) {
	cmd := convoLocateCommand("tmux", "a'b; rm -rf ~", "")
	if !strings.HasSuffix(cmd, ` corral 'tmux' 'a'\''b; rm -rf ~' ''`) {
		t.Fatalf("arguments not quoted: %s", cmd[len(cmd)-60:])
	}
}

func TestParseConvoLocateFirstValueCounts(t *testing.T) {
	// A later line (a value that held a newline) cannot replace a field.
	out := convoRec("pid=100", "start=500", "procstart=1", "sid="+sidA, "log=", "start=1", "HERDR_PANE_ID=w1:p1", "HERDR_PANE_ID=w9:p9")
	if procs := parseConvoLocate(out).procs; len(procs) != 0 {
		t.Fatalf("start time replaced: %+v", procs)
	}
	out = convoRec("pid=100", "start=500", "procstart=500", "sid="+sidA, "log=", "HERDR_PANE_ID=w1:p1", "HERDR_PANE_ID=w9:p9")
	if procs := parseConvoLocate(out).procs; len(procs) != 1 || procs[0].herdrPane != "w1:p1" {
		t.Fatalf("pane replaced: %+v", procs)
	}
}

func TestConvoGenerationsAreNotReused(t *testing.T) {
	var c convoStates
	a := c.get("s1").gen
	c.m = nil // (as if every state had been forgotten)
	if b := c.get("s1").gen; b == a {
		t.Fatalf("generation %d given again", b)
	}
}

func TestParseConvoLocateTmuxTitle(t *testing.T) {
	title := parseConvoLocate("tmux=%1 9 /dev/pts/2 \u2733 fix\u0007 the tests\x00").tmuxTitle
	if title != "\u2733 fix the tests" {
		t.Fatalf("title %q", title)
	}
}

func TestConvoStatesAreBounded(t *testing.T) {
	var c convoStates
	held := c.get("held") // a request under way keeps its state
	for i := 0; i < convoStatesMax+20; i++ {
		c.get("s\x00w1:p" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('0'+i/26))).done()
	}
	if len(c.m) > convoStatesMax {
		t.Fatalf("%d states kept", len(c.m))
	}
	if c.m["held"] != held {
		t.Fatal("a state in use was let go")
	}
	held.done()
}

func TestConvoKeysCommandQuotes(t *testing.T) {
	cmd := convoSendCommand("tmux", "a'b", "%3", "", 7, 9, sidA, 0, convoKeys["send_now"].tmux, 0)
	if !strings.HasSuffix(cmd, ` corral 'tmux' 'a'\''b' '%3' 7 9 keys '' '`+sidA+`' 0 'C-x' 'C-s'`) {
		t.Fatalf("arguments not quoted: %s", cmd[len(cmd)-90:])
	}
}

func TestConvoStatesComeBackUnderTheLimit(t *testing.T) {
	var c convoStates
	var held []*convoState
	for i := 0; i < convoStatesMax+10; i++ { // all in use: past the limit for a moment
		held = append(held, c.get(fmt.Sprint("k", i)))
	}
	for _, st := range held {
		st.done()
	}
	c.get("k0").done() // a later request, of one that is kept
	if len(c.m) > convoStatesMax {
		t.Fatalf("%d states kept", len(c.m))
	}
	if _, ok := c.m["k0"]; !ok {
		t.Fatal("the state asked for was let go")
	}
}

func TestParseConvoLocateStarted(t *testing.T) {
	procs := parseConvoLocate(convoRec("pid=100", "start=500", "btime=1792832000", "hz=100", "fg=1", "procstart=500", "sid="+sidA, "log=")).procs
	if len(procs) != 1 || procs[0].started != 1792832005000 {
		t.Fatalf("started: %+v", procs)
	}
}

// The mode as Claude Code's last lines say it (what Shift+Tab went on to).
func TestScreenMode(t *testing.T) {
	cases := map[string]string{
		"❯ \n  Opus 5.5·H │ Ctx 48.0K\n  ⏸ manual mode on\n":                         "default",
		"  ⏵⏵ accept edits on (shift+tab to cycle)\n":                                "acceptEdits",
		"  ⏸ plan mode on (shift+tab to cycle)":                                      "plan",
		"  ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents\n":                    "auto",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n":                          "bypassPermissions",
		"❯ \n  ? for shortcuts\n":                                                    "",
		"● I said plan mode on earlier\n  ⏵⏵ accept edits on (shift+tab to cycle)\n": "acceptEdits",
		"● plan mode on is what I would use\n❯ \n":                                   "",
	}
	for in, want := range cases {
		if got := screenMode(in); got != want {
			t.Errorf("screenMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Codex process: its log the session file it has open; a path not of
// Codex's sessions, or not of that session, is not read.
func TestParseConvoLocateCodex(t *testing.T) {
	sid := "01a17600-0000-7000-8000-000000000004"
	good := "/home/u/.codex/sessions/2026/10/26/rollout-2026-10-26T10-00-00-" + sid + ".jsonl"
	procs := parseConvoLocate(convoRec("pid=7", "start=70", "procstart=70", "sid="+sid, "log="+good, "agent=codex", "HERDR_PANE_ID=w1:p1") +
		convoRec("pid=8", "start=80", "procstart=80", "sid="+sid, "log=/home/u/.codex/sessions/x/rollout-other.jsonl", "agent=codex") +
		convoRec("pid=9", "start=90", "procstart=90", "sid="+sid, "log=/home/u/.claude/projects/-w/"+sid+".jsonl", "agent=codex") +
		convoRec("pid=10", "start=100", "procstart=100", "sid="+sidA, "log=/home/u/.claude/projects/-w/"+sidA+".jsonl", "agent=other")).procs
	if len(procs) != 4 || procs[0].agent != "codex" || procs[0].log != good {
		t.Fatalf("codex: %+v", procs)
	}
	if procs[1].log != "" || procs[2].log != "" {
		t.Errorf("foreign logs kept: %q %q", procs[1].log, procs[2].log)
	}
	if procs[3].agent != "claude" || procs[3].log == "" {
		t.Errorf("an unknown agent is Claude Code's: %+v", procs[3])
	}
}

// A Codex not yet begun on its log (a new conversation) is there, its
// session the zero one; a Claude Code never is with that.
func TestParseConvoLocateCodexNoLog(t *testing.T) {
	procs := parseConvoLocate(convoRec("pid=7", "start=70", "procstart=70", "sid="+convoNoSession, "log=", "agent=codex", "HERDR_PANE_ID=w1:p1") +
		convoRec("pid=8", "start=80", "procstart=80", "sid="+convoNoSession, "log=", "agent=claude", "HERDR_PANE_ID=w1:p2")).procs
	if len(procs) != 1 || procs[0].pid != 7 || procs[0].agent != "codex" || procs[0].log != "" {
		t.Fatalf("procs: %+v", procs)
	}
}

// A Codex whose log its app-server daemon keeps is given the conversation
// begun where it runs as it started and written since; not one a Codex that
// has ended left behind, nor one begun before it, and not when another Codex
// runs in the same place.
func TestDaemonLogPicksTheCodexOwn(t *testing.T) {
	const sec = 1000
	proc := func(pid int64, cwd string, startedAgo int64) convoProc {
		return convoProc{pid: pid, start: 70, agent: "codex", cwd: cwd, started: 1_700_000_000_000 - startedAgo*sec, herdrPane: "w1:p1"}
	}
	cand := func(sid, cwd string, beganAgo, wroteAgo int64) convoCand {
		return convoCand{log: "/home/u/.codex/sessions/2026/10/26/rollout-2026-10-26T10-00-00-" + sid + ".jsonl", cwd: cwd, began: 1_700_000_000_000 - beganAgo*sec, wrote: 1_700_000_000_000 - wroteAgo*sec}
	}
	mine, old, other := "01a17600-0000-7000-8000-000000000001", "01a17600-0000-7000-8000-000000000002", "01a17600-0000-7000-8000-000000000003"
	srv := &Server{}
	for _, c := range []struct {
		name  string
		procs []convoProc
		cands []convoCand
		which int // (which of the processes is asked after)
		want  string
	}{
		{"its own", []convoProc{proc(7, "/w", 60)}, []convoCand{cand(mine, "/w", 59, 1)}, 0, mine},
		{"left by one that ended", []convoProc{proc(7, "/w", 60)}, []convoCand{cand(old, "/w", 65, 61)}, 0, ""},
		{"begun before it started", []convoProc{proc(7, "/w", 60)}, []convoCand{cand(old, "/w", 600, 1)}, 0, ""},
		{"another place", []convoProc{proc(7, "/w", 60)}, []convoCand{cand(other, "/elsewhere", 59, 1)}, 0, ""},
		// Two in one place: one begun while only the first was there is
		// the first's; one begun once both were could be either's, so it
		// is shown for neither.
		{"two in one place: begun before the second started", []convoProc{proc(7, "/w", 60), proc(8, "/w", 50)}, []convoCand{cand(mine, "/w", 59, 2)}, 0, mine},
		{"two in one place: begun once both were there", []convoProc{proc(7, "/w", 60), proc(8, "/w", 50)}, []convoCand{cand(other, "/w", 40, 1)}, 0, ""},
		{"two in one place: the later one claims neither", []convoProc{proc(7, "/w", 60), proc(8, "/w", 50)}, []convoCand{cand(mine, "/w", 59, 2)}, 1, ""},
		{"one begun before any of them started", []convoProc{proc(7, "/w", 60), proc(8, "/w", 50)}, []convoCand{cand(old, "/w", 70, 1)}, 0, ""},
		{"the one written last (a new conversation in it)", []convoProc{proc(7, "/w", 60)}, []convoCand{cand(old, "/w", 59, 30), cand(mine, "/w", 40, 1)}, 0, mine},
	} {
		p := &c.procs[c.which]
		srv.daemonLog(p, convoLocated{procs: c.procs, cands: c.cands})
		if p.sid != c.want || (c.want == "" && p.log != "") {
			t.Errorf("%s: sid %q log %q, want %q", c.name, p.sid, p.log, c.want)
		}
	}
}

// A conversation found to be one Codex's is not taken for another's once
// that Codex has ended (its daemon keeps the log a while longer).
func TestDaemonLogKeepsWhoseItWas(t *testing.T) {
	srv := &Server{}
	const sec = 1000
	now := int64(1_700_000_000_000)
	sid := "01a17600-0000-7000-8000-00000000000b"
	log := "/home/u/.codex/sessions/2026/10/26/rollout-2026-10-26T10-00-00-" + sid + ".jsonl"
	// Two Codexes in one place: a conversation begun once both were there
	// could be either's, so neither shows it.
	first := convoProc{pid: 7, start: 70, agent: "codex", cwd: "/w", started: now - 600*sec}
	second := convoProc{pid: 8, start: 80, agent: "codex", cwd: "/w", started: now - 300*sec}
	c := convoCand{log: log, cwd: "/w", began: now - 299*sec, wrote: now - 10*sec}
	both := convoLocated{procs: []convoProc{first, second}, cands: []convoCand{c}}
	for _, p := range []*convoProc{&both.procs[0], &both.procs[1]} {
		srv.daemonLog(p, both)
		if p.sid != "" {
			t.Fatalf("taken while it could be either's: %d has %q", p.pid, p.sid)
		}
	}
	// The second ends; the one left does not take it either.
	q := &convoProc{pid: 7, start: 70, agent: "codex", cwd: "/w", started: now - 600*sec}
	srv.daemonLog(q, convoLocated{procs: []convoProc{*q}, cands: []convoCand{c}})
	if q.sid != "" || q.log != "" {
		t.Errorf("taken once the other had ended: sid %q log %q", q.sid, q.log)
	}
}

func TestUnescapeJSON(t *testing.T) {
	for in, want := range map[string]string{`/tmp/a`: `/tmp/a`, `/tmp/a\"b`: `/tmp/a"b`, `/tmp/a\\b`: `/tmp/a\b`, `/tmp/a\nb`: "/tmp/a\nb", `/tmp/a\q`: `/tmp/a\q`} {
		if got := unescapeJSON(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Full: what was said of which conversation is whose is no longer whole, so
// none is taken any more (rather than one contested being opened up again).
func TestConvoClaimsFull(t *testing.T) {
	var c convoClaims
	none := map[int64]bool{}
	c.contest("contested")
	for i := range maxConvoClaims + 10 {
		c.take(fmt.Sprintf("s%d", i), int64(i+1), 0, nil)
	}
	later := time.Now().UnixMilli() + 60_000
	if c.take("contested", 7, later, none) || c.take("s1", 2, later, none) || c.take("new-one", 3, later, none) {
		t.Error("taken although what is whose is no longer whole")
	}
}

// A conversation of a Codex that is gone may be taken by one started since
// (resumed in it); not by one that was already running beside it.
func TestConvoClaimsHandsOnWhenGone(t *testing.T) {
	var c convoClaims
	now := time.Now().UnixMilli()
	if !c.take("s", 7, now, map[int64]bool{7: true}) {
		t.Error("not taken by the one it is of")
	}
	if c.take("s", 9, now-60_000, map[int64]bool{}) {
		t.Error("taken by one that ran beside it")
	}
	if c.take("s", 9, now+60_000, map[int64]bool{7: true}) {
		t.Error("taken although the Codex it is of is still there")
	}
	if !c.take("s", 9, now+60_000, map[int64]bool{}) {
		t.Error("not taken by one started since it was gone")
	}
}

// What a screen holds of a text: no colours, and none of the breaks and
// spaces wrapping puts in it.
func TestFlattenAndMiddle(t *testing.T) {
	if got := flatten("\x1b[1m你好\x1b[0m，世\n界 — ok\r\n"); got != "你好，世界—ok" {
		t.Errorf("flatten: %q", got)
	}
	if flatten("中文连续文本") != flatten("中文连续\n文本") {
		t.Error("a line wrapped in the middle of the words")
	}
	if got := middle("0123456789", 4); got != "3456" {
		t.Errorf("middle: %q", got)
	}
	if got := middle("abc", 4); got != "abc" {
		t.Errorf("middle, short: %q", got)
	}
}

// There is no room for another: nothing is taken (rather than taken and
// not kept, to be taken by another next time).
func TestConvoClaimsNoRoom(t *testing.T) {
	var c convoClaims
	for i := range maxConvoClaims {
		if !c.take(fmt.Sprintf("s%d", i), 1, 0, nil) {
			t.Fatalf("not taken at %d", i)
		}
	}
	if c.take("one-more", 2, 0, nil) {
		t.Error("taken although there was no room for it")
	}
}

// A Codex whose log its app-server daemon keeps is still told when it was
// last at work: by the conversations the daemon holds that were begun
// where it runs and after it started. One left there by a Codex that has
// ended says nothing of this one, another place's does not count, and no
// other agent is told this way.
func TestProcWroteFromDaemonLogs(t *testing.T) {
	const started = 1_700_000_000_000
	cands := []convoCand{
		{log: "/home/u/.codex/sessions/a.jsonl", cwd: "/w", began: started + 10_000, wrote: started + 100_000},
		{log: "/home/u/.codex/sessions/b.jsonl", cwd: "/w", began: started + 20_000, wrote: started + 900_000},
		{log: "/home/u/.codex/sessions/old.jsonl", cwd: "/w", began: started - 600_000, wrote: started + 5_000_000},
		{log: "/home/u/.codex/sessions/c.jsonl", cwd: "/elsewhere", began: started + 10_000, wrote: started + 9_000_000},
	}
	codex := func(cwd string) convoProc { return convoProc{agent: "codex", cwd: cwd, started: started} }
	own := codex("/w")
	own.wrote = started + 500_000
	none := codex("/w")
	none.started = 0
	for _, c := range []struct {
		name string
		proc convoProc
		want int64
	}{
		{"its own log's time stands", own, started + 500_000},
		{"none of its own: the last written since it started", codex("/w"), started + 900_000},
		{"none begun where it runs", codex("/nowhere"), 0},
		{"where it runs is not known", codex(""), 0},
		{"when it started is not known", none, 0},
		{"Claude Code keeps its own log", convoProc{agent: "claude", cwd: "/w", started: started}, 0},
	} {
		if got := procWrote(c.proc, cands); got != c.want {
			t.Errorf("%s: wrote %d, want %d", c.name, got, c.want)
		}
	}
	// One begun before it started was left by a Codex that has ended: it
	// is no word on this one, however lately it was written.
	late := codex("/w")
	if got := procWrote(late, cands[2:3]); got != 0 {
		t.Errorf("a log left by one that ended: wrote %d, want 0", got)
	}
}

// The next permission mode is Shift+Tab, and on a herdr host it goes as
// the bytes themselves: herdr before 0.9.5 sends a plain Tab for that key
// name, which Claude Code does nothing with (0.8.0 seen doing it). On
// tmux the name stands, which tmux spells out itself.
func TestModeKeyGoesAsItsOwnBytesOnHerdr(t *testing.T) {
	herdr := convoSendCommand("herdr", "", "w1:p1", "/run/x.sock", 7, 9, sidA, 0, convoKeys["mode"].herdr, 8)
	if !strings.Contains(herdr, `herdr pane send-text "$p" "$e[Z"`) {
		t.Error("herdr: Shift+Tab does not go as its own bytes")
	}
	if !strings.Contains(herdr, `[ "$k" = shift+tab ]`) {
		t.Error("herdr: the key it is sent for is not named")
	}
	// (Every other key still goes by name, Escape and "send now" among them.)
	if !strings.Contains(herdr, `herdr pane send-keys "$p" "$k"`) {
		t.Error("herdr: other keys no longer go by name")
	}
	if tmux := convoSendCommand("tmux", "s", "%3", "", 7, 9, sidA, 0, convoKeys["mode"].tmux, 8); !strings.HasSuffix(tmux, " 'BTab'") {
		t.Errorf("tmux: %s", tmux[len(tmux)-40:])
	}
}

// The mode a screen shows, for a switch to one picked: its mode line's;
// an older Claude Code's default, which has none, by its input box; else
// not known.
func TestModeOnScreen(t *testing.T) {
	cases := map[string]string{
		"──────────\n❯ \n──────────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n": "auto",
		"──────────\n❯ \n──────────\n  ⏸ manual mode on\n":                     "default",
		"──────────\n❯ \n──────────\n  ? for shortcuts\n":                      "default",
		"Do you want to proceed?\n❯ 1. Yes\n  2. No\n":                         "",
		// (An input box and a mode quoted in the thread, a question in front now: not told.)
		"──────────\n❯ \n──────────\n  ⏵⏵ auto mode on\n" + strings.Repeat("│ line of the question\n", 12) + "Do you want to proceed?\n❯ 1. Yes\n  2. No\n": "",
		"": "",
	}
	for in, want := range cases {
		if got := modeOnScreen(in); got != want {
			t.Errorf("modeOnScreen(%q) = %q, want %q", in, got, want)
		}
	}
}
