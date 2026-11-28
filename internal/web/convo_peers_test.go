package web

import (
	"context"
	"fmt"
	"strings"
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

func peerMsgLine(uuid string, pid, start int64, name string) string {
	return fmt.Sprintf(
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":%q,"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/%d.sock","verifiedPeerPid":%d,"verifiedPeerProcStart":"%d","name":%q},"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/%d.sock\" from-name=\"%s\">\nhello\n</cross-session-message>"}}`,
		uuid, pid, pid, start, name, pid, name,
	)
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

// Positive test + B#2: online senders (herdr pane & tmux active pane) match
// their Corral name and link, including custom pane Label present ONLY in
// herdr.Snapshot.Panes (not in Snapshot.Agents).
func TestPeerOnlineSenderMatchesCorralNameAndLink(t *testing.T) {
	sock := "/tmp/herdr-test/default.sock"
	sid1 := "11111111-1111-1111-1111-111111111111"
	sid2 := "22222222-2222-2222-2222-222222222222"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "ws1", Label: "corral-work"},
			{ID: "ws2", Label: "~"},
		},
		// Realistic herdr structure (B#2): custom Label lives in Panes, while
		// Agents carries Agent/AgentSession/TerminalID with empty Label.
		Panes: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Cwd: "/srv/corral"},
			{ID: "w2:p1", WorkspaceID: "ws2", Label: "custom-pane-name", Cwd: "/srv/other"},
		},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "term-1", AgentSession: &herdr.AgentSessionInfo{Value: sid1}, Title: "✳ fix cross-session", Cwd: "/srv/corral"},
			{ID: "w2:p1", WorkspaceID: "ws2", Agent: "claude", TerminalID: "term-2", AgentSession: &herdr.AgentSessionInfo{Value: sid2}, Label: "", Title: "✳ review", Cwd: "/srv/other"},
		},
	}

	cfg := "1|10.0.0.1|22|alice|password|0"
	herdrSess := &term.Session{ID: "sess-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: cfg, Persistence: "herdr"}
	tmuxSess := &term.Session{ID: "sess-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: cfg, Persistence: "tmux", TmuxSession: "main"}

	srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(herdrSess.ID, sock)
	srv.herdrSocks.setServer(herdrSess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	t0 := time.Now()
	srv.recordLocatedProcs(herdrSess, convoLocated{procs: []convoProc{
		{pid: 530048, start: 193322812, agent: "claude", sid: sid1, herdrPane: "w1:p1", herdrSocket: sock, cwd: "/srv/corral", anc: herdrAnc()},
		{pid: 530099, start: 193399999, agent: "claude", sid: sid2, herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other", anc: herdrAnc()},
		{pid: 600001, start: 194000000, agent: "claude", sid: "33333333-3333-3333-3333-333333333333", herdrPane: "w9:p1", herdrSocket: "/tmp/unmanaged.sock", cwd: "/srv/ext", anc: herdrAnc()},
	}}, true, "", t0)

	tmuxProc := convoProc{pid: 300, start: 1200, agent: "claude", sid: "44444444-4444-4444-4444-444444444444", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/3"}
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/3", "✳ tmux-worker", tmuxProc), true, "", t0)

	hParser := makePeerParser(t,
		peerMsgLine("u1", 530048, 193322812, "ai-68"),
		peerMsgLine("u2", 530099, 193399999, "ai-rev"),
		peerMsgLine("u3", 530048, 999999999, "ai-reused"),
		peerMsgLine("u4", 600001, 194000000, "ai-ext"),
	)
	hPeers := srv.resolveConvoPeers(herdrSess, &convoState{parser: hParser})
	if len(hPeers) != 2 {
		t.Fatalf("herdr peers = %+v, want 2", hPeers)
	}
	p1 := hPeers["530048:193322812"]
	if p1.SessionID != "sess-herdr" || p1.Pane != "w1:p1" || p1.Title != "corral-work" || p1.HostName != "devbox" || p1.Persistence != "herdr" {
		t.Fatalf("herdr p1 = %+v", p1)
	}
	// B#2: pane w2:p1 must keep custom Label from Panes even though Agents.Label is "".
	p2 := hPeers["530099:193399999"]
	if p2.SessionID != "sess-herdr" || p2.Pane != "w2:p1" || p2.Title != "custom-pane-name" {
		t.Fatalf("herdr p2 = %+v, want custom-pane-name preserved from Panes", p2)
	}

	// Verify tmux online match.
	tParser := makePeerParser(t, peerMsgLine("u5", 300, 1200, "ai-tm"))
	tPeers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: tParser})
	if len(tPeers) != 1 {
		t.Fatalf("tmux peers = %+v, want 1", tPeers)
	}
	pt := tPeers["300:1200"]
	if pt.SessionID != "sess-tmux" || pt.Pane != "" || pt.Title != "tmux-worker" || pt.HostName != "devbox" || pt.Persistence != "tmux" || pt.MuxSession != "main" {
		t.Fatalf("tmux pt = %+v", pt)
	}
}

// Regression fix4 Part A: single-pane fast locate (fastHit, full=false) records
// ONLY its covered pane without clearing or inferring other panes. Opening S2
// (acme-web) via fast locate and then opening S1 (claude) via fast locate keeps
// S2's peer identity available when rendering S1's conversation.
func TestPeerFastLocateUpdatesOnlyCoveredPane(t *testing.T) {
	sock := "/tmp/herdr-test/default.sock"
	sid1 := "11111111-1111-1111-1111-111111111111"
	sid2 := "22222222-2222-2222-2222-222222222222"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "ws1", Label: "main"},
			{ID: "ws2", Label: "acme-web"},
		},
		Panes: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1"},
			{ID: "w2:p1", WorkspaceID: "ws2"},
		},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", AgentStatus: herdr.StatusWorking, TerminalID: "t1"},
			{ID: "w2:p1", WorkspaceID: "ws2", Agent: "claude", AgentStatus: herdr.StatusBlocked, TerminalID: "t2"},
		},
	}

	sess := &term.Session{ID: "sess-herdr", OwnerID: 1, HostID: 10, HostName: "test", HostConfig: "cfg1", Persistence: "herdr"}
	srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return snap, time.Now() }, func() {})

	t0 := time.Now()
	// Step 1: /agents opens acme-web (w2:p1) first via fastHit (full=false, coveredPane="w2:p1").
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 222, start: 2222, agent: "claude", sid: sid2, herdrPane: "w2:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, false, "w2:p1", t0)

	// Step 2: user clicks claude (w1:p1), which also hits fastHit (full=false, coveredPane="w1:p1").
	// Must NOT wipe w2:p1!
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
		{pid: 111, start: 1111, agent: "claude", sid: sid1, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
	}}, false, "w1:p1", t0.Add(time.Second))

	// Step 3: S1's conversation resolves peer message from S2 (222:2222).
	parser := makePeerParser(t, peerMsgLine("xs1", 222, 2222, "ai-77"), peerMsgLine("xs2", 1, 1, "ai-99"))
	peers := srv.resolveConvoPeers(sess, &convoState{parser: parser})
	if len(peers) != 1 || peers["222:2222"].Title != "acme-web" || peers["222:2222"].SessionID != "sess-herdr" || peers["222:2222"].Pane != "w2:p1" {
		t.Fatalf("after fast locates, peers = %+v, want acme-web (222:2222)", peers)
	}

	// Step 4: fast locate on w2:p1 finding no agent clears ONLY w2:p1, leaving w1:p1 intact.
	srv.recordLocatedProcs(sess, convoLocated{procs: nil}, false, "w2:p1", t0.Add(2*time.Second))
	parserBoth := makePeerParser(t, peerMsgLine("m1", 111, 1111, "ai-1"), peerMsgLine("m2", 222, 2222, "ai-2"))
	peersAfter := srv.resolveConvoPeers(sess, &convoState{parser: parserBoth})
	if _, ok := peersAfter["222:2222"]; ok {
		t.Fatalf("cleared w2:p1 should no longer match: %+v", peersAfter)
	}
	if peersAfter["111:1111"].Pane != "w1:p1" {
		t.Fatalf("uncovered w1:p1 should remain intact: %+v", peersAfter)
	}
}

// Background/child Claude inheriting HERDR_PANE_ID is never matched to parent's pane.
func TestPeerBackgroundChildNotMatchedToParent(t *testing.T) {
	sock := "/tmp/herdr-test/default.sock"
	parentSID := "11111111-1111-1111-1111-111111111111"
	childSID := "22222222-2222-2222-2222-222222222222"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "parent-work"}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t-parent", AgentSession: &herdr.AgentSessionInfo{Value: parentSID}, Label: "Parent Agent"},
		},
	}

	sess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
	srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	srv.herdrSocks.set(sess.ID, sock)
	srv.herdrSocks.setSids(sess.ID, snap)
	srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	parentProc := convoProc{pid: 100, start: 500, agent: "claude", sid: parentSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()}
	bgChildProc := convoProc{
		pid: 400, start: 900, agent: "claude", sid: childSID, herdrPane: "w1:p1", herdrSocket: sock,
		anc: []convoAnc{{pid: 390, comm: "bash"}, {pid: 100, comm: "claude"}, {pid: 90, comm: "zsh"}, {pid: 80, comm: "herdr"}},
	}
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{parentProc, bgChildProc}}, true, "", time.Now())

	parser := makePeerParser(t,
		peerMsgLine("u1", 100, 500, "ai-parent"),
		peerMsgLine("u2", 400, 900, "ai-bg-child"),
	)
	peers := srv.resolveConvoPeers(sess, &convoState{parser: parser})
	if peers["100:500"].Title != "Parent Agent" {
		t.Fatalf("parent 100:500 = %+v", peers["100:500"])
	}
	if p, ok := peers["400:900"]; ok {
		t.Fatalf("background child 400:900 must not match parent pane, got %+v", p)
	}
}

// Regression fix4 B#5: corraldMatchesSession refuses to use corrald for an old
// terminal session after its host record is retargeted to a different host
// config or bound host key / username.
func TestPeerCorraldRejectedAfterHostRetarget(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	u, err := st.CreateUser(ctx, "alice", "", "pw", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	h, err := st.CreateHost(ctx, &store.Host{
		OwnerID: u.ID, Name: "devbox", Address: "10.0.0.1", Port: 22, Username: "alice", Auth: store.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgA := hostConfig(h)
	sessOld := &term.Session{ID: "s-old", OwnerID: u.ID, HostID: h.ID, HostConfig: cfgA}
	srv := &Server{store: st}

	// Before retarget: matches.
	if !srv.corraldMatchesSession(ctx, sessOld) {
		t.Fatalf("expected corraldMatchesSession=true before host retarget")
	}

	// Retarget host record to another machine (address/port changed).
	h.Address = "10.0.0.2"
	h.Port = 2222
	if err := st.UpdateHost(ctx, h, true); err != nil {
		t.Fatal(err)
	}
	if srv.corraldMatchesSession(ctx, sessOld) {
		t.Fatalf("old terminal session must NOT use corrald after host record retarget")
	}
	// New terminal opened with the retargeted config matches again.
	h2, err := st.Host(ctx, u.ID, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessNew := &term.Session{ID: "s-new", OwnerID: u.ID, HostID: h.ID, HostConfig: hostConfig(h2)}
	if !srv.corraldMatchesSession(ctx, sessNew) {
		t.Fatalf("expected corraldMatchesSession=true for new retargeted session")
	}
}

// Fix3 + Fix4 scenarios: every case where current pickConvoProc selection does
// NOT match the message's own verified (pid, start) MUST return unmatched.
func TestPeerFix3AndFix4Scenarios(t *testing.T) {
	t.Run("same_session_resumed_new_pid", func(t *testing.T) {
		sock := "/tmp/herdr-test/default.sock"
		sid := "11111111-1111-1111-1111-111111111111"
		snap := &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "work"}},
			Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t1", AgentSession: &herdr.AgentSessionInfo{Value: sid}, Label: "Worker"}},
		}
		sess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.herdrSocks.set(sess.ID, sock)
		srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return snap, time.Now() }, func() {})

		t1 := time.Now()
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 100, start: 500, agent: "claude", sid: sid, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, "", t1)
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 200, start: 600, agent: "claude", sid: sid, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, "", t1.Add(time.Second))

		parser := makePeerParser(t, peerMsgLine("u1", 100, 500, "ai-old"), peerMsgLine("u2", 200, 600, "ai-new"))
		peers := srv.resolveConvoPeers(sess, &convoState{parser: parser})
		if _, ok := peers["100:500"]; ok {
			t.Fatalf("old PID from same resumed session must not match: %+v", peers)
		}
		if peers["200:600"].SessionID != "s-herdr" || peers["200:600"].Title != "Worker" {
			t.Fatalf("new resumed PID should match: %+v", peers)
		}
	})

	t.Run("tmux_background_old_process_and_own_scan_only", func(t *testing.T) {
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		herdrSess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		srv := &Server{}
		bgOld := convoProc{pid: 300, start: 1200, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
		fgNew := convoProc{pid: 400, start: 1500, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", tmuxPane: "%2", tmuxServerPID: "99", tty: "/dev/pts/2"}

		t0 := time.Now()
		// Active pane is %2 (fgNew); bgOld is still alive in background pane %1.
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%2", "99", "/dev/pts/2", "✳ active", bgOld, fgNew), true, "", t0)

		// B#1: a scan on another session (herdrSess) must NOT mutate or resurrect tmuxSess's entry.
		srv.recordLocatedProcs(herdrSess, convoLocated{procs: []convoProc{bgOld}}, true, "", t0.Add(time.Second))

		parser := makePeerParser(t, peerMsgLine("u1", 300, 1200, "ai-bg"), peerMsgLine("u2", 400, 1500, "ai-fg"))
		peers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: parser})
		if _, ok := peers["300:1200"]; ok {
			t.Fatalf("background tmux process 300:1200 must not match: %+v", peers)
		}
		if peers["400:1500"].SessionID != "s-tmux" || peers["400:1500"].Title != "active" {
			t.Fatalf("active tmux process 400:1500 should match: %+v", peers)
		}

		// B#4: empty scan on tmuxSess itself advances scanAt so an older scan cannot resurrect 400:1500.
		tEmpty := t0.Add(2 * time.Second)
		srv.recordLocatedProcs(tmuxSess, convoLocated{procs: nil}, true, "", tEmpty)
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%2", "99", "/dev/pts/2", "✳ active", fgNew), true, "", t0.Add(1500*time.Millisecond))
		if peers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: parser}); len(peers) != 0 {
			t.Fatalf("older scan resurrected tmux peer after empty scan: %+v", peers)
		}
	})

	t.Run("many_peers_no_eviction_and_unmatched_misses", func(t *testing.T) {
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{}
		live := convoProc{pid: 99999, start: 88888, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ live-one", live), true, "", time.Now())

		var sb strings.Builder
		for i := 1; i <= 600; i++ {
			sb.WriteString(peerMsgLine(fmt.Sprintf("m%d", i), int64(10000+i), int64(50000+i), fmt.Sprintf("ai-%d", i)))
			sb.WriteByte('\n')
		}
		sb.WriteString(peerMsgLine("live", 99999, 88888, "ai-live"))
		sb.WriteByte('\n')
		p := convo.NewClaude()
		p.Feed([]byte(sb.String()))

		peers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: p})
		if len(peers) != 1 || peers["99999:88888"].Title != "live-one" {
			t.Fatalf("got %d peers (%+v), want only live 99999:88888", len(peers), peers)
		}
	})

	t.Run("late_older_scan_ignored", func(t *testing.T) {
		sock := "/tmp/herdr-test/default.sock"
		sid1 := "11111111-1111-1111-1111-111111111111"
		sid2 := "22222222-2222-2222-2222-222222222222"
		curSnap := &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "~"}},
			Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t2", AgentSession: &herdr.AgentSessionInfo{Value: sid2}, Label: "New Agent"}},
		}
		sess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.herdrSocks.set(sess.ID, sock)
		srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return curSnap, time.Now() }, func() {})

		tOld := time.Now().Add(-10 * time.Second)
		tNew := time.Now()
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 200, start: 600, agent: "claude", sid: sid2, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, "", tNew)
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 100, start: 500, agent: "claude", sid: sid1, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, "", tOld)

		parser := makePeerParser(t, peerMsgLine("u1", 100, 500, "ai-old"), peerMsgLine("u2", 200, 600, "ai-new"))
		peers := srv.resolveConvoPeers(sess, &convoState{parser: parser})
		if _, ok := peers["100:500"]; ok {
			t.Fatalf("late older scan leaked 100:500: %+v", peers)
		}
		if peers["200:600"].Title != "New Agent" {
			t.Fatalf("newer scan 200:600 clobbered: %+v", peers)
		}
	})

	t.Run("empty_full_scan_clears_all", func(t *testing.T) {
		sock := "/tmp/herdr-test/default.sock"
		sid := "11111111-1111-1111-1111-111111111111"
		snap := &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "work"}},
			Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t1", AgentSession: &herdr.AgentSessionInfo{Value: sid}, Label: "Worker"}},
		}
		herdrSess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.herdrSocks.set(herdrSess.ID, sock)
		srv.herdrSocks.setServer(herdrSess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return snap, time.Now() }, func() {})

		t0 := time.Now()
		srv.recordLocatedProcs(herdrSess, convoLocated{procs: []convoProc{
			{pid: 100, start: 500, agent: "claude", sid: sid, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, "", t0)
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ tm",
			convoProc{pid: 300, start: 1200, agent: "claude", sid: sid, tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}), true, "", t0)

		srv.recordLocatedProcs(herdrSess, convoLocated{procs: nil}, true, "", t0.Add(time.Second))
		srv.recordLocatedProcs(tmuxSess, convoLocated{procs: nil}, true, "", t0.Add(time.Second))

		parser := makePeerParser(t, peerMsgLine("u1", 100, 500, "ai-h"), peerMsgLine("u2", 300, 1200, "ai-t"))
		if peers := srv.resolveConvoPeers(herdrSess, &convoState{parser: parser}); len(peers) != 0 {
			t.Fatalf("herdr peers after empty scan = %+v, want empty", peers)
		}
		if peers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: parser}); len(peers) != 0 {
			t.Fatalf("tmux peers after empty scan = %+v, want empty", peers)
		}
	})

	t.Run("stale_convo_bind_ignored", func(t *testing.T) {
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{}
		parser := makePeerParser(t, peerMsgLine("u1", 100, 500, "ai-stale-bind"))
		st := &convoState{
			parser: parser,
			bind:   &convoBind{PID: 100, Start: 500, Pane: "%1", Agent: "claude", Session: "11111111-1111-1111-1111-111111111111", Title: "Old Bind"},
		}
		if peers := srv.resolveConvoPeers(tmuxSess, st); len(peers) != 0 {
			t.Fatalf("stale st.bind must never resolve without current locate: %+v", peers)
		}
	})

	t.Run("host_config_retarget_isolated", func(t *testing.T) {
		sessOld := &term.Session{ID: "s-old", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "1|10.0.0.1|22|alice|password|0", Persistence: "tmux", TmuxSession: "main"}
		sessNew := &term.Session{ID: "s-new", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "2|10.0.0.9|2222|bob|password|0", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{}

		t0 := time.Now()
		procOld := convoProc{pid: 500, start: 1000, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "77", tty: "/dev/pts/1"}
		srv.recordLocatedProcs(sessOld, tmuxLocated("%1", "77", "/dev/pts/1", "✳ old-machine", procOld), true, "", t0)

		parser := makePeerParser(t, peerMsgLine("u1", 500, 1000, "ai-old"))
		if peers := srv.resolveConvoPeers(sessNew, &convoState{parser: parser}); len(peers) != 0 {
			t.Fatalf("retargeted host config read old machine's peer: %+v", peers)
		}
		srv.recordLocatedProcs(sessNew, tmuxLocated("%1", "88", "/dev/pts/2", "✳ new-machine"), true, "", t0.Add(time.Second))
		if peers := srv.resolveConvoPeers(sessOld, &convoState{parser: parser}); peers["500:1000"].SessionID != "s-old" {
			t.Fatalf("sessOld should remain unaffected by sessNew scan: %+v", peers)
		}
	})

	// B#10: subagent conversation state also resolves Peers via resolveConvoPeers.
	t.Run("subagent_convo_resolves_peers", func(t *testing.T) {
		sock := "/tmp/herdr-test/default.sock"
		sid := "11111111-1111-1111-1111-111111111111"
		snap := &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "acme-web"}},
			Panes:      []herdr.Pane{{ID: "w1:p2", WorkspaceID: "ws1"}},
			Agents:     []herdr.Pane{{ID: "w1:p2", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t2", AgentSession: &herdr.AgentSessionInfo{Value: sid}}},
		}
		sess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.herdrSocks.set(sess.ID, sock)
		srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return snap, time.Now() }, func() {})
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 222, start: 2222, agent: "claude", sid: sid, herdrPane: "w1:p2", herdrSocket: sock, anc: herdrAnc()},
		}}, false, "w1:p2", time.Now())

		subParser := convo.NewClaudeSub()
		subParser.Feed([]byte(peerMsgLine("u-sub", 222, 2222, "ai-77") + "\n"))
		peers := srv.resolveConvoPeers(sess, &convoState{sub: true, parser: subParser})
		if peers["222:2222"].Title != "acme-web" || peers["222:2222"].Pane != "w1:p2" {
			t.Fatalf("subagent convo peers = %+v, want acme-web", peers)
		}
	})

	// Fix5 fallback TTL rule: slots not refreshed for > peerSlotMaxAge (10m)
	// stop matching (show original name + "not in Corral"), using a fake clock.
	t.Run("slot_expires_after_10m_without_refresh", func(t *testing.T) {
		curTime := time.Unix(1_750_000_000, 0)
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{}
		srv.peers.nowFn = func() time.Time { return curTime }

		proc := convoProc{pid: 300, start: 1200, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ worker", proc), true, "", curTime)

		parser := makePeerParser(t, peerMsgLine("u1", 300, 1200, "ai-tm"))
		cst := &convoState{parser: parser}

		// At 9m59s: still valid.
		curTime = curTime.Add(peerSlotMaxAge - time.Second)
		if peers := srv.resolveConvoPeers(tmuxSess, cst); peers["300:1200"].Title != "worker" {
			t.Fatalf("at 9m59s expected match, got %+v", peers)
		}
		// At 10m01s: expired -> must NOT match.
		curTime = curTime.Add(2 * time.Second)
		if peers := srv.resolveConvoPeers(tmuxSess, cst); len(peers) != 0 {
			t.Fatalf("at 10m01s expired slot must not match, got %+v", peers)
		}
		// Refreshed by a new scan -> matches again.
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ worker", proc), true, "", curTime)
		if peers := srv.resolveConvoPeers(tmuxSess, cst); peers["300:1200"].Title != "worker" {
			t.Fatalf("after refresh expected match, got %+v", peers)
		}
	})

	// Fix5 #2: locatedSID from an OLDER scan must NOT veto a newer slot; a
	// SAME-OR-NEWER scan's locatedSID DOES veto a mismatched slot. Also a
	// background child Claude in the same pane must not overwrite parent SID.
	t.Run("locatedSID_veto_respects_scan_time_and_primary_only", func(t *testing.T) {
		sock := "/tmp/herdr-test/default.sock"
		oldSID := "11111111-1111-1111-1111-111111111111"
		newSID := "22222222-2222-2222-2222-222222222222"
		newerSID := "33333333-3333-3333-3333-333333333333"
		snap := &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "acme-web"}},
			Panes:      []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1"}},
			Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t1"}},
		}
		sess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.herdrSocks.set(sess.ID, sock)
		srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return snap, time.Now() }, func() {})

		t1 := time.Now().Add(-20 * time.Second)
		t2 := t1.Add(10 * time.Second)
		t3 := t2.Add(5 * time.Second)

		// Older locatedSID from t1 (oldSID).
		srv.herdrSocks.setLocated(sess.ID, "w1:p1", oldSID, t1)
		// Newer locate scan at t2 selects (200:600, newSID), while a background
		// child Claude (400:900, childSID) shares the same pane.
		parentProc := convoProc{pid: 200, start: 600, agent: "claude", sid: newSID, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()}
		childProc := convoProc{
			pid: 400, start: 900, agent: "claude", sid: newerSID, herdrPane: "w1:p1", herdrSocket: sock,
			anc: []convoAnc{{pid: 390, comm: "bash"}, {pid: 200, comm: "claude"}, {pid: 90, comm: "zsh"}, {pid: 80, comm: "herdr"}},
		}
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{parentProc, childProc}}, false, "w1:p1", t2)

		parser := makePeerParser(t, peerMsgLine("u1", 200, 600, "ai-parent"))
		cst := &convoState{parser: parser}
		// Older locatedSID (t1 < t2) MUST NOT veto the newer slot (t2).
		if peers := srv.resolveConvoPeers(sess, cst); peers["200:600"].Title != "acme-web" {
			t.Fatalf("stale older locatedSID wrongly vetoed newer slot: %+v", peers)
		}

		// Watcher recording locatedSIDs for [parentProc, childProc] at t2 must
		// keep parent's newSID (NOT child's newerSID) so parent is NOT vetoed!
		srv.recordHerdrLocatedSIDs(sess, sock, []convoProc{parentProc, childProc}, t2)
		if gotSID := srv.herdrSocks.locatedOf(sess.ID, "w1:p1"); gotSID != newSID {
			t.Fatalf("recordHerdrLocatedSIDs = %q, want parent %q (not child)", gotSID, newSID)
		}
		if peers := srv.resolveConvoPeers(sess, cst); peers["200:600"].Title != "acme-web" {
			t.Fatalf("child Claude in same pane wrongly vetoed parent slot: %+v", peers)
		}

		// Newer locatedSID (t3 > t2) with different SID MUST veto the stale slot.
		srv.herdrSocks.setLocated(sess.ID, "w1:p1", newerSID, t3)
		if peers := srv.resolveConvoPeers(sess, cst); len(peers) != 0 {
			t.Fatalf("newer mismatched locatedSID should veto slot, got %+v", peers)
		}
	})

	// Fix5 #1: a background full scan (like refreshCodexClaims via convoScans)
	// feeds its parsed locate result into peerLocateCache using the same full
	// scan entry point and replaces stale panes.
	t.Run("refreshCodexClaims_feeds_peer_cache", func(t *testing.T) {
		sock := "/tmp/herdr-test/default.sock"
		sid := "11111111-1111-1111-1111-111111111111"
		snap := &herdr.Snapshot{
			Workspaces: []herdr.Workspace{{ID: "ws1", Label: "codex-bg-ws"}},
			Panes:      []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1"}},
			Agents:     []herdr.Pane{{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", TerminalID: "t1", AgentSession: &herdr.AgentSessionInfo{Value: sid}}},
		}
		sess := &term.Session{ID: "s-herdr", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "herdr"}
		srv := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
		srv.herdrSocks.set(sess.ID, sock)
		srv.herdrSocks.setServer(sess.ID, "srv-1", 1, func() (*herdr.Snapshot, time.Time) { return snap, time.Now() }, func() {})

		// Seed convoScans with a completed full scan result so refreshCodexClaims
		// consumes it synchronously without any SSH/remote command.
		fullCmd := convoLocateCommand(sess.Persistence, sess.CurrentTmuxSession(), "")
		scanOut := convoRec(
			"pid=700", "start=7700", "procstart=7700", "agent=claude",
			"sid="+sid, "HERDR_PANE_ID=w1:p1", "HERDR_SOCKET_PATH="+sock,
			"anc=90:zsh,80:herdr",
		)
		done := make(chan struct{})
		close(done)
		now := time.Now()
		srv.convoScans.m = map[string]*convoScan{
			sess.ID + "\x00" + fullCmd: {done: done, out: []byte(scanOut), at: now, began: now},
		}
		// Seed an active Codex claim for hostKey so refreshCodexClaims runs.
		srv.convoClaimed.takeOn(convoClaimHostKey(sess), "codex-sid-1", procID{900, 9900}, 1000, nil, now.UnixMilli(), false)

		srv.refreshCodexClaims(sess)
		parser := makePeerParser(t, peerMsgLine("u1", 700, 7700, "ai-bg"))
		cst := &convoState{parser: parser}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if peers := srv.resolveConvoPeers(sess, cst); peers["700:7700"].Title == "codex-bg-ws" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("refreshCodexClaims did not feed full scan result into peer cache: %+v", srv.resolveConvoPeers(sess, cst))
	})
}

func TestPeerSanitization(t *testing.T) {
	if got := cleanPeerPaneTitle("✳ \x1b\x07\u202ehello\x00world"); got != "helloworld" {
		t.Fatalf("cleanPeerPaneTitle = %q, want %q", got, "helloworld")
	}
	if got := cleanPeerPaneTitle("claude --resume foo"); got != "" {
		t.Fatalf("launch cmd should be stripped, got %q", got)
	}
}
