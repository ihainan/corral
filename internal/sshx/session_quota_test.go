package sshx

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/testutil"
)

func setMaxSessionChannels(t *testing.T, n int) {
	t.Helper()
	old := MaxSessionChannels
	MaxSessionChannels = n
	t.Cleanup(func() { MaxSessionChannels = old })
}

// Concurrent acquisitions across all four session classes (Terminal, Aux,
// SFTP, Pane) must never exceed MaxSessionChannels or any per-class cap,
// and must return to zero when released (including duplicate release calls).
func TestSessionLedgerConcurrentCapNeverExceeded(t *testing.T) {
	c := &ssh.Client{}
	ctx := context.Background()

	// Phase 1: Deterministic barrier overlap across classes.
	// Hold 1 Terminal + 4 SFTP + 4 Pane = MaxSessionChannels (9) simultaneously
	// behind a barrier, verify all contenders get ErrBusy, then release together.
	type holderSpec struct {
		cls         SessionClass
		interactive bool
	}
	var specs []holderSpec
	specs = append(specs, holderSpec{SessionTerminal, true})
	for range MaxSFTPPerConn {
		specs = append(specs, holderSpec{SessionSFTP, true})
	}
	for range MaxPanePerConn {
		specs = append(specs, holderSpec{SessionPane, true})
	}
	if len(specs) != MaxSessionChannels {
		t.Fatalf("test setup expects %d holders, got %d", MaxSessionChannels, len(specs))
	}

	var readyWG, doneWG sync.WaitGroup
	releaseGate := make(chan struct{})
	var barrierErr atomic.Int32
	for _, sp := range specs {
		readyWG.Add(1)
		doneWG.Add(1)
		go func(sp holderSpec) {
			defer doneWG.Done()
			rel, err := AcquireSession(ctx, c, sp.cls, sp.interactive, 0)
			if err != nil {
				barrierErr.Add(1)
				readyWG.Done()
				return
			}
			readyWG.Done()
			<-releaseGate
			rel()
			rel() // oneShot must ignore duplicate release
		}(sp)
	}
	readyWG.Wait()
	if barrierErr.Load() != 0 {
		close(releaseGate)
		doneWG.Wait()
		t.Fatalf("barrier holders failed %d times", barrierErr.Load())
	}
	if got := SessionInUse(c); got != MaxSessionChannels {
		close(releaseGate)
		doneWG.Wait()
		t.Fatalf("barrier overlap inUse = %d, want %d", got, MaxSessionChannels)
	}
	// While all 9 slots are held simultaneously, concurrent contenders across
	// every class must all be refused with ErrBusy without exceeding the cap.
	var contendWG sync.WaitGroup
	var contendFail atomic.Int32
	for _, cls := range []SessionClass{SessionTerminal, SessionAux, SessionSFTP, SessionPane} {
		for _, interactive := range []bool{false, true} {
			contendWG.Add(1)
			go func(cls SessionClass, interactive bool) {
				defer contendWG.Done()
				if rel, err := AcquireSession(ctx, c, cls, interactive, 0); !errors.Is(err, ErrBusy) {
					if rel != nil {
						rel()
					}
					contendFail.Add(1)
				}
			}(cls, interactive)
		}
	}
	contendWG.Wait()
	close(releaseGate)
	doneWG.Wait()
	if contendFail.Load() != 0 {
		t.Fatalf("contenders breached full cap %d times", contendFail.Load())
	}
	if got := SessionInUse(c); got != 0 {
		t.Fatalf("barrier slots leaked: total=%d", got)
	}

	// Phase 2: High-contention race stress across all classes.
	var wg sync.WaitGroup
	var totalExceeded, classExceeded atomic.Int32
	var curTotal atomic.Int32

	classes := []struct {
		cls         SessionClass
		interactive bool
		limit       int
	}{
		{SessionTerminal, true, MaxTerminalPerConn},
		{SessionAux, false, MaxAuxPerConn},
		{SessionAux, true, MaxAuxPerConn},
		{SessionSFTP, true, MaxSFTPPerConn},
		{SessionPane, true, MaxPanePerConn},
	}

	for _, spec := range classes {
		for range 6 {
			wg.Add(1)
			go func(cls SessionClass, interactive bool, clsLimit int) {
				defer wg.Done()
				for range 300 {
					rel, err := AcquireSession(ctx, c, cls, interactive, 0)
					if err != nil {
						if !errors.Is(err, ErrBusy) {
							totalExceeded.Add(1)
						}
						continue
					}
					n := curTotal.Add(1)
					if int(n) > MaxSessionChannels || SessionInUse(c) > MaxSessionChannels {
						totalExceeded.Add(1)
					}
					if SessionClassInUse(c, cls) > clsLimit {
						classExceeded.Add(1)
					}
					curTotal.Add(-1)
					rel()
					rel() // oneShot must ignore duplicate release
				}
			}(spec.cls, spec.interactive, spec.limit)
		}
	}
	wg.Wait()

	if totalExceeded.Load() != 0 {
		t.Fatalf("total session cap exceeded %d times (limit %d)", totalExceeded.Load(), MaxSessionChannels)
	}
	if classExceeded.Load() != 0 {
		t.Fatalf("per-class cap exceeded %d times", classExceeded.Load())
	}
	if got := SessionInUse(c); got != 0 {
		t.Fatalf("slots leaked: total=%d", got)
	}
}

