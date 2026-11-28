package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

func herdrAnc() []convoAnc {
	return []convoAnc{{pid: 90, comm: "zsh"}, {pid: 80, comm: "herdr"}}
}

func makePeerParser(t *testing.T, lines ...string) *convo.Claude {
	t.Helper()
	p := convo.NewClaude()
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	p.Feed(buf)
	return p
}

func tmuxLocated(pane, serverPID, tty, title string, procs ...convoProc) convoLocated {
	return convoLocated{
		procs:      procs,
		tmuxActive: pane,
		tmuxPID:    serverPID,
		tmuxTTY:    tty,
		tmuxTitle:  title,
	}
}

func TestPeerResolutionLiveExitedRestartAndIsolation(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, err := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	hostA, err := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	hostB, err := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "otherbox", Address: "10.0.0.2", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}

	sock := "/tmp/herdr-test/default.sock"
	sid1 := "11111111-1111-1111-1111-111111111111"
	sid2 := "22222222-2222-2222-2222-222222222222"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "ws1", Label: "corral-work"},
			{ID: "ws2", Label: "~"},
		},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-1", AgentSession: &herdr.AgentSessionInfo{Value: sid1}, Title: "✳ fix cross-session", Cwd: "/srv/corral"},
			{ID: "w2:p1", WorkspaceID: "ws2", Agent: "claude", TerminalID: "term-2", AgentSession: &herdr.AgentSessionInfo{Value: sid2}, Label: "reviewer-pane", Title: "✳ review", Cwd: "/srv/other"},
		},
	}

	cfgA := hostConfig(hostA)
	cfgB := hostConfig(hostB)
	sessA := &term.Session{
		ID:          "sess-a",
		OwnerID:     alice.ID,
		HostID:      hostA.ID,
		HostName:    "devbox",
		HostConfig:  cfgA,
		Persistence: "herdr",
	}

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	srv := &Server{
		store:      st,
		herdrSocks: herdrSockets{path: map[string]string{}},
	}
	srv.herdrSocks.set(sessA.ID, sock)
	srv.herdrSocks.setServer(sessA.ID, "srv-key", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return snap, t0
	}, func() {})

	srv.recordLocatedProcs(sessA, convoLocated{procs: []convoProc{
		{pid: 530048, start: 193322812, agent: "claude", sid: sid1, herdrPane: "w1:p1", herdrSocket: sock, cwd: "/srv/corral", anc: herdrAnc()},
		{pid: 530099, start: 193399999, agent: "claude", sid: sid2, herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other", anc: herdrAnc()},
		{pid: 600001, start: 194000000, agent: "claude", sid: "33333333-3333-3333-3333-333333333333", herdrPane: "w9:p1", herdrSocket: "/tmp/unmanaged.sock", cwd: "/srv/ext", anc: herdrAnc()},
	}}, true, t0)

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","timestamp":"2026-11-28T03:00:00Z","origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"193322812","name":"ai-68"},"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\">\nhi 1\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","timestamp":"2026-11-28T03:00:01Z","origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"999999999","name":"ai-reused"},"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-reused\">\nhi 2\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u3","timestamp":"2026-11-28T03:00:02Z","origin":{"kind":"peer","from":"uds:/tmp/cc-socks/600001.sock","verifiedPeerPid":600001,"verifiedPeerProcStart":"194000000","name":"ai-ext"},"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/600001.sock\" from-name=\"ai-ext\">\nhi 3\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}

	peers := srv.resolveConvoPeers(ctx, sessA, cst)
	if len(peers) != 1 {
		t.Fatalf("got %d resolved peers, want 1: %+v", len(peers), peers)
	}
	p1, ok := peers["530048:193322812"]
	if !ok {
		t.Fatalf("missing 530048:193322812 in %+v", peers)
	}
	if !p1.Alive || p1.SessionID != sessA.ID || p1.Pane != "w1:p1" || p1.Title != "corral-work" || p1.HostName != "devbox" || p1.Key != herdrAgentKey(hostA.ID, "", "w1:p1") || p1.AgentSession != sid1 || p1.TerminalID != "term-1" {
		t.Fatalf("resolved p1 = %+v", p1)
	}

	// Host isolation: a session on hostB must not resolve hostA's peer PID.
	sessB := &term.Session{ID: "sess-b", OwnerID: alice.ID, HostID: hostB.ID, HostName: "otherbox", HostConfig: cfgB, Persistence: "herdr"}
	if otherPeers := srv.resolveConvoPeers(ctx, sessB, cst); len(otherPeers) != 0 {
		t.Fatalf("hostB resolved hostA peer: %+v", otherPeers)
	}

	// Sender exits (full scan no longer includes 530048:193322812).
	srv.recordLocatedProcs(sessA, convoLocated{procs: []convoProc{
		{pid: 530099, start: 193399999, agent: "claude", sid: sid2, herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other", anc: herdrAnc()},
	}}, true, t0.Add(10*time.Second))
	peersAfterExit := srv.resolveConvoPeers(ctx, sessA, cst)
	p1Exited := peersAfterExit["530048:193322812"]
	if p1Exited.Alive || p1Exited.Title != "corral-work" || p1Exited.HostName != "devbox" {
		t.Fatalf("after exit p1 = %+v, want Alive=false with remembered title", p1Exited)
	}

	// Simulated gateway restart (fresh Server, same SQLite store).
	srv2 := &Server{
		store:      st,
		herdrSocks: herdrSockets{path: map[string]string{}},
	}
	peersAfterRestart := srv2.resolveConvoPeers(ctx, sessA, cst)
	p1Restart := peersAfterRestart["530048:193322812"]
	if p1Restart.Alive || p1Restart.Title != "corral-work" || p1Restart.Pane != "w1:p1" || p1Restart.HostName != "devbox" || p1Restart.AgentSession != sid1 || p1Restart.TerminalID != "term-1" {
		t.Fatalf("after restart p1 = %+v, want persisted snapshot with Alive=false", p1Restart)
	}
}

