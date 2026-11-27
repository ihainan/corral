package gw

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

type fakeWatchDaemon struct {
	sockPath string
	ln       net.Listener

	mu             sync.Mutex
	epoch          string
	watchCalls     int
	unwatchCalls   int
	lastWatchLogs  []string
	watchErr       *proto.RPCError
	watchMode      string
	healthCaps     map[string]string
	conns          []net.Conn
	watchCallCh    chan []string
	unwatchCallCh  chan struct{}
	unwatchBlockCh chan struct{} // when non-nil, MethodAgentUnwatch waits for close before replying
	nextSeq        uint64
	activeWatching bool
}

func startFakeWatchDaemon(t *testing.T, epoch string) *fakeWatchDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gw-watch-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "corrald.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeWatchDaemon{
		sockPath:      sock,
		ln:            ln,
		epoch:         epoch,
		watchMode:     "inotify",
		healthCaps:    map[string]string{"locate": "ok", "tails": "ok", "watch": "ok"},
		watchCallCh:   make(chan []string, 32),
		unwatchCallCh: make(chan struct{}, 32),
	}
	t.Cleanup(func() {
		_ = ln.Close()
		d.mu.Lock()
		for _, c := range d.conns {
			_ = c.Close()
		}
		d.mu.Unlock()
	})

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			d.conns = append(d.conns, c)
			d.mu.Unlock()
			go d.serveConn(c)
		}
	}()
	return d
}

func (d *fakeWatchDaemon) serveConn(c net.Conn) {
	defer c.Close()
	for {
		var req proto.Request
		if err := proto.ReadJSON(c, &req); err != nil {
			return
		}
		switch req.Method {
		case proto.MethodDaemonHello:
			d.mu.Lock()
			ep := d.epoch
			d.mu.Unlock()
			res, _ := json.Marshal(proto.HelloResult{
				Version:  "0.3.1",
				Release:  1000,
				Protocol: proto.Protocol,
				Epoch:    ep,
				PID:      os.Getpid(),
				Manifest: proto.DefaultManifest(),
				State:    proto.StateRunning,
			})
			_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
		case proto.MethodDaemonResume:
			d.mu.Lock()
			ep := d.epoch
			d.mu.Unlock()
			res, _ := json.Marshal(proto.ResumeResult{Epoch: ep})
			_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
		case proto.MethodDaemonHealth:
			d.mu.Lock()
			caps := make(map[string]string, len(d.healthCaps))
			for k, v := range d.healthCaps {
				caps[k] = v
			}
			d.mu.Unlock()
			res, _ := json.Marshal(proto.HealthResult{Caps: caps, PID: os.Getpid()})
			_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
		case proto.MethodAgentWatch:
			var p proto.AgentWatchParams
			_ = json.Unmarshal(req.Params, &p)
			d.mu.Lock()
			d.watchCalls++
			d.lastWatchLogs = append([]string(nil), p.Logs...)
			werr := d.watchErr
			mode := d.watchMode
			ep := d.epoch
			if werr == nil {
				d.activeWatching = true
			}
			d.mu.Unlock()
			select {
			case d.watchCallCh <- p.Logs:
			default:
			}
			if werr != nil {
				_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Error: werr})
				continue
			}
			res, _ := json.Marshal(proto.AgentWatchResult{
				Watching: true,
				Mode:     mode,
				Epoch:    ep,
			})
			_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
		case proto.MethodAgentUnwatch:
			d.mu.Lock()
			d.unwatchCalls++
			d.activeWatching = false
			ep := d.epoch
			blockCh := d.unwatchBlockCh
			d.mu.Unlock()
			select {
			case d.unwatchCallCh <- struct{}{}:
			default:
			}
			if blockCh != nil {
				<-blockCh
			}
			res, _ := json.Marshal(proto.AgentWatchResult{
				Watching: false,
				Mode:     "idle",
				Epoch:    ep,
			})
			_ = proto.WriteJSON(c, proto.Response{ID: req.ID, Result: res})
		}
	}
}

func (d *fakeWatchDaemon) emitEvent(eventName string, data any) {
	raw, _ := json.Marshal(data)
	d.mu.Lock()
	d.nextSeq++
	ev := proto.Event{
		Event: eventName,
		Epoch: d.epoch,
		Seq:   d.nextSeq,
		Data:  raw,
	}
	conns := append([]net.Conn(nil), d.conns...)
	d.mu.Unlock()
	for _, c := range conns {
		_ = proto.WriteJSON(c, ev)
	}
}

func (d *fakeWatchDaemon) counts() (watches, unwatches int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.watchCalls, d.unwatchCalls
}

