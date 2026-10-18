package web

// Regressions from the VS Code review: archive digests, channel quota for the
// upload, connection changes, start/stop ordering, install lifetime.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// An archive that does not match its trusted digest is never unpacked, on
// either path.
func TestCodeServerArchiveDigestIsChecked(t *testing.T) {
	t.Run("download", func(t *testing.T) {
		e := newCodeEnv(t, false)
		e.h.cfg.CodeServerSHA256[codeArch()] = strings.Repeat("0", 64)
		st, _ := e.start(e.owner)
		if st["state"] != codeFailed || st["error"] != "checksum" {
			t.Fatalf("tampered download: %v", st)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".local/lib/corral/code-server-4.138.0")); err == nil {
			t.Fatal("tampered download unpacked")
		}
	})
	t.Run("upload", func(t *testing.T) {
		e := newCodeEnv(t, true)
		e.h.cfg.CodeServerSHA256[codeArch()] = strings.Repeat("0", 64)
		st, _ := e.start(e.owner)
		if st["state"] != codeFailed || st["error"] != "checksum" || !strings.Contains(st["message"].(string), "CORRAL_CODE_SERVER_DIR") {
			t.Fatalf("tampered upload not caught on the gateway: %v", st)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".local/lib/corral")); err == nil {
			if matches, _ := filepath.Glob(filepath.Join(e.home, ".local/lib/corral/.upload-*")); len(matches) != 0 {
				t.Fatal("tampered archive uploaded")
			}
		}
	})
}

// The SFTP upload uses a forwarding channel like any other.
func TestCodeServerUploadCountsAgainstChannels(t *testing.T) {
	e := newCodeEnv(t, true)
	setVar(t, &maxFwdChannelsPerConn, 0)
	st, _ := e.start(e.owner)
	if st["state"] != codeFailed || st["error"] != "busy" {
		t.Fatalf("upload over the channel limit: %v", st)
	}
}

// Changing the SSH user or how the host authenticates retires VS Code's
// connections and state, even with the address unchanged.
func TestCodeServerEndsWithIdentityChanges(t *testing.T) {
	for _, change := range []map[string]any{
		{"username": "bob", "auth": "password"},
		{"username": "alice", "auth": "ask"},
	} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			e := newCodeEnv(t, false)
			st, _ := e.start(e.owner)
			if st["state"] != codeReady {
				t.Fatalf("start: %v", st)
			}
			f, id := e.openCode(e.owner, st)
			ws, err := e.dialCode(f, id)
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"name": "box", "address": "127.0.0.1", "port": e.srv.Addr.Port}
			for k, v := range change {
				body[k] = v
			}
			mustStatus(t, e.owner.do("PUT", fmt.Sprintf("/api/hosts/%d", e.hostID), body), 200, "edit host")
			mustClose(t, ws, "VS Code after the identity changed")
			ownerID := e.h.userID(e.owner)
			if e.h.srv.code.socket(ownerID, e.hostID) != "" {
				t.Fatal("code-server state kept after the identity changed")
			}
			e.h.srv.fwd.mu.Lock()
			_, pooled := e.h.srv.fwd.conns[[2]int64{ownerID, e.hostID}]
			e.h.srv.fwd.mu.Unlock()
			if pooled {
				t.Fatal("connection made with the old identity still pooled")
			}
		})
	}
}

// A start asked for while a stop is running waits for it: the stop never
// tears down the instance started after it.
func TestCodeServerStartAfterStopIsNotTornDown(t *testing.T) {
	e := newCodeEnv(t, false)
	if st, _ := e.start(e.owner); st["state"] != codeReady {
		t.Fatalf("start: %v", st)
	}
	first := e.pid()
	stop := e.owner.doAsync("POST", fmt.Sprintf("/api/hosts/%d/code/stop", e.hostID), nil)
	time.Sleep(20 * time.Millisecond)
	st, _ := e.start(e.owner)
	mustStatus(t, awaitResp(t, stop, "stop"), 200, "stop")
	if st["state"] != codeReady {
		t.Fatalf("start after stop: %v", st)
	}
	time.Sleep(300 * time.Millisecond)
	second := e.pid()
	if second <= 0 || syscall.Kill(second, 0) != nil {
		t.Fatalf("the instance started after the stop is not running (pid %d, first %d)", second, first)
	}
	if r := e.owner.do("GET", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil); r.json()["state"] != codeReady {
		t.Fatalf("state: %s", r.Body)
	}
}