// Non-terminal classes must always leave TerminalReserve (1) slot free when
// no SessionTerminal is held yet, so the main terminal can always be opened.
func TestSessionLedgerTerminalReserve(t *testing.T) {
	c := &ssh.Client{}
	ctx := context.Background()

	var held []func()
	defer func() {
		for _, r := range held {
			r()
		}
	}()

	// Without a terminal, interactive classes can take at most MaxSessionChannels - TerminalReserve (8).
	// Fill 4 SFTP + 4 Pane = 8.
	for i := range MaxSFTPPerConn {
		rel, err := AcquireSession(ctx, c, SessionSFTP, true, 0)
		if err != nil {
			t.Fatalf("sftp #%d failed: %v", i+1, err)
		}
		held = append(held, rel)
	}
	for i := range MaxPanePerConn {
		rel, err := AcquireSession(ctx, c, SessionPane, true, 0)
		if err != nil {
			t.Fatalf("pane #%d failed: %v", i+1, err)
		}
		held = append(held, rel)
	}
	if got := SessionInUse(c); got != MaxSessionChannels-TerminalReserve {
		t.Fatalf("inUse = %d, want %d", got, MaxSessionChannels-TerminalReserve)
	}

	// An interactive Aux exec must NOT steal the last slot reserved for SessionTerminal.
	if _, err := AcquireSession(ctx, c, SessionAux, true, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("interactive Aux stole terminal reserve: got %v, want ErrBusy", err)
	}

	// The main terminal MUST succeed using its reserved slot.
	relTerm, err := AcquireSession(ctx, c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatalf("SessionTerminal could not claim reserved slot: %v", err)
	}
	held = append(held, relTerm)
	if got := SessionInUse(c); got != MaxSessionChannels {
		t.Fatalf("inUse with terminal = %d, want %d", got, MaxSessionChannels)
	}

	// At full capacity (9/9), everything is refused.
	if _, err := AcquireSession(ctx, c, SessionAux, true, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("extra Aux at full cap: got %v, want ErrBusy", err)
	}
}

// Background scans (non-interactive SessionAux) must leave InteractiveReserve (2)
// headroom slots so user-triggered actions (SFTP, Pane, interactive Exec) are
// never starved when the connection approaches MaxSessionChannels.
func TestSessionLedgerInteractiveReserveNotStarvedByBackground(t *testing.T) {
	c := &ssh.Client{}
	ctx := context.Background()

	var held []func()
	defer func() {
		for _, r := range held {
			r()
		}
	}()

	// Hold 1 Terminal + 4 Pane + 1 SFTP = 6 slots (3 remaining out of 9).
	relTerm, err := AcquireSession(ctx, c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatalf("terminal: %v", err)
	}
	held = append(held, relTerm)

	for i := range MaxPanePerConn {
		rel, err := AcquireSession(ctx, c, SessionPane, true, 0)
		if err != nil {
			t.Fatalf("pane #%d: %v", i+1, err)
		}
		held = append(held, rel)
	}
	relSFTP, err := AcquireSession(ctx, c, SessionSFTP, true, 0)
	if err != nil {
		t.Fatalf("sftp #1: %v", err)
	}
	held = append(held, relSFTP)

	// One background scan fits (6 -> 7, still leaving InteractiveReserve=2 free).
	relBg1, err := AcquireSession(ctx, c, SessionAux, false, 0)
	if err != nil {
		t.Fatalf("background #1 should fit: %v", err)
	}
	held = append(held, relBg1)

	// Second background scan MUST be refused (would leave only 1 free < InteractiveReserve=2).
	if _, err := AcquireSession(ctx, c, SessionAux, false, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("background #2 stole interactive reserve: got %v, want ErrBusy", err)
	}

	// Meanwhile, interactive SFTP and interactive Aux MUST both succeed into the 2 reserved slots.
	relSFTP2, err := AcquireSession(ctx, c, SessionSFTP, true, 0)
	if err != nil {
		t.Fatalf("interactive SFTP into reserve failed: %v", err)
	}
	held = append(held, relSFTP2)

	relIntAux, err := AcquireSession(ctx, c, SessionAux, true, 0)
	if err != nil {
		t.Fatalf("interactive Aux into reserve failed: %v", err)
	}
	held = append(held, relIntAux)

	if got := SessionInUse(c); got != MaxSessionChannels {
		t.Fatalf("inUse = %d, want %d", got, MaxSessionChannels)
	}
}

