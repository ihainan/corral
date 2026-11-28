package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/store"
)

// TestHelperCorralServer runs corral's real server startup path (run) inside a
// child process when CORRAL_HELPER_SERVER=1 is set. Because it invokes run(log)
// directly, it exercises cross-process flock, startup sweep, SIGTERM shutdown,
// and SIGKILL crash recovery without changing run's signature.
func TestHelperCorralServer(t *testing.T) {
	if os.Getenv("CORRAL_HELPER_SERVER") != "1" {
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("corral stopped", "err", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func randomMasterKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func cleanServerEnv(dataDir, listenAddr, masterKey string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CORRAL_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"CORRAL_HELPER_SERVER=1",
		"CORRAL_DATA_DIR="+dataDir,
		"CORRAL_LISTEN="+listenAddr,
		"CORRAL_PUBLIC_URL=https://corral.test",
		"CORRAL_MASTER_KEY="+masterKey,
	)
}

type runningServer struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	done   chan error
}

func startHelperServer(t *testing.T, dataDir, listenAddr, masterKey string) (*runningServer, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCorralServer$")
	cmd.Env = cleanServerEnv(dataDir, listenAddr, masterKey)

	pr, pw := io.Pipe()
	var buf bytes.Buffer
	cmd.Stderr = pw
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}

	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		done <- err
		close(done)
	}()
	go func() {
		defer pr.Close()
		sc := bufio.NewScanner(pr)
		signaled := false
		for sc.Scan() {
			line := sc.Text()
			buf.WriteString(line)
			buf.WriteByte('\n')
			if !signaled && strings.Contains(line, "corral listening") {
				signaled = true
				close(ready)
			}
		}
	}()

	select {
	case <-ready:
		return &runningServer{cmd: cmd, stderr: &buf, done: done}, nil
	case err := <-done:
		return &runningServer{cmd: cmd, stderr: &buf, done: done}, fmt.Errorf("server exited early: %w\nstderr:\n%s", err, buf.String())
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return nil, fmt.Errorf("timed out waiting for server to listen\nstderr:\n%s", buf.String())
	}
}