type watchTestSSHProvider struct {
	client *ssh.Client
	fp     string
	user   string
}

func (p *watchTestSSHProvider) AcquireSSH(context.Context, int64, int64) (*ssh.Client, func(), string, string, error) {
	return p.client, func() {}, p.fp, p.user, nil
}
func (p *watchTestSSHProvider) HostBinding(context.Context, int64, int64) (string, string, error) {
	return p.user, p.fp, nil
}
func (p *watchTestSSHProvider) UserLabel(context.Context, int64) string { return "alice" }

func setupWatchManagerHarness(t *testing.T, daemon *fakeWatchDaemon, unsubDelay time.Duration, healthInterval time.Duration) (*Manager, *store.Store, int64, int64, *ssh.Client, string) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "Alice", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	sshSrv := testutil.StartSSHServer(t, "alice", "pass", home)
	sshClient, err := ssh.Dial("tcp", sshSrv.Addr.String(), &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.Password("pass")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sshClient.Close() })

	h, err := st.CreateHost(ctx, &store.Host{
		OwnerID:  u.ID,
		Name:     "watch-box",
		Address:  "127.0.0.1",
		Port:     sshSrv.Addr.Port,
		Username: "alice",
		Auth:     "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	fp := "SHA256:watch-test-fp"
	mb, _ := proto.MarshalManifest(proto.DefaultManifest())
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           h.ID,
		State:            StateRunning,
		BoundFingerprint: fp,
		BoundUsername:    "alice",
		BoundGateway:     "gw-watch.local",
		ManifestJSON:     string(mb),
		Version:          "0.3.1",
		Release:          1000,
		MachineID:        "aabbccddeeff",
		SocketPath:       daemon.sockPath,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.RecordSSHVerified(ctx, h.ID, fp, "alice")

	mgr := NewManager(ManagerConfig{
		Store:                 st,
		Dist:                  &Dist{Version: "0.3.1", Release: 1000, Manifest: proto.DefaultManifest(), ManifestHash: "mh"},
		GatewayID:             "gw-watch.local",
		SSH:                   &watchTestSSHProvider{client: sshClient, fp: fp, user: "alice"},
		WatchUnsubscribeDelay: unsubDelay,
		HealthInterval:        healthInterval,
		RestartBackoffs:       []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond},
		ProbeOverride: func(context.Context, *ssh.Client) (*HostProbe, error) {
			return &HostProbe{
				OS:           "Linux",
				Machine:      "x86_64",
				Arch:         "amd64",
				Home:         home,
				HomeWritable: true,
				HasSymlink:   true,
				MachineID:    "aabbccddeeff",
				SocketPath:   daemon.sockPath,
				StateDir:     filepath.Join(home, ".local", "state", "corral", "corrald", "aabbccddeeff"),
			}, nil
		},
	})
	t.Cleanup(func() { mgr.Close() })

	mgr.OnHostConnected(ctx, u.ID, h.ID, "alice", fp, "alice", sshClient)
	if mgr.SharedConnCount() != 1 {
		t.Fatalf("expected 1 active sharedConn, got %d", mgr.SharedConnCount())
	}
	return mgr, st, u.ID, h.ID, sshClient, fp
}