// Interactive callers with await > 0 wake promptly when a slot is released,
// return ErrBusy when the queue window expires, and honour context cancellation.
// Background callers (await == 0) return ErrBusy immediately.
func TestSessionLedgerAwaitWakesOnReleaseAndTimesOut(t *testing.T) {
	setMaxSessionChannels(t, 3)
	c := &ssh.Client{}
	ctx := context.Background()

	relTerm, err := AcquireSession(ctx, c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer relTerm()
	relPane1, err := AcquireSession(ctx, c, SessionPane, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	relPane2, err := AcquireSession(ctx, c, SessionPane, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer relPane2()

	// Background call fails immediately with ErrBusy.
	start := time.Now()
	if _, err := AcquireSession(ctx, c, SessionAux, false, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("background acquire: got %v, want ErrBusy", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("background acquire waited %v", d)
	}

	// Interactive call times out with ErrBusy when nothing is released.
	start = time.Now()
	if _, err := AcquireSession(ctx, c, SessionSFTP, true, 80*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("timed-out interactive acquire: got %v, want ErrBusy", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("interactive acquire returned too early: %v", d)
	}

	// Context cancellation unblocks waiting promptly.
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	if _, err := AcquireSession(cctx, c, SessionSFTP, true, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: got %v, want context.Canceled", err)
	}

	// Releasing a slot wakes the queued interactive caller immediately.
	go func() {
		time.Sleep(50 * time.Millisecond)
		relPane1()
	}()
	start = time.Now()
	relSFTP, err := AcquireSession(ctx, c, SessionSFTP, true, 2*time.Second)
	if err != nil {
		t.Fatalf("queued interactive acquire after release failed: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("woke too slowly after release: %v", d)
	}
	relSFTP()
}

// A session channel whose close is never answered by the peer keeps its ledger
// slot until the peer actually replies SSH_MSG_CHANNEL_CLOSE, blocking other
// classes once the ledger is full.
//
// Because golang.org/x/crypto/ssh's server mux auto-replies CHANNEL_CLOSE as
// soon as it reads a client's CHANNEL_CLOSE, this test interposes a TCP relay
// (freezer) and freezes client->server traffic after exec is accepted so the
// server truly never receives or replies to CHANNEL_CLOSE.
func TestSessionChannelNotReleasedUntilReallyEnded(t *testing.T) {
	setMaxSessionChannels(t, 4)
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var fr *freezer
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "session" {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go func() {
			for r := range reqs {
				if r.Type == "exec" {
					// Freeze client->server BEFORE replying so the client's
					// subsequent CHANNEL_CLOSE on cancel never reaches the
					// server and the server never auto-replies CHANNEL_CLOSE.
					fr.up.Store(true)
					r.Reply(true, nil)
					go io.Copy(io.Discard, ch)
				} else if r.WantReply {
					r.Reply(false, nil)
				}
			}
		}()
		return true
	})

	fr = newFreezer(t, srv.Addr.String())
	c := dialVia(t, fr)

	// Hold 1 terminal slot + 1 SFTP slot (2/4 used).
	relTerm, err := AcquireSession(context.Background(), c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer relTerm()
	relSFTP1, err := AcquireSession(context.Background(), c, SessionSFTP, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer relSFTP1()

	// Start an interactive Exec that times out while client->server is frozen.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := Exec(ctx, c, "hang", 1<<20, ExecOptions{Interactive: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck exec: got %v, want DeadlineExceeded", err)
	}

	// Wait well beyond a local loopback round-trip: if the server had auto-replied
	// CHANNEL_CLOSE, the slot would already be back to 0.
	time.Sleep(150 * time.Millisecond)

	// The Exec returned, but its channel has NOT really ended on the host.
	// The ledger MUST still count it (Terminal 1 + SFTP 1 + Aux 1 = 3/4).
	if got := SessionInUse(c); got != 3 || SessionClassInUse(c, SessionAux) != 1 {
		t.Fatalf("ledger released stuck exec too early: total=%d aux=%d", got, SessionClassInUse(c, SessionAux))
	}

	// Only 1 slot remains (4 - 3 = 1): second SFTP succeeds, Pane fails with ErrBusy.
	relSFTP2, err := AcquireSession(context.Background(), c, SessionSFTP, true, 0)
	if err != nil {
		t.Fatalf("second SFTP should fit in remaining slot: %v", err)
	}
	defer relSFTP2()
	if _, err := AcquireSession(context.Background(), c, SessionPane, true, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("pane should be rejected while stuck exec holds its slot: %v", err)
	}

	// Unfreeze client->server: the client's CHANNEL_CLOSE reaches the server,
	// the server replies CHANNEL_CLOSE, and the stuck exec's slot returns.
	fr.up.Store(false)
	eventually(t, "stuck exec never released its ledger slot after peer CHANNEL_CLOSE", func() bool {
		return SessionClassInUse(c, SessionAux) == 0
	})
	// Now SessionPane can acquire the freed slot.
	relPane, err := AcquireSession(context.Background(), c, SessionPane, true, 0)
	if err != nil {
		t.Fatalf("pane after stuck exec ended: %v", err)
	}
	relPane()
}

// AwaitSlot alone must NOT grant interactive priority: a background Exec with
// Interactive: false and AwaitSlot > 0 must still respect InteractiveReserve
// (refusing/timing out while only reserve slots remain, and waking only once a
// non-reserve slot becomes free).
func TestExecAwaitSlotDoesNotImplyInteractive(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		return func(ch ssh.Channel) {
			_, _ = ch.Write([]byte("ok"))
			_ = ch.CloseWrite()
			_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
		}
	})
	c := dialTest(t, srv)
	ctx := context.Background()

	// Default MaxSessionChannels = 9, TerminalReserve = 1, InteractiveReserve = 2.
	// Hold 1 Terminal + 4 Pane + 2 SFTP = 7/9 -> only the 2 InteractiveReserve slots remain.
	var held []func()
	defer func() {
		for _, r := range held {
			r()
		}
	}()
	relTerm, err := AcquireSession(ctx, c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	held = append(held, relTerm)
	for range MaxPanePerConn {
		r, err := AcquireSession(ctx, c, SessionPane, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	relSFTP1, err := AcquireSession(ctx, c, SessionSFTP, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	relSFTP2, err := AcquireSession(ctx, c, SessionSFTP, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	held = append(held, relSFTP2)

	// Background Exec with AwaitSlot > 0 MUST NOT use the 2 InteractiveReserve slots.
	if _, err := Exec(ctx, c, "bg-wait", 1<<20, ExecOptions{AwaitSlot: 80 * time.Millisecond}); !errors.Is(err, ErrBusy) {
		t.Fatalf("non-interactive Exec with AwaitSlot stole interactive reserve: got %v, want ErrBusy", err)
	}

	// Interactive Exec MUST succeed immediately into InteractiveReserve.
	res, err := Exec(ctx, c, "user-click", 1<<20, ExecOptions{Interactive: true})
	if err != nil || string(res.Out) != "ok" {
		t.Fatalf("interactive Exec into reserve failed: out=%q err=%v", res.Out, err)
	}

	// When a slot is freed (7 -> 6, leaving 1 non-reserve slot), a background
	// Exec waiting on AwaitSlot wakes and succeeds without touching reserve.
	go func() {
		time.Sleep(40 * time.Millisecond)
		relSFTP1()
	}()
	res, err = Exec(ctx, c, "bg-wake", 1<<20, ExecOptions{AwaitSlot: 2 * time.Second})
	if err != nil || string(res.Out) != "ok" {
		t.Fatalf("non-interactive Exec waiting on AwaitSlot failed after non-reserve slot freed: out=%q err=%v", res.Out, err)
	}
}

// Long-running background Aux execs must be capped below MaxAuxPerConn (at 3
// out of 4) so the 4th background exec fails immediately with ErrBusy while an
// interactive exec can still claim the reserved in-class Aux slot.
func TestSessionLedgerBackgroundAuxLeavesInteractiveSlot(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	releaseBg := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseBg:
		default:
			close(releaseBg)
		}
	})
	bgStarted := make(chan struct{}, MaxAuxPerConn)
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd == "bg-hold" {
			return func(ch ssh.Channel) {
				bgStarted <- struct{}{}
				<-releaseBg
				_ = ch.CloseWrite()
				_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return func(ch ssh.Channel) {
			_, _ = ch.Write([]byte("interactive-ok"))
			_ = ch.CloseWrite()
			_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
		}
	})

	c := dialTest(t, srv)
	ctx := context.Background()

	// Hold 1 main terminal slot (total 1/9).
	relTerm, err := AcquireSession(ctx, c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer relTerm()

	// Start 3 long-running background execs (MaxAuxPerConn - AuxInteractiveReserve = 3).
	bgCap := MaxAuxPerConn - AuxInteractiveReserve
	if bgCap != 3 {
		t.Fatalf("expected background Aux cap 3, got %d (MaxAux=%d, AuxReserve=%d)", bgCap, MaxAuxPerConn, AuxInteractiveReserve)
	}
	var bgWG sync.WaitGroup
	for range bgCap {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			_, _ = Output(ctx, c, "bg-hold", 1<<20)
		}()
	}
	for range bgCap {
		select {
		case <-bgStarted:
		case <-time.After(3 * time.Second):
			t.Fatal("background exec did not start in time")
		}
	}
	if got := SessionClassInUse(c, SessionAux); got != 3 {
		t.Fatalf("SessionAux inUse = %d, want 3", got)
	}

	// A 4th background exec MUST be refused immediately with ErrBusy.
	start := time.Now()
	if _, err := Output(ctx, c, "bg-hold", 1<<20); !errors.Is(err, ErrBusy) {
		t.Fatalf("4th background exec: got %v, want ErrBusy", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("4th background exec did not fail immediately: waited %v", d)
	}

	// An interactive exec MUST still succeed into the reserved 4th Aux slot.
	res, err := Exec(ctx, c, "user-action", 1<<20, ExecOptions{Interactive: true})
	if err != nil || string(res.Out) != "interactive-ok" {
		t.Fatalf("interactive exec blocked by 3 background Aux execs: out=%q err=%v", res.Out, err)
	}

	close(releaseBg)
	bgWG.Wait()
	eventually(t, "background Aux slots did not release", func() bool {
		return SessionClassInUse(c, SessionAux) == 0
	})
}

// Forwarded channels (direct-tcpip and direct-streamlocal, including Aux and
// Watch dials) do not consume OpenSSH's MaxSessions and must NOT be counted
// in the session ledger or blocked when the session ledger is full.
func TestForwardedChannelsNotCountedInSessionLedger(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != "direct-tcpip" && nc.ChannelType() != "direct-streamlocal@openssh.com" {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			io.Copy(io.Discard, ch)
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fill the entire session ledger (1 Terminal + 4 SFTP + 4 Pane = 9/9).
	var held []func()
	relTerm, err := AcquireSession(ctx, c, SessionTerminal, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	held = append(held, relTerm)
	for range MaxSFTPPerConn {
		r, err := AcquireSession(ctx, c, SessionSFTP, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	for range MaxPanePerConn {
		r, err := AcquireSession(ctx, c, SessionPane, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	if SessionInUse(c) != MaxSessionChannels {
		t.Fatalf("expected full session ledger (%d), got %d", MaxSessionChannels, SessionInUse(c))
	}

	// Opening direct-tcpip, direct-streamlocal (Aux), and direct-streamlocal (Watch)
	// must all succeed while the session ledger is completely full, and must not
	// change SessionInUse(c).
	tcpConn, err := DialVia(ctx, c, "tcp", "127.0.0.1:80", DialOptions{})
	if err != nil {
		t.Fatalf("direct-tcpip blocked by full session ledger: %v", err)
	}
	defer tcpConn.Close()

	auxUnix, err := DialVia(ctx, c, "unix", "/run/herdr.sock", DialOptions{Aux: true})
	if err != nil {
		t.Fatalf("direct-streamlocal Aux blocked by full session ledger: %v", err)
	}
	defer auxUnix.Close()

	watchUnix, err := DialVia(ctx, c, "unix", "/run/watch.sock", DialOptions{Watch: true})
	if err != nil {
		t.Fatalf("direct-streamlocal Watch blocked by full session ledger: %v", err)
	}
	defer watchUnix.Close()

	if got := SessionInUse(c); got != MaxSessionChannels {
		t.Fatalf("forwarded channels altered session ledger: got %d, want %d", got, MaxSessionChannels)
	}

	// Release all session slots while forwarded channels remain open: session
	// ledger must drop to 0.
	for _, r := range held {
		r()
	}
	if got := SessionInUse(c); got != 0 {
		t.Fatalf("forwarded channels counted in session ledger: got %d, want 0", got)
	}
}
