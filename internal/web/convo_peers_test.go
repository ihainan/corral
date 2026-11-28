package web

import (
	"context"
	"errors"
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

	sessA := &term.Session{
		ID:          "sess-a",
		OwnerID:     alice.ID,
		HostID:      hostA.ID,
		HostName:    "devbox",
		Persistence: "herdr",
	}

	srv := &Server{
		store:      st,
		herdrSocks: herdrSockets{path: map[string]string{}},
	}
	srv.herdrSocks.set(sessA.ID, sock)
	srv.herdrSocks.setServer(sessA.ID, "srv-key", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	srv.recordLocatedProcs(sessA, convoLocated{procs: []convoProc{
		{pid: 530048, start: 193322812, agent: "claude", sid: sid1, herdrPane: "w1:p1", herdrSocket: sock, cwd: "/srv/corral", anc: herdrAnc()},
		{pid: 530099, start: 193399999, agent: "claude", sid: sid2, herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other", anc: herdrAnc()},
		{pid: 600001, start: 194000000, agent: "claude", sid: "33333333-3333-3333-3333-333333333333", herdrPane: "w9:p1", herdrSocket: "/tmp/unmanaged.sock", cwd: "/srv/ext", anc: herdrAnc()},
	}}, true)

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
	sessB := &term.Session{ID: "sess-b", OwnerID: alice.ID, HostID: hostB.ID, HostName: "otherbox", Persistence: "herdr"}
	if otherPeers := srv.resolveConvoPeers(ctx, sessB, cst); len(otherPeers) != 0 {
		t.Fatalf("hostB resolved hostA peer: %+v", otherPeers)
	}

	// Sender exits (full scan no longer includes 530048:193322812).
	srv.recordLocatedProcs(sessA, convoLocated{procs: []convoProc{
		{pid: 530099, start: 193399999, agent: "claude", sid: sid2, herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other", anc: herdrAnc()},
	}}, true)
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

// Regression #1: a background/child Claude launched by a parent Claude inherits
// HERDR_PANE_ID and HERDR_SOCKET_PATH. Only the primary process chosen by
// pickConvoProc may map to the parent's pane; the child must stay unmatched
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

	sess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", Persistence: "herdr"}
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
	// Child spawned inside parent Claude (later start time, parent pid 100 in anc).
	bgChildProc := convoProc{
		pid: 400, start: 900, agent: "claude", sid: childSID,
		herdrPane: "w1:p1", herdrSocket: sock,
		anc: []convoAnc{{pid: 390, comm: "bash"}, {pid: 100, comm: "claude"}, {pid: 90, comm: "zsh"}, {pid: 80, comm: "herdr"}},
	}
	// Independent subagent running in its own herdr pane w1:p2.
	ownPaneSubProc := convoProc{
		pid: 500, start: 950, agent: "claude", sid: ownPaneSubSID,
		herdrPane: "w1:p2", herdrSocket: sock,
		anc: []convoAnc{{pid: 490, comm: "zsh"}, {pid: 80, comm: "herdr"}},
	}

	// Put bgChildProc AFTER parentProc so naive iteration would overwrite parentProc.
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{parentProc, bgChildProc, ownPaneSubProc}}, true)

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

// Regression #2: when a sender exits and a new agent takes over the same pane
// BEFORE the next locate scan runs, resolveConvoPeers must detect the changed
// session ID / terminal ID in the live snapshot and report the old peer as
// exited with its remembered old title (never alive, never overwritten).
func TestPeerResolutionReplacedPaneBeforeNextLocate(t *testing.T) {
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
		},
	}

	sess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", Persistence: "herdr"}
	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setServer(sess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return curSnap, time.Now()
	}, func() {})

	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: oldSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true)

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":100,"verifiedPeerProcStart":"500","name":"ai-old"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-old\">\nhi\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}

	// Initially alive and matched.
	peers := srv.resolveConvoPeers(ctx, sess, cst)
	if !peers["100:500"].Alive || peers["100:500"].Title != "Old Agent" {
		t.Fatalf("before replacement = %+v", peers["100:500"])
	}

	// Case A: new session starts in the same pane (different AgentSession, NO locate scan yet).
	curSnap = &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "new-agent-ws"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-old", AgentSession: &herdr.AgentSessionInfo{Value: newSID}, Label: "New Replacement Agent"},
		},
	}
	peers = srv.resolveConvoPeers(ctx, sess, cst)
	if peers["100:500"].Alive || peers["100:500"].Title != "Old Agent" {
		t.Fatalf("after session replacement without locate: got %+v, want Alive=false Title='Old Agent'", peers["100:500"])
	}

	// Case B: terminal recreated with new TerminalID (even if AgentSession omitted).
	curSnap = &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "new-agent-ws"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-new", Label: "Recreated Terminal Agent"},
		},
	}
	peers = srv.resolveConvoPeers(ctx, sess, cst)
	if peers["100:500"].Alive || peers["100:500"].Title != "Old Agent" {
		t.Fatalf("after terminal replacement without locate: got %+v, want Alive=false Title='Old Agent'", peers["100:500"])
	}
}