func (rs *runningServer) stopSIGTERM(t *testing.T) {
	t.Helper()
	if rs == nil || rs.cmd == nil || rs.cmd.Process == nil {
		return
	}
	_ = rs.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-rs.done:
		if err != nil {
			t.Fatalf("server did not exit cleanly on SIGTERM: %v\nstderr:\n%s", err, rs.stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = rs.cmd.Process.Kill()
		<-rs.done
		t.Fatalf("timed out waiting for server SIGTERM exit")
	}
}

func (rs *runningServer) killSIGKILL(t *testing.T) {
	t.Helper()
	if rs == nil || rs.cmd == nil || rs.cmd.Process == nil {
		return
	}
	_ = rs.cmd.Process.Kill()
	select {
	case <-rs.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for server SIGKILL exit")
	}
}

// TestServerDataDirLockPreventsSecondServerSweep verifies Fix16 Item #1:
//   - Starting a second corral server on the same DataDir fails before opening
//     the store / running RecoverCrashedCorraldOps, leaving the first server's
//     live running op untouched (even when both use the same listen port, where
//     without the lock the sweep ran before net.Listen failed).
//   - Direct store access (as CLI subcommands reading/editing users would do)
//     still succeeds while the server holds the data-dir lock.
//   - Clean restart (SIGTERM then new server) and crash recovery (SIGKILL
//     leaving the lock file on disk, then new server) both succeed cleanly.
func TestServerDataDirLockPreventsSecondServerSweep(t *testing.T) {
	dataDir := t.TempDir()
	masterKey := randomMasterKey(t)
	addr := freeTCPAddr(t)

	srv1, err := startHelperServer(t, dataDir, addr, masterKey)
	if err != nil {
		t.Fatalf("start first server: %v", err)
	}
	t.Cleanup(func() {
		if srv1 != nil && srv1.cmd != nil && srv1.cmd.Process != nil {
			_ = srv1.cmd.Process.Kill()
		}
	})

	// While server 1 is running and holds the lock, a direct store open
	// (modeling non-server CLI subcommands) must still work; seed a live
	// running op owned by server 1.
	ctx := context.Background()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("direct store.Open while server running failed: %v", err)
	}
	u, err := st.CreateUser(ctx, "admin", "Admin", "x", store.RoleAdmin)
	if err != nil {
		_ = st.Close()
		t.Fatalf("CreateUser: %v", err)
	}
	uid := u.ID
	h, err := st.CreateHost(ctx, &store.Host{
		OwnerID:  uid,
		Name:     "box1",
		Address:  "10.0.0.1",
		Port:     22,
		Username: "alice",
		Auth:     store.AuthPassword,
	})
	if err != nil {
		_ = st.Close()
		t.Fatalf("CreateHost: %v", err)
	}
	const liveOpID = "1601000000000001"
	acctKey := store.CorraldAccountKey("SHA256:fp-live", "alice")
	if err := st.UpsertCorraldHost(ctx, &store.CorraldHost{
		HostID:           h.ID,
		State:            "removing",
		BoundFingerprint: "SHA256:fp-live",
		BoundUsername:    "alice",
		BoundGateway:     "corral.test",
		ActiveOpID:       liveOpID,
	}); err != nil {
		_ = st.Close()
		t.Fatalf("UpsertCorraldHost: %v", err)
	}
	if err := st.CreateCorraldOp(ctx, &store.CorraldOp{
		ID:         liveOpID,
		HostID:     h.ID,
		AccountKey: acctKey,
		Kind:       "remove",
		State:      "running",
		Step:       "uninstall",
		CreatedBy:  uid,
	}, false); err != nil {
		_ = st.Close()
		t.Fatalf("CreateCorraldOp: %v", err)
	}
	_ = st.Close()

	// Start a second server on the SAME dataDir (same port, matching the review
	// scenario where previously the startup sweep ran before net.Listen failed).
	cmd2 := exec.Command(os.Args[0], "-test.run=^TestHelperCorralServer$")
	cmd2.Env = cleanServerEnv(dataDir, addr, masterKey)
	out2, err2 := cmd2.CombinedOutput()
	if err2 == nil {
		t.Fatalf("expected second server on same dataDir to fail, but it succeeded\noutput:\n%s", string(out2))
	}

	// Verify server 1's live running op was NOT touched by server 2!
	stCheck, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store after second server exit: %v", err)
	}
	op, err := stCheck.LatestCorraldOp(ctx, h.ID)
	if err != nil {
		_ = stCheck.Close()
		t.Fatalf("LatestCorraldOp: %v", err)
	}
	if op.ID != liveOpID || op.State != "running" {
		_ = stCheck.Close()
		t.Fatalf("Fix16 Item #1 regression: second server rewrote first server's live op: got state=%q error_code=%q (want running)\nsecond server output:\n%s", op.State, op.ErrorCode, string(out2))
	}
	ch, err := stCheck.CorraldHostByID(ctx, h.ID)
	if err != nil {
		_ = stCheck.Close()
		t.Fatalf("CorraldHostByID: %v", err)
	}
	if ch.State != "removing" || ch.ActiveOpID != liveOpID {
		_ = stCheck.Close()
		t.Fatalf("Fix16 Item #1 regression: second server modified host row: state=%q active_op_id=%q", ch.State, ch.ActiveOpID)
	}
	_ = stCheck.Close()

	out2Str := string(out2)
	if !strings.Contains(out2Str, "another corral server") && !strings.Contains(out2Str, "corral.lock") {
		t.Fatalf("expected clear data-dir lock error from second server, got:\n%s", out2Str)
	}

	// Clean restart (SIGTERM stops old server before new starts, as in compose).
	srv1.stopSIGTERM(t)
	srv1 = nil

	srv3, err := startHelperServer(t, dataDir, freeTCPAddr(t), masterKey)
	if err != nil {
		t.Fatalf("restart server after SIGTERM failed: %v", err)
	}
	t.Cleanup(func() {
		if srv3 != nil && srv3.cmd != nil && srv3.cmd.Process != nil {
			_ = srv3.cmd.Process.Kill()
		}
	})

	// After clean restart, the real startup sweep SHOULD have swept the old op.
	stAfterRestart, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("open store after restart: %v", err)
	}
	opAfter, err := stAfterRestart.LatestCorraldOp(ctx, h.ID)
	_ = stAfterRestart.Close()
	if err != nil || opAfter.State != "failed" || opAfter.ErrorCode != "interrupted" {
		t.Fatalf("expected restarted server to sweep crashed op to failed/interrupted, got (%+v, %v)", opAfter, err)
	}

	// Simulate crash (SIGKILL): lock file stays on disk, kernel releases flock,
	// so a new server must start cleanly without blocking.
	srv3.killSIGKILL(t)
	srv3 = nil
	if _, err := os.Stat(filepath.Join(dataDir, "corral.lock")); err != nil {
		t.Fatalf("expected lock file to remain on disk after SIGKILL, stat err=%v", err)
	}

	srv4, err := startHelperServer(t, dataDir, freeTCPAddr(t), masterKey)
	if err != nil {
		t.Fatalf("server start after SIGKILL crash failed: %v", err)
	}
	srv4.stopSIGTERM(t)
}