// A start waits for its turn: while a stop (here: the turn held) is under
// way it does not touch the host.
func TestCodeServerStartWaitsForAStop(t *testing.T) {
	e := newCodeEnv(t, false)
	ownerID := e.h.userID(e.owner)
	release := e.holdTurn(t)
	mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil), 202, "start")
	time.Sleep(500 * time.Millisecond)
	if st := e.h.srv.code.status(ownerID, e.hostID); st.State != codeChecking {
		t.Fatalf("start went ahead during a stop: %+v", st)
	}
	if b, _ := os.ReadFile(e.curlLog); len(b) != 0 {
		t.Fatal("start ran on the host during a stop")
	}
	release()
	if st, _ := e.waitCode(e.owner); st["state"] != codeReady {
		t.Fatalf("start after the stop: %v", st)
	}
}

// slowCurl makes downloads take a while.
func (e *codeEnv) slowCurl(t *testing.T) {
	path := filepath.Join(filepath.Dir(e.curlLog), "curl")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "#!/bin/sh\n", "#!/bin/sh\nsleep 1\n", 1)), 0o755)
}

// An install the user started carries on after a plain sign-out; disabling
// the user ends it.
func TestCodeServerInstallLifetime(t *testing.T) {
	e := newCodeEnv(t, false)
	e.slowCurl(t)
	mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil), 202, "start")
	mustStatus(t, e.owner.do("POST", "/api/logout", nil), 200, "sign out")
	again := e.h.login("alice")
	if st, _ := e.waitCode(again); st["state"] != codeReady {
		t.Fatalf("install after sign-out: %v", st)
	}

	e2 := newCodeEnv(t, false)
	e2.slowCurl(t)
	mustStatus(t, e2.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code", e2.hostID), nil), 202, "start")
	ownerID := e2.h.userID(e2.owner)
	mustStatus(t, e2.admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", ownerID), map[string]any{"disabled": true}), 200, "disable")
	if st := e2.h.srv.code.status(ownerID, e2.hostID); st.State != codeStopped {
		t.Fatalf("install kept for a disabled user: %+v", st)
	}
}

// holdTurn takes the host's next start/stop turn, as a stop under way would.
func (e *codeEnv) holdTurn(t *testing.T) func() {
	key := [2]int64{e.h.userID(e.owner), e.hostID}
	e.h.srv.code.mu.Lock()
	ticket := e.h.srv.code.enqueue(key)
	e.h.srv.code.mu.Unlock()
	if err := e.h.srv.code.waitTurn(context.Background(), key, ticket); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { e.h.srv.code.doneTurn(key) }) }
	t.Cleanup(release) // a failing test must not leave requests waiting
	return release
}

// Access revoked while a start waits for its turn: the start never runs.
func TestCodeServerRevokedBeforeItsTurn(t *testing.T) {
	e := newCodeEnv(t, false)
	ownerID := e.h.userID(e.owner)
	release := e.holdTurn(t)
	mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil), 202, "start")
	mustStatus(t, e.admin.do("POST", fmt.Sprintf("/api/admin/users/%d/password", ownerID), map[string]string{"password": "reset-password-1"}), 200, "reset")
	release()
	time.Sleep(500 * time.Millisecond)
	if b, _ := os.ReadFile(e.curlLog); len(b) != 0 {
		t.Fatal("a start ran on the host after the user's access was revoked")
	}
	if st := e.h.srv.code.status(ownerID, e.hostID); st.State != codeStopped {
		t.Fatalf("job kept after revocation: %+v", st)
	}
}