// Regression fix1 #1: a background/child Claude launched by a parent Claude
// inherits HERDR_PANE_ID and HERDR_SOCKET_PATH. Only the primary process chosen
// by pickConvoProc may map to the parent's pane; the child must stay unmatched
// ("not in Corral") regardless of slice ordering, unless it runs in its own pane.
func TestPeerResolutionBackgroundChildNotMisattributedToParent(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})

	sock := "/tmp/herdr-test/default.sock"
	parentSID := "11111111-1111-1111-1111-111111111111"
	childSID := "22222222-2222-2222-2222-222222222222"
	ownPaneSubSID := "33333333-3333-3333-3333-333333333333"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "parent-work"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t-parent", AgentSession: &herdr.AgentSessionInfo{Value: parentSID}, Label: "Parent Agent"},
			{ID: "w1:p2", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t-sub", AgentSession: &herdr.AgentSessionInfo{Value: ownPaneSubSID}, Label: "Sub Pane Agent"},
		},
	}

	sess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: hostConfig(hostA), Persistence: "herdr"}
	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setSids(sess.ID, snap)
	srv.herdrSocks.setServer(sess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	parentProc := convoProc{
		pid: 100, start: 500, agent: "claude", sid: parentSID,
		herdrPane: "w1:p1", herdrSocket: sock,
		anc: []convoAnc{{pid: 90, comm: "zsh"}, {pid: 80, comm: "herdr"}},
	}
	bgChildProc := convoProc{
		pid: 400, start: 900, agent: "claude", sid: childSID,
		herdrPane: "w1:p1", herdrSocket: sock,
		anc: []convoAnc{{pid: 390, comm: "bash"}, {pid: 100, comm: "claude"}, {pid: 90, comm: "zsh"}, {pid: 80, comm: "herdr"}},
	}
	ownPaneSubProc := convoProc{
		pid: 500, start: 950, agent: "claude", sid: ownPaneSubSID,
		herdrPane: "w1:p2", herdrSocket: sock,
		anc: []convoAnc{{pid: 490, comm: "zsh"}, {pid: 80, comm: "herdr"}},
	}

	// Put bgChildProc AFTER parentProc so naive iteration would overwrite parentProc.
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{parentProc, bgChildProc, ownPaneSubProc}}, true, time.Now().Add(-2*time.Hour).Truncate(time.Second))

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":100,"verifiedPeerProcStart":"500","name":"ai-parent"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-parent\">\nfrom parent\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","origin":{"kind":"peer","verifiedPeerPid":400,"verifiedPeerProcStart":"900","name":"ai-bg-child"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-bg-child\">\nfrom bg child\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u3","origin":{"kind":"peer","verifiedPeerPid":500,"verifiedPeerProcStart":"950","name":"ai-sub-pane"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-sub-pane\">\nfrom sub pane\n</cross-session-message>"}}`,
	)

	peers := srv.resolveConvoPeers(ctx, sess, &convoState{parser: parser})
	if p, ok := peers["100:500"]; !ok || !p.Alive || p.Title != "Parent Agent" || p.Pane != "w1:p1" {
		t.Fatalf("parent 100:500 = %+v (ok=%v), want Alive Parent Agent in w1:p1", p, ok)
	}
	if p, ok := peers["400:900"]; ok {
		t.Fatalf("background child 400:900 must NOT be mapped to parent pane, got %+v", p)
	}
	if p, ok := peers["500:950"]; !ok || !p.Alive || p.Title != "Sub Pane Agent" || p.Pane != "w1:p2" {
		t.Fatalf("own-pane subagent 500:950 = %+v (ok=%v), want Alive Sub Pane Agent in w1:p2", p, ok)
	}
}

// Regression fix1 #2 & fix2 #1: replaced pane OR missing AgentSession /
// TerminalID / Agent in current snapshot MUST fail closed (Alive=false with
// remembered old title; never matched on incomplete initial snapshot).
func TestPeerResolutionFailClosedOnReplacedOrIncompleteHerdrSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})

	sock := "/tmp/herdr-test/default.sock"
	oldSID := "11111111-1111-1111-1111-111111111111"
	newSID := "99999999-9999-9999-9999-999999999999"
	curSnap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "old-agent-ws"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-old", AgentSession: &herdr.AgentSessionInfo{Value: oldSID}, Label: "Old Agent"},
			// w1:p2 is missing TerminalID -> must NOT be recorded at all.
			{ID: "w1:p2", WorkspaceID: "ws1", Agent: "claude", TerminalID: "", AgentSession: &herdr.AgentSessionInfo{Value: newSID}, Label: "Incomplete Pane"},
		},
	}

	sess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: hostConfig(hostA), Persistence: "herdr"}
	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setServer(sess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return curSnap, time.Now()
	}, func() {})

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: oldSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		{pid: 200, start: 600, agent: "claude", sid: newSID, herdrPane: "w1:p2", herdrSocket: sock, anc: herdrAnc()},
	}}, true, t0)

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":100,"verifiedPeerProcStart":"500","name":"ai-old"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-old\">\nhi\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","origin":{"kind":"peer","verifiedPeerPid":200,"verifiedPeerProcStart":"600","name":"ai-incomplete"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-incomplete\">\nhi2\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}

	peers := srv.resolveConvoPeers(ctx, sess, cst)
	if !peers["100:500"].Alive || peers["100:500"].Title != "Old Agent" {
		t.Fatalf("complete peer 100:500 = %+v, want Alive Old Agent", peers["100:500"])
	}
	if p, ok := peers["200:600"]; ok {
		t.Fatalf("incomplete snapshot peer 200:600 must NOT be matched, got %+v", p)
	}

	// Each degraded/replaced snapshot variant must fail closed to Alive=false
	// while preserving the verified remembered title "Old Agent".
	badSnaps := []struct {
		name string
		pane herdr.Pane
	}{
		{"replaced_agent_session", herdr.Pane{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-old", AgentSession: &herdr.AgentSessionInfo{Value: newSID}, Label: "New Replacement"}},
		{"replaced_terminal_id", herdr.Pane{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-new", AgentSession: &herdr.AgentSessionInfo{Value: oldSID}, Label: "Recreated Terminal"}},
		{"missing_agent_session", herdr.Pane{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-old", AgentSession: nil, Label: "Missing Session"}},
		{"missing_terminal_id", herdr.Pane{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "", AgentSession: &herdr.AgentSessionInfo{Value: oldSID}, Label: "Missing Terminal"}},
		{"missing_agent_program", herdr.Pane{ID: "w1:p1", WorkspaceID: "ws1", Agent: "", TerminalID: "term-old", AgentSession: &herdr.AgentSessionInfo{Value: oldSID}, Label: "Missing Program"}},
	}
	for _, tc := range badSnaps {
		curSnap = &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "new-ws"}},
			Agents:     []herdr.Pane{tc.pane},
		}
		got := srv.resolveConvoPeers(ctx, sess, cst)["100:500"]
		if got.Alive || got.Title != "Old Agent" || got.SessionID != "" {
			t.Fatalf("%s: got %+v, want Alive=false SessionID=\"\" Title=\"Old Agent\"", tc.name, got)
		}
	}
}

// Regression fix1 #4 & fix2 #2: tmux peer survives host-wide herdr full scan
// while its process is still alive, AND fails closed immediately when:
// (a) a new Corral tmux session is recreated with the SAME tmux session name,
// (b) another process or no process wins pickConvoProc in the tmux session, or
// (c) the tmux process exits /proc.
func TestPeerResolutionTmuxFailClosedAndRecreation(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "box1", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	cfg := hostConfig(hostA)

	sock := "/tmp/herdr-test/default.sock"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "herdr-ws"}},
		Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t1", AgentSession: &herdr.AgentSessionInfo{Value: "11111111-1111-1111-1111-111111111111"}}},
	}
	herdrSess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "box1", HostConfig: cfg, Persistence: "herdr"}
	tmuxSess1 := &term.Session{ID: "s-tmux-1", OwnerID: alice.ID, HostID: hostA.ID, HostName: "box1", HostConfig: cfg, Persistence: "tmux", TmuxSession: "main"}
	tmuxSess2 := &term.Session{ID: "s-tmux-2", OwnerID: alice.ID, HostID: hostA.ID, HostName: "box1", HostConfig: cfg, Persistence: "tmux", TmuxSession: "main"}

	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(herdrSess.ID, sock)
	srv.herdrSocks.setServer(herdrSess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	tmuxProc1 := convoProc{pid: 300, start: 1200, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/3"}

	// Locate tmuxSess1 and attach verified convo title.
	srv.recordLocatedProcs(tmuxSess1, tmuxLocated("%1", "99", "/dev/pts/3", "✳ tmux-task", tmuxProc1), true, t0)
	srv.recordBoundProc(tmuxSess1, &convoBind{PID: 300, Start: 1200, Pane: "%1", Agent: "claude", Session: "22222222-2222-2222-2222-222222222222", Title: "✳ tmux-task"}, "tmux-convo-title", t0)

	// Host-wide full scan triggered from herdrSess still sees tmux proc 300:1200 in /proc.
	srv.recordLocatedProcs(herdrSess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		tmuxProc1,
	}}, true, t0.Add(5*time.Second))

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":300,"verifiedPeerProcStart":"1200","name":"ai-tm"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-tm\">\ntm\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","origin":{"kind":"peer","verifiedPeerPid":400,"verifiedPeerProcStart":"1500","name":"ai-tm2"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-tm2\">\ntm2\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}

	// Alive on the exact original tmux session s-tmux-1.
	peers := srv.resolveConvoPeers(ctx, tmuxSess1, cst)
	if !peers["300:1200"].Alive || peers["300:1200"].SessionID != "s-tmux-1" || peers["300:1200"].Title != "tmux-convo-title" {
		t.Fatalf("tmux peer on original session = %+v, want Alive on s-tmux-1", peers["300:1200"])
	}

	// Same-name tmux session recreated as s-tmux-2 (even before any locate on s-tmux-2):
	// MUST NOT treat old 300:1200 as alive under s-tmux-2!
	peersRecreated := srv.resolveConvoPeers(ctx, tmuxSess2, cst)
	if peersRecreated["300:1200"].Alive || peersRecreated["300:1200"].SessionID != "" || peersRecreated["300:1200"].Title != "tmux-convo-title" {
		t.Fatalf("recreated same-name tmux session wrongly matched old peer: %+v", peersRecreated["300:1200"])
	}

	// Inside s-tmux-1, active pane switches to another agent 400:1500 while 300:1200
	// is still running in another pane -> 300:1200 must become Alive=false!
	tmuxProc2 := convoProc{pid: 400, start: 1500, agent: "claude", sid: "33333333-3333-3333-3333-333333333333", tmuxPane: "%2", tmuxServerPID: "99", tty: "/dev/pts/4"}
	srv.recordLocatedProcs(tmuxSess1, tmuxLocated("%2", "99", "/dev/pts/4", "✳ second-task", tmuxProc1, tmuxProc2), true, t0.Add(10*time.Second))
	peersSwitched := srv.resolveConvoPeers(ctx, tmuxSess1, cst)
	if peersSwitched["300:1200"].Alive {
		t.Fatalf("replaced tmux pane process 300:1200 must NOT stay alive: %+v", peersSwitched["300:1200"])
	}
	if !peersSwitched["400:1500"].Alive || peersSwitched["400:1500"].SessionID != "s-tmux-1" {
		t.Fatalf("new active tmux process 400:1500 = %+v, want Alive", peersSwitched["400:1500"])
	}

	// Once tmux session active pane has NO agent picked, 400:1500 also becomes Alive=false.
	srv.recordLocatedProcs(tmuxSess1, tmuxLocated("%2", "99", "/dev/pts/4", "bash"), true, t0.Add(20*time.Second))
	peersEmpty := srv.resolveConvoPeers(ctx, tmuxSess1, cst)
	if peersEmpty["400:1500"].Alive {
		t.Fatalf("tmux peer must become Alive=false when no agent is picked: %+v", peersEmpty["400:1500"])
	}
}

// Regression fix2 #5: recordBoundProc must ONLY write/update when the binding's
// (pid, start) matches the current verified pane occupant AND live pane
// identity. A stale binding must never contaminate the old process's remembered
// title or bleed its title onto a replacement process.
func TestPeerRecordBoundProcFailClosedAgainstStaleBinding(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	cfg := hostConfig(hostA)
	sock := "/tmp/herdr-test/default.sock"
	oldSID := "11111111-1111-1111-1111-111111111111"
	newSID := "22222222-2222-2222-2222-222222222222"

	curSnap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "~"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-old", AgentSession: &herdr.AgentSessionInfo{Value: oldSID}, Title: "✳ old-work"},
		},
	}
	sess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: cfg, Persistence: "herdr"}
	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setServer(sess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return curSnap, time.Now()
	}, func() {})

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	// Unverified binding BEFORE any pickConvoProc locate scan -> must NOT create a record.
	oldBind := &convoBind{PID: 100, Start: 500, Pane: "w1:p1", TerminalID: "term-old", Agent: "claude", Session: oldSID}
	srv.recordBoundProc(sess, oldBind, "Unverified Title", t0)
	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":100,"verifiedPeerProcStart":"500","name":"ai-old"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-old\">\nold\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","origin":{"kind":"peer","verifiedPeerPid":200,"verifiedPeerProcStart":"600","name":"ai-new"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-new\">\nnew\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}
	if peers := srv.resolveConvoPeers(ctx, sess, cst); len(peers) != 0 {
		t.Fatalf("recordBoundProc before pickConvoProc must not create peer record: %+v", peers)
	}

	// Verify 100:500 via locate scan, then update its verified convo title.
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: oldSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true, t0)
	srv.recordBoundProc(sess, oldBind, "Verified Old Title", t0)
	if p := srv.resolveConvoPeers(ctx, sess, cst)["100:500"]; !p.Alive || p.Title != "Verified Old Title" {
		t.Fatalf("verified old peer = %+v", p)
	}

	// Pane switches to New Agent in snapshot BEFORE next locate scan.
	curSnap = &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "~"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-new", AgentSession: &herdr.AgentSessionInfo{Value: newSID}, Title: "✳ new-work"},
		},
	}
	// Stale poll calls recordBoundProc with oldBind -> MUST NOT overwrite 100:500's title or resurrect it!
	srv.recordBoundProc(sess, oldBind, "Contaminated New Title", t0.Add(5*time.Second))
	if p := srv.resolveConvoPeers(ctx, sess, cst)["100:500"]; p.Alive || p.Title != "Verified Old Title" {
		t.Fatalf("stale recordBoundProc contaminated old peer: %+v", p)
	}

	// Locate scan now picks New Agent 200:600 in w1:p1 -> must NOT inherit 100:500's convoTitle!
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 200, start: 600, agent: "claude", sid: newSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true, t0.Add(10*time.Second))
	// Another stale recordBoundProc for 100:500 must be completely ignored.
	srv.recordBoundProc(sess, oldBind, "Late Stale Title", t0.Add(15*time.Second))
	peers := srv.resolveConvoPeers(ctx, sess, cst)
	if pOld := peers["100:500"]; pOld.Alive || pOld.Title != "Verified Old Title" {
		t.Fatalf("old peer after replacement = %+v, want Alive=false Title=\"Verified Old Title\"", pOld)
	}
	if pNew := peers["200:600"]; !pNew.Alive || pNew.Title != "new-work" {
		t.Fatalf("new peer after replacement = %+v, want Alive=true Title=\"new-work\" (no title bleed)", pNew)
	}
}

// Regression fix2 #7: late-arriving older scans must NEVER overwrite newer
// observations, resurrect exited processes, or flip latestByKey back to an
// earlier occupant (in memory or in SQLite).
func TestPeerLateOlderScanDoesNotOverwriteNewerObservation(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	cfg := hostConfig(hostA)
	sock := "/tmp/herdr-test/default.sock"
	oldSID := "11111111-1111-1111-1111-111111111111"
	newSID := "22222222-2222-2222-2222-222222222222"

	curSnap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "~"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-old", AgentSession: &herdr.AgentSessionInfo{Value: oldSID}, Label: "Old Agent"},
		},
	}
	sess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: cfg, Persistence: "herdr"}
	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setServer(sess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return curSnap, time.Now()
	}, func() {})

	t1 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	t2 := t1.Add(10 * time.Second)
	t3 := t1.Add(20 * time.Second)

	// Scan at t1 sees Old Agent 100:500.
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: oldSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true, t1)

	// Pane switches to New Agent 200:600; scan at t3 sees New Agent 200:600 (and 100:500 gone).
	curSnap = &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "~"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-new", AgentSession: &herdr.AgentSessionInfo{Value: newSID}, Label: "New Agent"},
		},
	}
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 200, start: 600, agent: "claude", sid: newSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true, t3)

	// Late-arriving older scan from t2 (before t3!) still had Old Agent 100:500 and NOT 200:600.
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: oldSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true, t2)

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":100,"verifiedPeerProcStart":"500","name":"ai-old"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-old\">\nold\n</cross-session-message>"}}`,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","origin":{"kind":"peer","verifiedPeerPid":200,"verifiedPeerProcStart":"600","name":"ai-new"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-new\">\nnew\n</cross-session-message>"}}`,
	)
	peers := srv.resolveConvoPeers(ctx, sess, &convoState{parser: parser})
	if peers["100:500"].Alive {
		t.Fatalf("late older scan resurrected exited peer 100:500: %+v", peers["100:500"])
	}
	if !peers["200:600"].Alive || peers["200:600"].Title != "New Agent" {
		t.Fatalf("late older scan clobbered newer peer 200:600: %+v", peers["200:600"])
	}
}

// Regression fix2 #8: retargeting a host record to a different machine/port/user
// (different HostConfig under the same OwnerID+HostID) must isolate peer reads
// and exit scans in both memory and SQLite.
func TestPeerScopeIsolationAcrossHostConfigRetarget(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	cfgOld := "1|10.0.0.1|22|alice|password|0"
	cfgNew := "2|10.0.0.9|2222|bob|password|0"

	sessOld := &term.Session{ID: "s-old", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: cfgOld, Persistence: "tmux", TmuxSession: "main"}
	sessNew := &term.Session{ID: "s-new", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: cfgNew, Persistence: "tmux", TmuxSession: "main"}

	srv := &Server{store: st}
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	procOld := convoProc{pid: 500, start: 1000, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "77", tty: "/dev/pts/1"}
	srv.recordLocatedProcs(sessOld, tmuxLocated("%1", "77", "/dev/pts/1", "✳ old-machine", procOld), true, t0)

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":500,"verifiedPeerProcStart":"1000","name":"ai-old"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-old\">\nhi\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}

	// sessNew (retargeted machine) must NOT resolve sessOld's peer (memory or SQLite).
	if got := srv.resolveConvoPeers(ctx, sessNew, cst); len(got) != 0 {
		t.Fatalf("retargeted host config read old machine's peer: %+v", got)
	}

	// Full scan on sessNew (where 500:1000 does not exist) must NOT mark sessOld's peer exited.
	srv.recordLocatedProcs(sessNew, tmuxLocated("%1", "88", "/dev/pts/2", "✳ new-machine"), true, t0.Add(5*time.Second))
	if got := srv.resolveConvoPeers(ctx, sessOld, cst)["500:1000"]; !got.Alive || got.SessionID != "s-old" {
		t.Fatalf("full scan on new host config wrongly marked old host config peer exited: %+v", got)
	}

	// Fresh server restart: sessNew still cannot read sessOld's persisted SQLite row; sessOld can.
	srv2 := &Server{store: st}
	if got := srv2.resolveConvoPeers(ctx, sessNew, cst); len(got) != 0 {
		t.Fatalf("after restart, retargeted host config read old machine's DB row: %+v", got)
	}
	if got := srv2.resolveConvoPeers(ctx, sessOld, cst)["500:1000"]; got.Title != "old-machine" || got.Alive {
		t.Fatalf("after restart, old host config did not read its own persisted row: %+v", got)
	}
}

// Regression fix1 #5: failed SQLite persist must NOT mark cached as persisted
// (next observation retries), and concurrent writes must never let an older
// snapshot overwrite a newer observation in SQLite.
func TestPeerPersistFailureRetriesAndConcurrentOrdering(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	cfg := hostConfig(hostA)
	tmuxSess := &term.Session{ID: "s-tmux", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: cfg, Persistence: "tmux", TmuxSession: "main"}

	// Part A: write failure must retry on the next observation with the SAME metadata.
	srv := &Server{store: st}
	var attempts atomic.Int32
	var failFirst atomic.Bool
	failFirst.Store(true)
	srv.peers.persistFn = func(ctx context.Context, ownerID, hostID int64, hc string, rows []store.PeerIdentity) error {
		attempts.Add(1)
		if failFirst.Load() {
			return errors.New("sqlite busy")
		}
		return st.UpsertPeerIdentities(ctx, ownerID, hostID, hc, rows)
	}

	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	p1 := convoProc{pid: 501, start: 7001, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ same-title", p1), true, t0)
	if attempts.Load() != 1 {
		t.Fatalf("attempts after first write = %d, want 1", attempts.Load())
	}
	if rows, _ := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfg, []store.PeerProcKey{{PID: 501, ProcStart: 7001}}); len(rows) != 0 {
		t.Fatalf("expected 0 DB rows after failed persist, got %+v", rows)
	}
	failFirst.Store(false)
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ same-title", p1), true, t0.Add(time.Second))
	if attempts.Load() != 2 {
		t.Fatalf("attempts after retry = %d, want 2", attempts.Load())
	}
	if rows, _ := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfg, []store.PeerProcKey{{PID: 501, ProcStart: 7001}}); rows[store.PeerProcKey{PID: 501, ProcStart: 7001}].Title != "same-title" {
		t.Fatalf("retry did not persist row: %+v", rows)
	}

	// Part B: concurrent observation ordering — even when an older goroutine is
	// paused before committing and a newer observation arrives, SQLite must end
	// with the newer title ("title-v2"), never overwritten by "title-v1".
	srv2 := &Server{store: st}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstPaused atomic.Bool
	srv2.peers.persistFn = func(ctx context.Context, ownerID, hostID int64, hc string, rows []store.PeerIdentity) error {
		if firstPaused.CompareAndSwap(false, true) {
			entered <- struct{}{}
			<-release
		}
		return st.UpsertPeerIdentities(ctx, ownerID, hostID, hc, rows)
	}

	p2 := convoProc{pid: 502, start: 7002, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", tmuxPane: "%2", tmuxServerPID: "99", tty: "/dev/pts/2"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv2.recordLocatedProcs(tmuxSess, tmuxLocated("%2", "99", "/dev/pts/2", "✳ title-v1", p2), true, t0)
	}()
	<-entered
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv2.recordLocatedProcs(tmuxSess, tmuxLocated("%2", "99", "/dev/pts/2", "✳ title-v2", p2), true, t0.Add(2*time.Second))
	}()
	time.Sleep(25 * time.Millisecond)
	close(release)
	wg.Wait()

	rows, err := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, cfg, []store.PeerProcKey{{PID: 502, ProcStart: 7002}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[store.PeerProcKey{PID: 502, ProcStart: 7002}].Title; got != "title-v2" {
		t.Fatalf("concurrent persist ended with title=%q, want %q", got, "title-v2")
	}
}

// Regression fix1 #6/#9 & fix2 #9: LRU negative cache handles 600 unknown peer
// refs across 3 consecutive polls with ONLY 1 SQLite lookup (no whole-map clear
// thrashing), evicts only the oldest entry when full, and throttles hourly
// last_seen refresh.
func TestPeerNegativeCacheLRUAndHourlyLastSeenThrottle(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, _ := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	hostA, _ := st.CreateHost(ctx, &store.Host{
		OwnerID: alice.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	cfg := hostConfig(hostA)
	tmuxSess := &term.Session{ID: "s-tmux", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", HostConfig: cfg, Persistence: "tmux", TmuxSession: "main"}

	curTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	var lookups, persists atomic.Int32
	srv := &Server{store: st}
	srv.peers.nowFn = func() time.Time { return curTime }
	srv.peers.lookupFn = func(ctx context.Context, ownerID, hostID int64, hc string, keys []store.PeerProcKey) (map[store.PeerProcKey]store.PeerIdentity, error) {
		lookups.Add(1)
		return st.LookupPeerIdentities(ctx, ownerID, hostID, hc, keys)
	}
	srv.peers.persistFn = func(ctx context.Context, ownerID, hostID int64, hc string, rows []store.PeerIdentity) error {
		persists.Add(1)
		return st.UpsertPeerIdentities(ctx, ownerID, hostID, hc, rows)
	}

	// 600 distinct missing peer refs polled 3 consecutive times -> only 1 DB lookup!
	var sb strings.Builder
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&sb, `{"type":"user","isMeta":true,"promptSource":"system","uuid":"u%d","origin":{"kind":"peer","verifiedPeerPid":%d,"verifiedPeerProcStart":"%d","name":"ai-%d"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-%d\">\nhi\n</cross-session-message>"}}`+"\n", i, 10000+i, 50000+i, i, i)
	}
	p600 := convo.NewClaude()
	p600.Feed([]byte(sb.String()))
	cst600 := &convoState{parser: p600}

	srv.resolveConvoPeers(ctx, tmuxSess, cst600)
	srv.resolveConvoPeers(ctx, tmuxSess, cst600)
	srv.resolveConvoPeers(ctx, tmuxSess, cst600)
	if lookups.Load() != 1 {
		t.Fatalf("600 missed peer refs across 3 polls caused %d SQLite lookups, want 1", lookups.Load())
	}

	// After 60s TTL expires, next poll queries SQLite once more.
	curTime = curTime.Add(61 * time.Second)
	srv.resolveConvoPeers(ctx, tmuxSess, cst600)
	if lookups.Load() != 2 {
		t.Fatalf("expected 2nd SQLite lookup after negative cache expired, got %d", lookups.Load())
	}

	// Small-cap LRU eviction test: inserting a 4th miss evicts ONLY the oldest
	// miss (pid 1), keeping pids 2 and 3 cached (no whole-map wipe).
	lru := newPeerNegLRU(3)
	lru.put(peerProcID{pid: 1, start: 10}, curTime)
	lru.put(peerProcID{pid: 2, start: 20}, curTime)
	lru.put(peerProcID{pid: 3, start: 30}, curTime)
	// Touch pid 1 so pid 2 becomes least-recently-used.
	if !lru.hasFresh(peerProcID{pid: 1, start: 10}, curTime, peerNegCacheTTL) {
		t.Fatalf("expected pid 1 fresh")
	}
	lru.put(peerProcID{pid: 4, start: 40}, curTime)
	if lru.hasFresh(peerProcID{pid: 2, start: 20}, curTime, peerNegCacheTTL) {
		t.Fatalf("expected LRU pid 2 evicted")
	}
	for _, want := range []peerProcID{{1, 10}, {3, 30}, {4, 40}} {
		if !lru.hasFresh(want, curTime, peerNegCacheTTL) {
			t.Fatalf("expected %+v still cached after single LRU eviction", want)
		}
	}

	// Observing a previously-missed peer immediately clears its negative cache entry.
	pMiss := convoProc{pid: 10001, start: 50001, agent: "claude", sid: "33333333-3333-3333-3333-333333333333", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ found-now", pMiss), true, curTime)
	if peers := srv.resolveConvoPeers(ctx, tmuxSess, cst600); !peers["10001:50001"].Alive || peers["10001:50001"].Title != "found-now" {
		t.Fatalf("peer should resolve immediately once observed: %+v", peers["10001:50001"])
	}
	if persists.Load() != 1 {
		t.Fatalf("persists after initial observation = %d, want 1", persists.Load())
	}

	// Throttled hourly last_seen refresh: repeated identical observations within
	// 1 hour do not write SQLite; after 1 hour, last_seen refreshes once.
	curTime = curTime.Add(30 * time.Minute)
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ found-now", pMiss), true, curTime)
	if persists.Load() != 1 {
		t.Fatalf("persists within 1h throttle = %d, want 1", persists.Load())
	}
	curTime = curTime.Add(31 * time.Minute)
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ found-now", pMiss), true, curTime)
	if persists.Load() != 2 {
		t.Fatalf("persists after >1h = %d, want 2", persists.Load())
	}
}

func TestPeerSanitization(t *testing.T) {
	if got := cleanPeerPaneTitle("✳ \x1b\x07\u202ehello\x00world"); got != "helloworld" {
		t.Fatalf("cleanPeerPaneTitle = %q, want %q", got, "helloworld")
	}
	if got := cleanPeerPaneTitle("claude --resume foo"); got != "" {
		t.Fatalf("launch cmd should be stripped, got %q", got)
	}
}
