package sshx

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/testutil"
)

func dialTest(t *testing.T, srv *testutil.SSHServer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.Addr.Port)), &ssh.ClientConfig{User: "alice",
		Auth: []ssh.AuthMethod{ssh.Password("alice-pass")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestOutputIsCapped(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd == "small" {
			return func(ch ssh.Channel) {
				ch.Write([]byte("hello"))
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return func(ch ssh.Channel) { // never stops printing
			chunk := bytes.Repeat([]byte("x"), 32<<10)
			for {
				if _, err := ch.Write(chunk); err != nil {
					return
				}
			}
		}
	})
	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := Output(ctx, c, "small", 1<<20)
	if err != nil || string(out) != "hello" {
		t.Fatalf("small output: %q %v", out, err)
	}
	out, err = Output(ctx, c, "flood", 1<<20)
	if !errors.Is(err, ErrOutputTooLarge) || len(out) > 1<<20 {
		t.Fatalf("flood: %d bytes, %v", len(out), err)
	}
	// The connection is still usable afterwards.
	if out, err := Output(ctx, c, "small", 1<<20); err != nil || string(out) != "hello" {
		t.Fatalf("after flood: %q %v", out, err)
	}
}

// A host may send EOF first and the exit status a moment later: reading EOF
// must not close the channel and cut the status off.
func TestExitStatusAfterEOFIsKept(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		return func(ch ssh.Channel) {
			ch.Write([]byte("hello"))
			ch.CloseWrite()
			time.Sleep(200 * time.Millisecond)
			status := byte(0)
			if cmd == "fail" {
				status = 3
			}
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, status})
		}
	})
	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := Output(ctx, c, "ok", 1<<20); err != nil || string(out) != "hello" {
		t.Fatalf("exit status after EOF: %q %v", out, err)
	}
	if _, err := Output(ctx, c, "fail", 1<<20); err == nil || !strings.Contains(err.Error(), "status 3") {
		t.Fatalf("failing command after EOF: %v", err)
	}
}

func TestOutputHonoursContext(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	hang := make(chan struct{})
	defer close(hang)
	srv.HandleExec(func(string) func(ssh.Channel) { return func(ssh.Channel) { <-hang } })
	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Output(ctx, c, "silent", 1<<20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}