// The job itself checks, before each step, that the owner's access and the
// host's connection settings are what they were when it was asked for (not
// only that it was cancelled).
func TestCodeServerJobChecksAccessBeforeEachStep(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(e *codeEnv, ownerID int64)
	}{
		{"user", func(e *codeEnv, ownerID int64) {
			e.h.srv.code.mu.Lock()
			if e.h.srv.code.userGen == nil {
				e.h.srv.code.userGen = map[int64]uint64{}
			}
			e.h.srv.code.userGen[ownerID]++
			e.h.srv.code.mu.Unlock()
		}},
		{"host", func(e *codeEnv, ownerID int64) {
			if err := e.h.srv.store.BumpConnVersion(context.Background(), ownerID, e.hostID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCodeEnv(t, false)
			ownerID := e.h.userID(e.owner)
			release := e.holdTurn(t)
			mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil), 202, "start")
			c.change(e, ownerID)
			release()
			st, _ := e.waitCode(e.owner)
			if st["state"] != codeFailed || st["error"] != "revoked" {
				t.Fatalf("start after a change: %v", st)
			}
			if b, _ := os.ReadFile(e.curlLog); len(b) != 0 {
				t.Fatal("the start went on after the change")
			}
		})
	}
}

// A stop waiting for its turn shows as stopping.
func TestCodeServerStopShowsWhileQueued(t *testing.T) {
	e := newCodeEnv(t, false)
	release := e.holdTurn(t)
	stop := e.owner.doAsync("POST", fmt.Sprintf("/api/hosts/%d/code/stop", e.hostID), nil)
	eventually(t, "stop not shown as stopping", func() bool {
		return e.h.srv.code.status(e.h.userID(e.owner), e.hostID).State == codeStopping
	})
	notYet(t, stop, "a stop during another start or stop")
	release()
	mustStatus(t, awaitResp(t, stop, "stop"), 200, "stop")
	if st := e.h.srv.code.status(e.h.userID(e.owner), e.hostID); st.State != codeStopped {
		t.Fatalf("after the stop: %+v", st)
	}
}

// A stop ends the instance the start made, not a newer one started since
// (by another gateway, or after a restart).
func TestCodeServerStopTargetsItsInstance(t *testing.T) {
	e := newCodeEnv(t, false)
	if st, _ := e.start(e.owner); st["state"] != codeReady {
		t.Fatalf("start: %v", st)
	}
	pidFile := filepath.Join(e.home, ".local/state/corral/code-server.pid")
	b, _ := os.ReadFile(pidFile)
	f := strings.Fields(string(b))
	if len(f) != 3 {
		t.Fatalf("pid file: %q", b)
	}
	e.h.srv.code.mu.Lock()
	known := e.h.srv.code.jobs[[2]int64{e.h.userID(e.owner), e.hostID}].instance
	e.h.srv.code.mu.Unlock()
	if known != f[2] {
		t.Fatalf("job knows instance %q, host runs %q", known, f[2])
	}
	pid, _ := strconv.Atoi(f[0])
	os.WriteFile(pidFile, []byte(f[0]+" "+f[1]+" newer\n"), 0o600)
	mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code/stop", e.hostID), nil), 200, "stop")
	time.Sleep(200 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("the stop ended a newer instance")
	}
}

// alive is a process that exists and has not ended (zombies have).
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	return i > 0 && len(s) > i+2 && s[i+2] != 'Z'
}

// A cancelled install is ended on the host: its processes are gone and its
// staging directory removed, before anything else runs there.
func TestCodeServerCancelledInstallIsEndedOnTheHost(t *testing.T) {
	e := newCodeEnv(t, false)
	pidFile := filepath.Join(e.home, "curl.pid")
	os.WriteFile(filepath.Join(filepath.Dir(e.curlLog), "curl"), []byte("#!/bin/sh\necho $$ >"+pidFile+"\nexec sleep 60\n"), 0o755)
	mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil), 202, "start")
	var pid int
	eventually(t, "the download never started", func() bool {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return pid > 0
	})
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if len(e.staging()) == 0 {
		t.Fatal("no staging directory while installing")
	}
	ownerID := e.h.userID(e.owner)
	mustStatus(t, e.admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", ownerID), map[string]any{"disabled": true}), 200, "disable")
	eventually(t, "the download still runs on the host", func() bool { return !alive(pid) })
	eventually(t, "staging left behind", func() bool { return len(e.staging()) == 0 })
	// The host lock is free again: a stop goes through at once.
	mustStatus(t, e.admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", ownerID), map[string]any{"disabled": false}), 200, "enable")
	again := e.h.login("alice")
	mustStatus(t, again.do("POST", fmt.Sprintf("/api/hosts/%d/code/stop", e.hostID), nil), 200, "stop after cancel")
}
