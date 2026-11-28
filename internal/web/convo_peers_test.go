package web

import (
	"context"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

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
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{
			{ID: "ws1", Label: "corral-work"},
			{ID: "ws2", Label: "~"},
		},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "ws1", Agent: "claude", Title: "✳ fix cross-session", Cwd: "/srv/corral"},
			{ID: "w2:p1", WorkspaceID: "ws2", Agent: "claude", Label: "reviewer-pane", Title: "✳ review", Cwd: "/srv/other"},
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

	// Observe two Corral-managed Claude processes and one unmanaged process (different socket).
	srv.recordLocatedProcs(sessA, []convoProc{
		{pid: 530048, start: 193322812, agent: "claude", sid: "11111111-1111-1111-1111-111111111111", herdrPane: "w1:p1", herdrSocket: sock, cwd: "/srv/corral"},
		{pid: 530099, start: 193399999, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other"},
		{pid: 600001, start: 194000000, agent: "claude", sid: "33333333-3333-3333-3333-333333333333", herdrPane: "w9:p1", herdrSocket: "/tmp/unmanaged.sock", cwd: "/srv/ext"},
	}, true)

	// Build a receiver conversation that received messages from:
	// 1. 530048:193322812 (matched live in Corral)
	// 2. 530048:999999999 (same PID, wrong procStart -> PID reuse, must NOT match)
	// 3. 600001:194000000 (unmanaged socket -> must NOT match)
	parser := convo.NewClaude()
	parser.Feed([]byte(
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"193322812","name":"ai-68"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\">\nhi 1\n</cross-session-message>"}}` + "\n" +
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"999999999","name":"ai-reused"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-reused\">\nhi 2\n</cross-session-message>"}}` + "\n" +
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u3","timestamp":"2026-11-28T03:00:02Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/600001.sock","verifiedPeerPid":600001,"verifiedPeerProcStart":"194000000","name":"ai-ext"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/600001.sock\" from-name=\"ai-ext\">\nhi 3\n</cross-session-message>"}}` + "\n",
	))
	cst := &convoState{parser: parser}

	peers := srv.resolveConvoPeers(ctx, sessA, cst)
	if len(peers) != 1 {
		t.Fatalf("got %d resolved peers, want 1: %+v", len(peers), peers)
	}
	p1, ok := peers["530048:193322812"]
	if !ok {
		t.Fatalf("missing 530048:193322812 in %+v", peers)
	}
	if !p1.Alive || p1.SessionID != sessA.ID || p1.Pane != "w1:p1" || p1.Title != "corral-work" || p1.HostName != "devbox" || p1.Key != herdrAgentKey(hostA.ID, "", "w1:p1") {
		t.Fatalf("resolved p1 = %+v", p1)
	}

	// Host isolation: a session on hostB must not resolve hostA's peer PID.
	sessB := &term.Session{ID: "sess-b", OwnerID: alice.ID, HostID: hostB.ID, HostName: "otherbox", Persistence: "herdr"}
	if otherPeers := srv.resolveConvoPeers(ctx, sessB, cst); len(otherPeers) != 0 {
		t.Fatalf("hostB resolved hostA peer: %+v", otherPeers)
	}

	// Sender exits (full scan no longer includes 530048:193322812):
	// historical message still keeps remembered Corral title, with Alive=false.
	srv.recordLocatedProcs(sessA, []convoProc{
		{pid: 530099, start: 193399999, agent: "claude", sid: "22222222-2222-2222-2222-222222222222", herdrPane: "w2:p1", herdrSocket: sock, cwd: "/srv/other"},
	}, true)
	peersAfterExit := srv.resolveConvoPeers(ctx, sessA, cst)
	p1Exited := peersAfterExit["530048:193322812"]
	if p1Exited.Alive || p1Exited.Title != "corral-work" || p1Exited.HostName != "devbox" {
		t.Fatalf("after exit p1 = %+v, want Alive=false with remembered title", p1Exited)
	}

	// Simulated gateway restart (fresh Server with empty in-memory cache, same SQLite store):
	// historical peer identity is recalled cleanly from SQLite with Alive=false.
	srv2 := &Server{
		store:      st,
		herdrSocks: herdrSockets{path: map[string]string{}},
	}
	peersAfterRestart := srv2.resolveConvoPeers(ctx, sessA, cst)
	p1Restart := peersAfterRestart["530048:193322812"]
	if p1Restart.Alive || p1Restart.Title != "corral-work" || p1Restart.Pane != "w1:p1" || p1Restart.HostName != "devbox" {
		t.Fatalf("after restart p1 = %+v, want persisted snapshot with Alive=false", p1Restart)
	}
}

func TestPeerResolutionPaneReuseAndTmux(t *testing.T) {
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

	sock := "/tmp/herdr-test/work.sock"
	snap := &herdr.Snapshot{
		Workspaces: []herdr.Workspace{{ID: "ws1", Label: "backend"}},
		Agents:     []herdr.Pane{{ID: "w1:p2", WorkspaceID: "ws1", Agent: "claude", Title: "✳ server"}},
	}
	herdrSess := &term.Session{
		ID:           "s-herdr",
		OwnerID:      alice.ID,
		HostID:       hostA.ID,
		HostName:     "box1",
		Persistence:  "herdr",
		HerdrSession: "work",
	}
	tmuxSess := &term.Session{
		ID:          "s-tmux",
		OwnerID:     alice.ID,
		HostID:      hostA.ID,
		HostName:    "box1",
		Persistence: "tmux",
		TmuxSession: "main",
	}

	srv := &Server{
		store:      st,
		herdrSocks: herdrSockets{path: map[string]string{}},
	}
	srv.herdrSocks.set(herdrSess.ID, sock)
	srv.herdrSocks.setServer(herdrSess.ID, "srv-work", alice.ID, func() (*herdr.Snapshot, time.Time) {
		return snap, time.Now()
	}, func() {})

	// Old process 100:500 runs in w1:p2, then pane is reused by 200:900.
	srv.recordLocatedProcs(herdrSess, []convoProc{
		{pid: 100, start: 500, agent: "claude", herdrPane: "w1:p2", herdrSocket: sock},
	}, false)
	srv.recordLocatedProcs(herdrSess, []convoProc{
		{pid: 200, start: 900, agent: "claude", herdrPane: "w1:p2", herdrSocket: sock},
	}, false)
	// Also bind a tmux process 300:1200.
	srv.recordBoundProc(tmuxSess, &convoBind{PID: 300, Start: 1200, Pane: "%1", Agent: "claude", Title: "✳ tmux-task"}, "tmux-convo-title")

	parser := convo.NewClaude()
	parser.Feed([]byte(
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","origin":{"kind":"peer","verifiedPeerPid":100,"verifiedPeerProcStart":"500","name":"ai-old"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-old\">\nold\n</cross-session-message>"}}` + "\n" +
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","origin":{"kind":"peer","verifiedPeerPid":200,"verifiedPeerProcStart":"900","name":"ai-new"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-new\">\nnew\n</cross-session-message>"}}` + "\n" +
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u3","origin":{"kind":"peer","verifiedPeerPid":300,"verifiedPeerProcStart":"1200","name":"ai-tm"},"message":{"role":"user","content":"<cross-session-message from-name=\"ai-tm\">\ntm\n</cross-session-message>"}}` + "\n",
	))
	cst := &convoState{parser: parser}

	peers := srv.resolveConvoPeers(ctx, herdrSess, cst)
	if peers["100:500"].Alive {
		t.Fatalf("reused pane's old process 100:500 must not be Alive: %+v", peers["100:500"])
	}
	if !peers["200:900"].Alive || peers["200:900"].SessionID != herdrSess.ID {
		t.Fatalf("current process 200:900 should be Alive: %+v", peers["200:900"])
	}
	tmPeers := srv.resolveConvoPeers(ctx, tmuxSess, cst)
	if !tmPeers["300:1200"].Alive || tmPeers["300:1200"].Title != "tmux-convo-title" || tmPeers["300:1200"].Key != tmuxAgentKey(hostA.ID, "main") {
		t.Fatalf("tmux peer 300:1200 = %+v", tmPeers["300:1200"])
	}
}

