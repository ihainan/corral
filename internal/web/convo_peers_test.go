package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
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

// Positive test: online senders (herdr pane & tmux active pane) whose verified
// (pid, proc_start) matches the current locate's pickConvoProc winner return
// their Corral name and session/pane link.
func TestPeerOnlineSenderMatchesCorralNameAndLink(t *testing.T) {
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
	}}, true, t0)

	tmuxProc := convoProc{pid: 300, start: 1200, agent: "claude", sid: "44444444-4444-4444-4444-444444444444", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/3"}
	srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/3", "✳ tmux-worker", tmuxProc), true, t0)

	// Verify herdr online match (reused PID with different start and unmanaged socket are omitted).
	hParser := makePeerParser(t,
		peerMsgLine("u1", 530048, 193322812, "ai-68"),
		peerMsgLine("u2", 530048, 999999999, "ai-reused"),
		peerMsgLine("u3", 600001, 194000000, "ai-ext"),
	)
	hPeers := srv.resolveConvoPeers(herdrSess, &convoState{parser: hParser})
	if len(hPeers) != 1 {
		t.Fatalf("herdr peers = %+v, want 1", hPeers)
	}
	p1 := hPeers["530048:193322812"]
	if p1.SessionID != "sess-herdr" || p1.Pane != "w1:p1" || p1.Title != "corral-work" || p1.HostName != "devbox" || p1.Persistence != "herdr" {
		t.Fatalf("herdr p1 = %+v", p1)
	}

	// Verify tmux online match.
	tParser := makePeerParser(t, peerMsgLine("u4", 300, 1200, "ai-tm"))
	tPeers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: tParser})
	if len(tPeers) != 1 {
		t.Fatalf("tmux peers = %+v, want 1", tPeers)
	}
	pt := tPeers["300:1200"]
	if pt.SessionID != "sess-tmux" || pt.Pane != "" || pt.Title != "tmux-worker" || pt.HostName != "devbox" || pt.Persistence != "tmux" || pt.MuxSession != "main" {
		t.Fatalf("tmux pt = %+v", pt)
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
	srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{parentProc, bgChildProc}}, true, time.Now())

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

// Fix3 scenarios: every case where current pickConvoProc selection does NOT
// match the message's own verified (pid, start) MUST return unmatched (omitted).
func TestPeerFix3Scenarios(t *testing.T) {
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
		}}, true, t1)
		// Same session UUID resumed with a new PID (200:600); old PID 100:500 gone.
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 200, start: 600, agent: "claude", sid: sid, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, t1.Add(time.Second))

		parser := makePeerParser(t, peerMsgLine("u1", 100, 500, "ai-old"), peerMsgLine("u2", 200, 600, "ai-new"))
		peers := srv.resolveConvoPeers(sess, &convoState{parser: parser})
		if _, ok := peers["100:500"]; ok {
			t.Fatalf("old PID from same resumed session must not match: %+v", peers)
		}
		if peers["200:600"].SessionID != "s-herdr" || peers["200:600"].Title != "Worker" {
			t.Fatalf("new resumed PID should match: %+v", peers)
		}
	})

	t.Run("tmux_background_old_process", func(t *testing.T) {
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{}
		bgOld := convoProc{pid: 300, start: 1200, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
		fgNew := convoProc{pid: 400, start: 1500, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", tmuxPane: "%2", tmuxServerPID: "99", tty: "/dev/pts/2"}

		// Active pane is %2 (fgNew); bgOld is still alive in background pane %1.
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%2", "99", "/dev/pts/2", "✳ active", bgOld, fgNew), true, time.Now())

		parser := makePeerParser(t, peerMsgLine("u1", 300, 1200, "ai-bg"), peerMsgLine("u2", 400, 1500, "ai-fg"))
		peers := srv.resolveConvoPeers(tmuxSess, &convoState{parser: parser})
		if _, ok := peers["300:1200"]; ok {
			t.Fatalf("background tmux process 300:1200 must not match: %+v", peers)
		}
		if peers["400:1500"].SessionID != "s-tmux" || peers["400:1500"].Title != "active" {
			t.Fatalf("active tmux process 400:1500 should match: %+v", peers)
		}
	})

	t.Run("many_peers_no_eviction_and_unmatched_misses", func(t *testing.T) {
		tmuxSess := &term.Session{ID: "s-tmux", OwnerID: 1, HostID: 10, HostName: "devbox", HostConfig: "cfg1", Persistence: "tmux", TmuxSession: "main"}
		srv := &Server{}
		live := convoProc{pid: 99999, start: 88888, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ live-one", live), true, time.Now())

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
		// Newer scan completes first with 200:600.
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 200, start: 600, agent: "claude", sid: sid2, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, tNew)
		// Older scan from tOld arrives late with 100:500 -> must be ignored.
		srv.recordLocatedProcs(sess, convoLocated{procs: []convoProc{
			{pid: 100, start: 500, agent: "claude", sid: sid1, herdrPane: "w1:p1", herdrSocket: sock, anc: herdrAnc()},
		}}, true, tOld)

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
		}}, true, t0)
		srv.recordLocatedProcs(tmuxSess, tmuxLocated("%1", "99", "/dev/pts/1", "✳ tm",
			convoProc{pid: 300, start: 1200, agent: "claude", sid: sid, tmuxPane: "%1", tmuxServerPID: "99", tty: "/dev/pts/1"}), true, t0)

		// Empty full scan replaces everything with an empty map.
		srv.recordLocatedProcs(herdrSess, convoLocated{procs: nil}, true, t0.Add(time.Second))
		srv.recordLocatedProcs(tmuxSess, convoLocated{procs: nil}, true, t0.Add(time.Second))

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
		// Even with an old st.bind present, if no current locate scan selected
		// that (pid, start), resolveConvoPeers must return nil.
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
		srv.recordLocatedProcs(sessOld, tmuxLocated("%1", "77", "/dev/pts/1", "✳ old-machine", procOld), true, t0)

		parser := makePeerParser(t, peerMsgLine("u1", 500, 1000, "ai-old"))
		// Retargeted host config must NOT read sessOld's live peer.
		if peers := srv.resolveConvoPeers(sessNew, &convoState{parser: parser}); len(peers) != 0 {
			t.Fatalf("retargeted host config read old machine's peer: %+v", peers)
		}
		// Empty scan on sessNew must NOT clear sessOld.
		srv.recordLocatedProcs(sessNew, tmuxLocated("%1", "88", "/dev/pts/2", "✳ new-machine"), true, t0.Add(time.Second))
		if peers := srv.resolveConvoPeers(sessOld, &convoState{parser: parser}); peers["500:1000"].SessionID != "s-old" {
			t.Fatalf("sessOld should remain unaffected by sessNew scan: %+v", peers)
		}
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
