package web

import (
	"fmt"
	"strings"
	"testing"
)

const sidA = "194b7874-ce3d-46b3-b901-f53bd5747ce0"
const sidB = "607ffb6d-81da-4b8d-9d09-cad35b7a2ab4"

func convoRec(fields ...string) string { return strings.Join(fields, "\n") + "\x00" }

func TestParseConvoLocate(t *testing.T) {
	out := "tmux=%3 4242\x00" +
		convoRec("pid=100", "start=500", "fg=1", "procstart=500", "sid="+sidA, "log=/home/u/.claude/projects/-w/"+sidA+".jsonl",
			"HERDR_PANE_ID=w7:p1", "HERDR_SOCKET_PATH=/home/u/.config/herdr/herdr.sock") +
		// The pid was reused: the session file is a dead process's.
		convoRec("pid=101", "start=900", "procstart=600", "sid="+sidB, "log=") +
		convoRec("pid=102", "start=700", "procstart=700", "sid="+sidB, "log=", "TMUX=/tmp/tmux-1000/default,4242,0", "TMUX_PANE=%3") +
		// A log path that is not this session's is not read.
		convoRec("pid=103", "start=800", "procstart=800", "sid="+sidB, "log=/etc/passwd") +
		convoRec("pid=104", "start=800", "procstart=800", "sid=../../x", "log=") +
		convoRec("garbage")
	procs, active, serverPID, _, noProc := parseConvoLocate(out)
	if active != "%3" || serverPID != "4242" || noProc {
		t.Fatalf("tmux %q %q noproc %v", active, serverPID, noProc)
	}
	if len(procs) != 3 || !procs[0].fg || procs[1].fg {
		t.Fatalf("procs: %+v", procs)
	}
	if p := procs[0]; p.pid != 100 || p.sid != sidA || p.herdrPane != "w7:p1" || !strings.HasSuffix(p.log, sidA+".jsonl") {
		t.Errorf("herdr proc: %+v", p)
	}
	if p := procs[1]; p.pid != 102 || p.tmuxPane != "%3" || p.tmuxServerPID != "4242" || p.tmuxSocket != "/tmp/tmux-1000/default" {
		t.Errorf("tmux proc: %+v", p)
	}
	if p := procs[2]; p.pid != 103 || p.log != "" {
		t.Errorf("foreign log kept: %+v", p)
	}
	if _, _, _, _, noProc := parseConvoLocate("noproc\x00"); !noProc {
		t.Error("noproc not seen")
	}
	if _, active, _, _, _ := parseConvoLocate("tmux=%3; rm -rf / 1\x00"); active != "" {
		t.Errorf("bad tmux answer accepted: %q", active)
	}
}

func TestPickConvoProc(t *testing.T) {
	sock := "/home/u/.config/herdr/herdr.sock"
	procs := []convoProc{
		{pid: 1, start: 10, sid: sidA, herdrPane: "w1:p1", herdrSocket: sock},
		{pid: 2, start: 30, sid: sidB, herdrPane: "w1:p1", herdrSocket: sock}, // started later in the same pane
		{pid: 3, start: 20, sid: sidA, herdrPane: "w2:p1", herdrSocket: "/other/herdr.sock"},
		{pid: 4, start: 40, sid: sidA, tmuxPane: "%3", tmuxServerPID: "4242"},
		{pid: 5, start: 50, sid: sidB, tmuxPane: "%3", tmuxServerPID: "9999"}, // another tmux server
	}
	if p := pickConvoProc(procs, "herdr", "w1:p1", sock, "", ""); p == nil || p.pid != 2 {
		t.Errorf("herdr: %+v", p)
	}
	if p := pickConvoProc(procs, "herdr", "w2:p1", sock, "", ""); p != nil {
		t.Errorf("another herdr server's pane matched: %+v", p)
	}
	if p := pickConvoProc(procs, "herdr", "", sock, "", ""); p != nil {
		t.Errorf("no pane matched: %+v", p)
	}
	if p := pickConvoProc(procs, "tmux", "", "", "%3", "4242"); p == nil || p.pid != 4 {
		t.Errorf("tmux: %+v", p)
	}
	if p := pickConvoProc(procs, "none", "w1:p1", sock, "%3", "4242"); p != nil {
		t.Errorf("plain shell matched: %+v", p)
	}
}

func TestConvoLocateCommandQuotesArguments(t *testing.T) {
	cmd := convoLocateCommand("tmux", "a'b; rm -rf ~")
	if !strings.HasSuffix(cmd, ` corral 'tmux' 'a'\''b; rm -rf ~'`) {
		t.Fatalf("arguments not quoted: %s", cmd[len(cmd)-60:])
	}
}

func TestParseConvoLocateFirstValueCounts(t *testing.T) {
	// A later line (a value that held a newline) cannot replace a field.
	out := convoRec("pid=100", "start=500", "procstart=1", "sid="+sidA, "log=", "start=1", "HERDR_PANE_ID=w1:p1", "HERDR_PANE_ID=w9:p9")
	if procs, _, _, _, _ := parseConvoLocate(out); len(procs) != 0 {
		t.Fatalf("start time replaced: %+v", procs)
	}
	out = convoRec("pid=100", "start=500", "procstart=500", "sid="+sidA, "log=", "HERDR_PANE_ID=w1:p1", "HERDR_PANE_ID=w9:p9")
	if procs, _, _, _, _ := parseConvoLocate(out); len(procs) != 1 || procs[0].herdrPane != "w1:p1" {
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
	_, _, _, title, _ := parseConvoLocate("tmux=%1 9 \u2733 fix\u0007 the tests\x00")
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

func TestTmuxKeysCommandQuotes(t *testing.T) {
	cmd := tmuxKeysCommand("a'b", "%3", convoKeys["send_now"].tmux)
	if !strings.HasSuffix(cmd, ` corral 'a'\''b' '%3' 'C-x' 'C-s'`) {
		t.Fatalf("arguments not quoted: %s", cmd[len(cmd)-50:])
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