func TestPeerSanitizationAndBgScanCooldown(t *testing.T) {
	if got := cleanPeerPaneTitle("✳ \x1b\x07\u202ehello\x00world"); got != "helloworld" {
		t.Fatalf("cleanPeerPaneTitle = %q, want %q", got, "helloworld")
	}
	if got := cleanPeerPaneTitle("claude --resume foo"); got != "" {
		t.Fatalf("launch cmd should be stripped, got %q", got)
	}

	var r peerRegistry
	scope := peerHostScope{ownerID: 1, hostID: 2}
	ref := []peerProcID{{pid: 42, start: 100}}
	t0 := time.Unix(1_700_000_000, 0)
	if !r.shouldTriggerBgScan(scope, ref, t0) {
		t.Fatal("first miss should trigger bg scan")
	}
	if r.shouldTriggerBgScan(scope, ref, t0.Add(time.Second)) {
		t.Fatal("should not trigger while running")
	}
	r.finishBgScan(scope)
	// Same ref already checked -> should not re-trigger even after cooldown.
	if r.shouldTriggerBgScan(scope, ref, t0.Add(time.Minute)) {
		t.Fatal("already checked ref should not trigger again")
	}
	// New ref within cooldown -> blocked; after cooldown -> allowed.
	ref2 := []peerProcID{{pid: 43, start: 200}}
	if r.shouldTriggerBgScan(scope, ref2, t0.Add(5*time.Second)) {
		t.Fatal("new ref within cooldown should not trigger")
	}
	if !r.shouldTriggerBgScan(scope, ref2, t0.Add(peerMissScanCooldown+time.Second)) {
		t.Fatal("new ref after cooldown should trigger")
	}
}