func waitCond(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestWatchLifecycleSubscribeGraceCancelAndUnsubscribe(t *testing.T) {
	d := startFakeWatchDaemon(t, "ep-1")
	mgr, _, uid, hid, _, _ := setupWatchManagerHarness(t, d, 120*time.Millisecond, time.Hour)

	var coverNotifies atomic.Int32
	mgr.SetWatchCallbacks(nil, func(hostIDs []int64) {
		coverNotifies.Add(1)
	})

	// No browser watching -> MUST NOT subscribe agent.watch.
	time.Sleep(40 * time.Millisecond)
	if w, _ := d.counts(); w != 0 {
		t.Fatalf("expected 0 watch RPCs before any browser arrives, got %d", w)
	}
	if mgr.IsWatchCovered(uid, hid) {
		t.Fatal("host must not be covered before watch subscription")
	}

	// First browser arrives -> subscribes immediately.
	w1 := new(int)
	mgr.SetWatcher(w1, uid, []int64{hid})
	select {
	case <-d.watchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first agent.watch RPC")
	}
	waitCond(t, 2*time.Second, "host covered", func() bool { return mgr.IsWatchCovered(uid, hid) })
	if coverNotifies.Load() == 0 {
		t.Fatal("expected coverage callback after watch subscribed")
	}

	// Second browser arrives -> no duplicate agent.watch RPC.
	w2 := new(int)
	mgr.SetWatcher(w2, uid, []int64{hid})
	time.Sleep(40 * time.Millisecond)
	if w, u := d.counts(); w != 1 || u != 0 {
		t.Fatalf("expected (watch=1, unwatch=0) with 2 browsers, got (%d, %d)", w, u)
	}

	// First browser leaves while second still watching -> no unwatch.
	mgr.RemoveWatcher(w1)
	time.Sleep(180 * time.Millisecond)
	if _, u := d.counts(); u != 0 {
		t.Fatalf("expected 0 unwatch RPCs while second browser still watches, got %d", u)
	}

	// Second (last) browser leaves -> stays subscribed during grace period,
	// and a third browser arriving before grace expires cancels unsubscribe!
	mgr.RemoveWatcher(w2)
	time.Sleep(40 * time.Millisecond)
	if _, u := d.counts(); u != 0 {
		t.Fatalf("unwatched too early before grace delay: %d", u)
	}
	w3 := new(int)
	mgr.SetWatcher(w3, uid, []int64{hid})
	time.Sleep(160 * time.Millisecond)
	if w, u := d.counts(); w != 1 || u != 0 {
		t.Fatalf("expected grace timer cancelled when w3 joined, got watch=%d unwatch=%d", w, u)
	}

	// Last browser leaves and stays gone past grace period -> sends agent.unwatch!
	mgr.RemoveWatcher(w3)
	select {
	case <-d.unwatchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent.unwatch after grace delay")
	}
	waitCond(t, 2*time.Second, "host uncovered after unwatch", func() bool { return !mgr.IsWatchCovered(uid, hid) })
}

func TestWatchResubscribesOnHandoffReconnectAndUnexpectedDrop(t *testing.T) {
	d := startFakeWatchDaemon(t, "ep-1")
	mgr, _, uid, hid, _, fp := setupWatchManagerHarness(t, d, 5*time.Second, time.Hour)

	gotChanged := make(chan proto.AgentChangedEventData, 4)
	mgr.SetWatchCallbacks(func(hostIDs []int64, ev proto.AgentChangedEventData) {
		gotChanged <- ev
	}, nil)

	w1 := new(int)
	mgr.SetWatcher(w1, uid, []int64{hid})
	select {
	case <-d.watchCallCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial watch")
	}
	waitCond(t, 5*time.Second, "initial covered", func() bool { return mgr.IsWatchCovered(uid, hid) })

	sc := mgr.getSharedConn(accountKey{Fingerprint: fp, Username: "alice"})
	if sc == nil {
		t.Fatal("expected sharedConn")
	}
	sc.mu.Lock()
	firstClient := sc.client
	sc.mu.Unlock()

	// 1. Simulate hot-update handoff via daemon.reconnect event.
	d.mu.Lock()
	d.epoch = "ep-2"
	d.mu.Unlock()
	d.emitEvent(proto.EventDaemonReconnect, proto.ReconnectEventData{Reason: "handoff", NewRelease: 2000})

	select {
	case <-d.watchCallCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agent.watch re-subscribe after daemon.reconnect")
	}
	// Wait until reconnectSharedConn has actually swapped sc.client onto the
	// new ep-2 connection and cleared sc.reconnecting (not just the old client
	// still reporting covered before the swap finishes).
	waitCond(t, 5*time.Second, "handoff switched to ep-2 client and covered", func() bool {
		sc.mu.Lock()
		switched := sc.client != nil && sc.client != firstClient && !sc.reconnecting && sc.epoch == "ep-2"
		sc.mu.Unlock()
		return switched && mgr.IsWatchCovered(uid, hid)
	})

	sc.mu.Lock()
	secondClient := sc.client
	sc.mu.Unlock()

	// Verify agent.changed event arrives on the new connection.
	d.emitEvent(proto.EventAgentChanged, proto.AgentChangedEventData{
		ChangedLogs: []string{"/tmp/fake.jsonl"},
	})
	select {
	case ev := <-gotChanged:
		if len(ev.ChangedLogs) != 1 || ev.ChangedLogs[0] != "/tmp/fake.jsonl" {
			t.Fatalf("unexpected changed event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agent.changed after handoff")
	}

	// 2. Simulate unexpected connection drop -> automatic reconnect & re-subscribe.
	if !mgr.ForceDropConn(fp, "alice") {
		t.Fatal("ForceDropConn failed")
	}
	select {
	case <-d.watchCallCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for agent.watch re-subscribe after unexpected disconnect")
	}
	waitCond(t, 5*time.Second, "reconnected to new client and covered", func() bool {
		sc.mu.Lock()
		reconnected := sc.client != nil && sc.client != secondClient && !sc.reconnecting
		sc.mu.Unlock()
		return reconnected && mgr.IsWatchCovered(uid, hid)
	})
}

func TestWatchDegradationRemovesCoverage(t *testing.T) {
	// Case A: agent.watch RPC fails -> immediately leaves coverage.
	t.Run("WatchRPCError", func(t *testing.T) {
		d := startFakeWatchDaemon(t, "ep-err")
		d.mu.Lock()
		d.watchErr = &proto.RPCError{Code: proto.ErrInternal, Message: "inotify failed"}
		d.mu.Unlock()

		mgr, _, uid, hid, _, _ := setupWatchManagerHarness(t, d, time.Second, time.Hour)
		coverCh := make(chan []int64, 4)
		mgr.SetWatchCallbacks(nil, func(hostIDs []int64) {
			coverCh <- hostIDs
		})

		w1 := new(int)
		mgr.SetWatcher(w1, uid, []int64{hid})
		select {
		case <-d.watchCallCh:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for watch attempt")
		}
		select {
		case <-coverCh:
		case <-time.After(2 * time.Second):
			t.Fatal("expected coverage notification on watch RPC failure")
		}
		if mgr.IsWatchCovered(uid, hid) {
			t.Fatal("host must not be covered when agent.watch fails")
		}
	})

	// Case B: daemon.health later reports watch degraded -> immediately leaves coverage.
	t.Run("HealthWatchUnavailable", func(t *testing.T) {
		d := startFakeWatchDaemon(t, "ep-health")
		mgr, _, uid, hid, _, _ := setupWatchManagerHarness(t, d, time.Second, 50*time.Millisecond)

		coverCh := make(chan []int64, 8)
		mgr.SetWatchCallbacks(nil, func(hostIDs []int64) {
			coverCh <- hostIDs
		})

		w1 := new(int)
		mgr.SetWatcher(w1, uid, []int64{hid})
		select {
		case <-d.watchCallCh:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for initial watch")
		}
		waitCond(t, 2*time.Second, "initially covered", func() bool { return mgr.IsWatchCovered(uid, hid) })

		// Degrade watch capability in daemon.health.
		d.mu.Lock()
		d.healthCaps["watch"] = "degraded:poll_fallback(inotify_error)"
		d.mu.Unlock()

		waitCond(t, 2*time.Second, "uncovered after health degrades watch", func() bool { return !mgr.IsWatchCovered(uid, hid) })
	})
}

// Regression A.1: Detaching the last host on a sharedConn (rebind, host edit,
// delete) must still notify OnCoverageChanged for that host ID.
func TestWatchDetachLastHostNotifiesCoverage(t *testing.T) {
	d := startFakeWatchDaemon(t, "ep-detach")
	mgr, _, uid, hid, _, _ := setupWatchManagerHarness(t, d, time.Second, time.Hour)

	coverCh := make(chan []int64, 16)
	mgr.SetWatchCallbacks(nil, func(hostIDs []int64) {
		coverCh <- append([]int64(nil), hostIDs...)
	})

	w1 := new(int)
	mgr.SetWatcher(w1, uid, []int64{hid})
	select {
	case <-d.watchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial watch")
	}
	waitCond(t, 2*time.Second, "initially covered", func() bool { return mgr.IsWatchCovered(uid, hid) })

	// Drain earlier notifications before detaching.
	for len(coverCh) > 0 {
		<-coverCh
	}

	mgr.DetachHost(hid)

	if mgr.IsWatchCovered(uid, hid) {
		t.Fatal("detached host must not be covered")
	}
	select {
	case ids := <-coverCh:
		found := false
		for _, id := range ids {
			if id == hid {
				found = true
			}
		}
		if !found {
			t.Fatalf("coverage notification %v did not include detached host %d", ids, hid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected coverage invalidation callback when detaching last host")
	}
}

// Regression A.2: If the old connection drops during reconnect/handoff AND the
// replacement connection fails to dial, coverage invalidation must still be
// broadcast so browsers never stay stuck on slow polling.
func TestWatchFailedReconnectAfterOldConnDropNotifiesCoverage(t *testing.T) {
	d := startFakeWatchDaemon(t, "ep-reconn-fail")
	mgr, _, uid, hid, _, fp := setupWatchManagerHarness(t, d, time.Second, time.Hour)

	coverCh := make(chan []int64, 16)
	mgr.SetWatchCallbacks(nil, func(hostIDs []int64) {
		coverCh <- append([]int64(nil), hostIDs...)
	})

	w1 := new(int)
	mgr.SetWatcher(w1, uid, []int64{hid})
	select {
	case <-d.watchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial watch")
	}
	waitCond(t, 2*time.Second, "initially covered", func() bool { return mgr.IsWatchCovered(uid, hid) })
	for len(coverCh) > 0 {
		<-coverCh
	}

	// Shut down the daemon listener so reconnect dials fail, emit daemon.reconnect
	// (setting sc.reconnecting=true), and drop the existing connection.
	_ = d.ln.Close()
	d.emitEvent(proto.EventDaemonReconnect, proto.ReconnectEventData{Reason: "handoff", NewRelease: 2000})
	// Give handleDaemonEvent a brief moment to start reconnecting, then close old conn.
	time.Sleep(15 * time.Millisecond)
	mgr.ForceDropConn(fp, "alice")

	waitCond(t, 3*time.Second, "uncovered after failed reconnect", func() bool { return !mgr.IsWatchCovered(uid, hid) })
	select {
	case ids := <-coverCh:
		found := false
		for _, id := range ids {
			if id == hid {
				found = true
			}
		}
		if !found {
			t.Fatalf("coverage notification %v missing host %d", ids, hid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected coverage invalidation callback when old conn dropped during failing reconnect")
	}
}

// Regression A.3: When corrald returns Watching=true, Mode="poll", the host is
// not in healthy cover, yet the gateway must remember the remote subscription
// is active and send agent.unwatch after the last browser leaves.
func TestWatchPollModeStillUnsubscribesAfterLastBrowserLeaves(t *testing.T) {
	d := startFakeWatchDaemon(t, "ep-poll")
	d.mu.Lock()
	d.watchMode = "poll"
	d.mu.Unlock()

	mgr, _, uid, hid, _, _ := setupWatchManagerHarness(t, d, 80*time.Millisecond, time.Hour)

	w1 := new(int)
	mgr.SetWatcher(w1, uid, []int64{hid})
	select {
	case <-d.watchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent.watch")
	}
	waitCond(t, 2*time.Second, "remote watch active in poll mode", func() bool {
		return mgr.WatchSubscribedForHost(hid)
	})
	if mgr.IsWatchCovered(uid, hid) {
		t.Fatal("poll-degraded host must NOT be considered in healthy cover")
	}

	// While browser stays connected, remote subscription must not unwatch prematurely.
	time.Sleep(130 * time.Millisecond)
	if _, u := d.counts(); u != 0 {
		t.Fatalf("expected 0 unwatch calls while browser still watching, got %d", u)
	}

	// Once last browser leaves, grace period expires and agent.unwatch MUST be sent.
	mgr.RemoveWatcher(w1)
	select {
	case <-d.unwatchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent.unwatch for poll-mode subscription")
	}
	waitCond(t, 2*time.Second, "remote watch cleared after unwatch", func() bool {
		return !mgr.WatchSubscribedForHost(hid)
	})
}

// Regression A.7: If a new browser joins while a grace-period agent.unwatch RPC
// is in flight, the gateway must re-subscribe immediately after Unwatch returns.
func TestWatchUnsubscribeRaceWithNewWatcherReSubscribes(t *testing.T) {
	d := startFakeWatchDaemon(t, "ep-race")
	unwatchGate := make(chan struct{})
	d.mu.Lock()
	d.unwatchBlockCh = unwatchGate
	d.mu.Unlock()

	mgr, _, uid, hid, _, _ := setupWatchManagerHarness(t, d, 60*time.Millisecond, time.Hour)

	w1 := new(int)
	mgr.SetWatcher(w1, uid, []int64{hid})
	select {
	case <-d.watchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial watch")
	}
	waitCond(t, 2*time.Second, "initially covered", func() bool { return mgr.IsWatchCovered(uid, hid) })

	// Last browser leaves -> wait until agent.unwatch is actually in flight on daemon.
	mgr.RemoveWatcher(w1)
	select {
	case <-d.unwatchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight agent.unwatch")
	}

	// While Unwatch is blocked in flight, a new browser joins!
	w2 := new(int)
	mgr.SetWatcher(w2, uid, []int64{hid})

	// Unblock Unwatch -> gateway must immediately re-subscribe for w2.
	close(unwatchGate)
	select {
	case <-d.watchCallCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for re-subscribe after in-flight unwatch raced with new watcher")
	}
	waitCond(t, 2*time.Second, "w2 covered after unwatch race", func() bool { return mgr.IsWatchCovered(uid, hid) })
}