// Regression #4: a tmux peer identity recorded earlier must NOT be wrongly
// marked exited by a subsequent host-wide full scan when the tmux process is
// still running, and MUST be marked exited once it actually leaves /proc.
func TestPeerResolutionTmuxSurvivesFullScan(t *testing.T) {
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

	sock := "/tmp/herdr-test/default.sock"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "herdr-ws"}},
		Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t1", AgentSession: &herdr.AgentSessionInfo{Value: "11111111-1111-1111-1111-111111111111"}}},
	}
	herdrSess := &term.Session{ID: "s-herdr", OwnerID: alice.ID, HostID: hostA.ID, HostName: "box1", Persistence: "herdr"}
	tmuxSess := &term.Session{ID: "s-tmux", OwnerID: alice.ID, HostID: hostA.ID, HostName: "box1", Persistence: "tmux", TmuxSession: "main"}

	srv := &Server{store: st, herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(herdrSess.ID, sock)
	srv.herdrSocks.setServer(herdrSess.ID, "srv-1", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	// Bind tmux peer 300:1200.
	srv.recordBoundProc(tmuxSess, &convoBind{PID: 300, Start: 1200, Pane: "%1", Agent: "claude", Session: "22222222-2222-2222-2222-222222222222", Title: "✳ tmux-task"}, "tmux-convo-title")

	// Subsequent host-wide full scan triggered from herdrSess still sees tmux proc 300:1200 in /proc.
	srv.recordLocatedProcs(herdrSess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		{pid: 300, start: 1200, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/3"},
	}}, true)

	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":300,"verifiedPeerProcStart":"1200","name":"ai-tm"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-tm\">\ntm\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}

	peers := srv.resolveConvoPeers(ctx, tmuxSess, cst)
	if !peers["300:1200"].Alive || peers["300:1200"].Title != "tmux-convo-title" {
		t.Fatalf("tmux peer wrongly marked exited after full scan: %+v", peers["300:1200"])
	}

	// Once tmux proc 300:1200 actually exits (absent from next full scan), mark exited.
	srv.recordLocatedProcs(herdrSess, convoLocated{procs: []convoProc{
		{pid: 100, start: 500, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, true)
	peersAfter := srv.resolveConvoPeers(ctx, tmuxSess, cst)
	if peersAfter["300:1200"].Alive || peersAfter["300:1200"].Title != "tmux-convo-title" {
		t.Fatalf("tmux peer should be exited once absent from full scan: %+v", peersAfter["300:1200"])
	}
}

// Regression #5: failed SQLite persist must NOT mark cached as persisted (next
// observation retries), and concurrent writes must never let an older snapshot
// overwrite a newer observation in SQLite.
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
	tmuxSess := &term.Session{ID: "s-tmux", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", Persistence: "tmux", TmuxSession: "main"}

	// Part A: write failure must retry on the next observation with the SAME metadata.
	srv := &Server{store: st}
	var attempts atomic.Int32
	var failFirst atomic.Bool
	failFirst.Store(true)
	srv.peers.persistFn = func(ctx context.Context, ownerID, hostID int64, rows []store.PeerIdentity) error {
		attempts.Add(1)
		if failFirst.Load() {
			return errors.New("sqlite busy")
		}
		return st.UpsertPeerIdentities(ctx, ownerID, hostID, rows)
	}

	b := &convoBind{PID: 501, Start: 7001, Pane: "%1", Agent: "claude", Session: "11111111-1111-1111-1111-111111111111"}
	srv.recordBoundProc(tmuxSess, b, "same-title")
	if attempts.Load() != 1 {
		t.Fatalf("attempts after first write = %d, want 1", attempts.Load())
	}
	// DB must NOT have the row yet.
	if rows, _ := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []store.PeerProcKey{{PID: 501, ProcStart: 7001}}); len(rows) != 0 {
		t.Fatalf("expected 0 DB rows after failed persist, got %+v", rows)
	}
	// Next observation with identical metadata retries and succeeds.
	failFirst.Store(false)
	srv.recordBoundProc(tmuxSess, b, "same-title")
	if attempts.Load() != 2 {
		t.Fatalf("attempts after retry = %d, want 2", attempts.Load())
	}
	if rows, _ := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []store.PeerProcKey{{PID: 501, ProcStart: 7001}}); rows[store.PeerProcKey{PID: 501, ProcStart: 7001}].Title != "same-title" {
		t.Fatalf("retry did not persist row: %+v", rows)
	}

	// Part B: concurrent observation ordering — even when an older goroutine is
	// paused before committing and a newer observation arrives, SQLite must end
	// with the newer title ("title-v2"), never overwritten by "title-v1".
	srv2 := &Server{store: st}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var firstPaused atomic.Bool
	srv2.peers.persistFn = func(ctx context.Context, ownerID, hostID int64, rows []store.PeerIdentity) error {
		if firstPaused.CompareAndSwap(false, true) {
			entered <- struct{}{}
			<-release
		}
		return st.UpsertPeerIdentities(ctx, ownerID, hostID, rows)
	}

	b2 := &convoBind{PID: 502, Start: 7002, Pane: "%2", Agent: "claude", Session: "22222222-2222-2222-2222-222222222222"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv2.recordBoundProc(tmuxSess, b2, "title-v1")
	}()
	<-entered // goroutine 1 holds persistMu with "title-v1"
	wg.Add(1)
	go func() {
		defer wg.Done()
		// goroutine 2 records "title-v2" in memory, then waits on persistMu.
		srv2.recordBoundProc(tmuxSess, b2, "title-v2")
	}()
	// Give goroutine 2 a moment to update in-memory state and queue on persistMu.
	time.Sleep(25 * time.Millisecond)
	close(release)
	wg.Wait()

	rows, err := st.LookupPeerIdentities(ctx, alice.ID, hostA.ID, []store.PeerProcKey{{PID: 502, ProcStart: 7002}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[store.PeerProcKey{PID: 502, ProcStart: 7002}].Title; got != "title-v2" {
		t.Fatalf("concurrent persist ended with title=%q, want %q", got, "title-v2")
	}
}

// Regression #6 & #9: throttled hourly last_seen refresh and 60-second negative
// cache avoiding repeated SQLite queries for unknown peer refs.
func TestPeerNegativeCacheAndHourlyLastSeenThrottle(t *testing.T) {
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
	tmuxSess := &term.Session{ID: "s-tmux", OwnerID: alice.ID, HostID: hostA.ID, HostName: "devbox", Persistence: "tmux", TmuxSession: "main"}

	curTime := time.Unix(1_700_000_000, 0)
	var lookups, persists atomic.Int32
	srv := &Server{store: st}
	srv.peers.nowFn = func() time.Time { return curTime }
	srv.peers.lookupFn = func(ctx context.Context, ownerID, hostID int64, keys []store.PeerProcKey) (map[store.PeerProcKey]store.PeerIdentity, error) {
		lookups.Add(1)
		return st.LookupPeerIdentities(ctx, ownerID, hostID, keys)
	}
	srv.peers.persistFn = func(ctx context.Context, ownerID, hostID int64, rows []store.PeerIdentity) error {
		persists.Add(1)
		return st.UpsertPeerIdentities(ctx, ownerID, hostID, rows)
	}

	// Negative cache (item 9): repeated polls for an unknown peer only query SQLite once per 60s.
	parser := makePeerParser(t,
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":999,"verifiedPeerProcStart":"8888","name":"ai-miss"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-miss\">\nhi\n</cross-session-message>"}}`,
	)
	cst := &convoState{parser: parser}
	srv.resolveConvoPeers(ctx, tmuxSess, cst)
	srv.resolveConvoPeers(ctx, tmuxSess, cst)
	srv.resolveConvoPeers(ctx, tmuxSess, cst)
	if lookups.Load() != 1 {
		t.Fatalf("expected 1 SQLite lookup within negative cache TTL, got %d", lookups.Load())
	}
	curTime = curTime.Add(61 * time.Second)
	srv.resolveConvoPeers(ctx, tmuxSess, cst)
	if lookups.Load() != 2 {
		t.Fatalf("expected 2nd SQLite lookup after negative cache expired, got %d", lookups.Load())
	}

	// Observing the previously-missed peer immediately clears negative cache.
	b := &convoBind{PID: 999, Start: 8888, Pane: "%1", Agent: "claude", Session: "33333333-3333-3333-3333-333333333333"}
	srv.recordBoundProc(tmuxSess, b, "found-now")
	if peers := srv.resolveConvoPeers(ctx, tmuxSess, cst); !peers["999:8888"].Alive || peers["999:8888"].Title != "found-now" {
		t.Fatalf("peer should resolve immediately once observed: %+v", peers)
	}
	if persists.Load() != 1 {
		t.Fatalf("persists after initial observation = %d, want 1", persists.Load())
	}

	// Throttled hourly last_seen refresh (item 6): repeated identical observations
	// within 1 hour do not write SQLite; after 1 hour, last_seen refreshes once.
	curTime = curTime.Add(30 * time.Minute)
	srv.recordBoundProc(tmuxSess, b, "found-now")
	if persists.Load() != 1 {
		t.Fatalf("persists within 1h throttle = %d, want 1", persists.Load())
	}
	curTime = curTime.Add(31 * time.Minute)
	srv.recordBoundProc(tmuxSess, b, "found-now")
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
