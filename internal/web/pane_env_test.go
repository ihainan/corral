package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

func TestIsSensitiveEnv(t *testing.T) {
	sensitive := [][2]string{
		{"ANTHROPIC_API_KEY", "anything"},
		{"GITHUB_TOKEN", "val"},
		{"APP_SECRET", "123"},
		{"DB_PASSWORD", "short"},
		{"REDIS_PASS", "abc"},
		{"BASIC_AUTH", "abc"},
		{"GOOGLE_APPLICATION_CREDENTIALS", "/tmp/sa.json"},
		{"SESSION_COOKIE", "abc"},
		{"AWS_SESSION_TOKEN", "abc"},
		{"TLS_PRIVATE_PEM", "x"},
		{"PLAIN_NAME_SK", "sk-ant-api03-1234567890abcdefghij"},
		{"PLAIN_NAME_GH", "ghp_1234567890abcdefghijklmnopqrstuv"},
		{"PLAIN_NAME_URL", "postgres://dbuser:s3cr3tpass@db.internal:5432/app"},
		{"PLAIN_NAME_HTTP_URL", "http://alice:hunter2@proxy.internal:8080"},
		{"PLAIN_NAME_JWT", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"},
		{"PLAIN_NAME_HEX", "9f8e7d6c5b4a3210fedcba9876543210"},
		{"PLAIN_NAME_RAND", "aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW"},
		{"PLAIN_NAME_BEARER", "Bearer some-opaque-value"},
		{"PLAIN_NAME_PEM", "-----BEGIN PRIVATE KEY-----\nMIIE..."},
	}
	for _, tc := range sensitive {
		if !isSensitiveEnv(tc[0], tc[1]) {
			t.Errorf("isSensitiveEnv(%q, %q) = false, want true", tc[0], tc[1])
		}
	}

	plain := [][2]string{
		{"HOME", "/tmp/test-home"},
		{"PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		{"TERM", "xterm-256color"},
		{"LANG", "en_US.UTF-8"},
		{"EDITOR", "vim"},
		{"PWD", "/tmp/work/corral"},
		{"HTTPS_PROXY", "http://proxy.internal:8080"},
		{"COLORTERM", "truecolor"},
		{"GOPATH", "/tmp/gopath"},
		{"RUST_LOG", "info,corral=debug"},
	}
	for _, tc := range plain {
		if isSensitiveEnv(tc[0], tc[1]) {
			t.Errorf("isSensitiveEnv(%q, %q) = true, want false", tc[0], tc[1])
		}
	}
}

func TestMaskEnvPreview(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"a", "••••••••"},
		{"abcd", "••••••••"},
		{"abcdef", "ab••••••••"},
		{"sk-ant-secret-999999", "sk-a••••••••"},
		{"a\nb\tcdefgh", "a b ••••••••"},
	}
	for _, tc := range cases {
		if got := maskEnvPreview(tc.in); got != tc.want {
			t.Errorf("maskEnvPreview(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseProcEnviron(t *testing.T) {
	raw := []byte("Z_VAR=last\x00A_VAR=first=with=eq\x00MULTI=line1\nline2\x00A_VAR=updated\x00=bad\x00NOEQ\x00")
	got := parseProcEnviron(raw, false)
	if len(got) != 3 {
		t.Fatalf("expected 3 vars, got %+v", got)
	}
	if got[0].Name != "A_VAR" || got[0].Value != "updated" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Name != "MULTI" || got[1].Value != "line1\nline2" {
		t.Errorf("got[1] = %+v", got[1])
	}
	if got[2].Name != "Z_VAR" || got[2].Value != "last" {
		t.Errorf("got[2] = %+v", got[2])
	}

	// Truncated buffer without trailing NUL drops incomplete tail entry.
	trunc := []byte("ALPHA=1\x00BETA=partial")
	gotTrunc := parseProcEnviron(trunc, true)
	if len(gotTrunc) != 1 || gotTrunc[0].Name != "ALPHA" {
		t.Fatalf("truncated parse = %+v, want only ALPHA", gotTrunc)
	}
}

func TestPickHerdrEnvPID(t *testing.T) {
	pi := &herdr.PaneProcessInfoResult{
		PaneID:   "w1:p1",
		ShellPID: 100,
		ForegroundProcesses: []herdr.ProcessEntry{
			{PID: 100, Name: "zsh"},
			{PID: 200, Name: "node", Argv: []string{"/usr/bin/node", "script.js"}},
			{PID: 300, Name: "claude", Argv: []string{"claude"}},
		},
	}
	pid, name := pickHerdrEnvPID(pi)
	if pid != 300 || name != "claude" {
		t.Fatalf("expected agent pid 300/claude, got %d/%q", pid, name)
	}

	pi2 := &herdr.PaneProcessInfoResult{
		PaneID:   "w1:p1",
		ShellPID: 100,
		ForegroundProcesses: []herdr.ProcessEntry{
			{PID: 100, Name: "bash"},
			{PID: 250, Name: "python3"},
		},
	}
	pid, name = pickHerdrEnvPID(pi2)
	if pid != 250 || name != "python3" {
		t.Fatalf("expected non-shell pid 250/python3, got %d/%q", pid, name)
	}

	pi3 := &herdr.PaneProcessInfoResult{PaneID: "w1:p1", ShellPID: 100}
	pid, _ = pickHerdrEnvPID(pi3)
	if pid != 100 {
		t.Fatalf("expected shell fallback 100, got %d", pid)
	}
}

// startPTYChild launches a long-lived child process attached to a real Linux
// PTY (so it is a genuine foreground process group leader) with the given env.
func startPTYChild(t *testing.T, extraEnv []string) (int, func()) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx: %v", err)
	}
	var unlock int32
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, int(unlock)); err != nil {
		ptmx.Close()
		t.Skipf("TIOCSPTLCK failed: %v", err)
	}
	ptyNum, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		ptmx.Close()
		t.Skipf("TIOCGPTN failed: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptyNum), os.O_RDWR, 0)
	if err != nil {
		ptmx.Close()
		t.Skipf("open pts slave failed: %v", err)
	}

	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, extraEnv...)
	if err := cmd.Start(); err != nil {
		slave.Close()
		ptmx.Close()
		t.Fatalf("start pty child: %v", err)
	}
	slave.Close()
	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = ptmx.Close()
	}
	return cmd.Process.Pid, cleanup
}

func TestPaneEnvEndToEndMaskingRevealAuditAndOwnership(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc/<pid>/environ required")
	}

	secretKeyVal := "sk-ant-api03-supersecretvalue987654321"
	secretURLVal := "postgres://svcuser:myVerySecretPwd@db.example.internal:5432/prod"
	secretRandVal := "aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW"
	plainVal := "corral-plain-marker"

	childPID, stopChild := startPTYChild(t, []string{
		"CORRAL_PLAIN_TEST=" + plainVal,
		"CORRAL_API_KEY=" + secretKeyVal,
		"CORRAL_DATABASE_URL=" + secretURLVal,
		"CORRAL_OPAQUE_VAL=" + secretRandVal,
	})
	defer stopChild()

	h := newHarness(t)
	admin := h.admin()
	alice := h.member(admin, "alice")
	bob := h.member(admin, "bob")

	hd := newFakeHerdr(t)
	hd.procInfo = map[string]herdr.PaneProcessInfoResult{
		"w1:p1": {
			PaneID:   "w1:p1",
			ShellPID: childPID,
			ForegroundProcesses: []herdr.ProcessEntry{
				{PID: childPID, Name: "claude", Argv: []string{"claude"}},
			},
		},
	}

	srv := h.sshServer(t.TempDir())
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if cmd == herdr.KeysCommand {
			return noHerdrConfig
		}
		if cmd == herdr.DiscoverCommand {
			return func(ch ssh.Channel) {
				b, _ := json.Marshal(map[string]any{
					"sessions": []any{map[string]any{"name": "default", "default": true, "running": true, "socket_path": hd.path}},
				})
				ch.Write(b)
				ch.CloseWrite()
				time.Sleep(30 * time.Millisecond)
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		if strings.Contains(cmd, "/proc/$pid/environ") {
			return func(ch ssh.Channel) {
				out, _ := exec.Command("sh", "-c", cmd).Output()
				ch.Write(out)
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return nil
	})

	rHost := alice.do("POST", "/api/hosts", map[string]any{
		"name": "env-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "herdr",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	rSess := alice.do("POST", "/api/sessions", map[string]any{"host_id": int64(rHost.json()["id"].(float64))})
	mustStatus(t, rSess, http.StatusCreated, "open session")
	sessID := rSess.json()["id"].(string)

	// Wait briefly for herdr socket discovery to register.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.srv.herdrSocks.get(sessID) == "" {
		time.Sleep(25 * time.Millisecond)
	}
	if h.srv.herdrSocks.get(sessID) == "" {
		t.Fatal("herdr socket was not discovered")
	}

	// 1. Cross-user access must be refused with 404.
	mustStatus(t, bob.do("GET", "/api/sessions/"+sessID+"/pane-env?pane=w1:p1", nil), http.StatusNotFound, "bob GET pane-env")
	mustStatus(t, bob.do("POST", "/api/sessions/"+sessID+"/pane-env/reveal", map[string]any{"pane": "w1:p1", "name": "CORRAL_API_KEY"}), http.StatusNotFound, "bob POST reveal")

	// 2. Owner GET pane-env returns sorted vars with secrets omitted from response body.
	rEnv := alice.do("GET", "/api/sessions/"+sessID+"/pane-env?pane=w1:p1", nil)
	mustStatus(t, rEnv, http.StatusOK, "owner GET pane-env")
	rawBody := string(rEnv.Body)
	for _, secret := range []string{secretKeyVal, secretURLVal, secretRandVal, "myVerySecretPwd"} {
		if strings.Contains(rawBody, secret) {
			t.Fatalf("secret %q leaked in GET pane-env response: %s", secret, rawBody)
		}
	}

	var envResp paneEnvResponse
	if err := json.Unmarshal(rEnv.Body, &envResp); err != nil {
		t.Fatal(err)
	}
	if envResp.PID != childPID || envResp.Reason != "" {
		t.Fatalf("unexpected env header: pid=%d want=%d reason=%q", envResp.PID, childPID, envResp.Reason)
	}
	byName := map[string]paneEnvVar{}
	for _, v := range envResp.Vars {
		byName[v.Name] = v
	}
	if v, ok := byName["CORRAL_PLAIN_TEST"]; !ok || v.Sensitive || v.Value != plainVal {
		t.Fatalf("plain var mismatch: %+v", v)
	}
	for _, k := range []string{"CORRAL_API_KEY", "CORRAL_DATABASE_URL", "CORRAL_OPAQUE_VAL"} {
		v, ok := byName[k]
		if !ok || !v.Sensitive || v.Value != "" || v.Preview == "" {
			t.Fatalf("sensitive var %s mismatch: %+v", k, v)
		}
	}

	// 3. Reveal returns the full value and records an audit entry WITHOUT the value.
	rRev1 := alice.do("POST", "/api/sessions/"+sessID+"/pane-env/reveal", map[string]any{"pane": "w1:p1", "name": "CORRAL_API_KEY"})
	mustStatus(t, rRev1, http.StatusOK, "reveal CORRAL_API_KEY")
	var revResp paneEnvRevealResponse
	if err := json.Unmarshal(rRev1.Body, &revResp); err != nil {
		t.Fatal(err)
	}
	if revResp.Name != "CORRAL_API_KEY" || revResp.Value != secretKeyVal || revResp.PID != childPID {
		t.Fatalf("unexpected reveal response: %+v", revResp)
	}

	// Second reveal writes a second audit entry.
	rRev2 := alice.do("POST", "/api/sessions/"+sessID+"/pane-env/reveal", map[string]any{"pane": "w1:p1", "name": "CORRAL_API_KEY"})
	mustStatus(t, rRev2, http.StatusOK, "second reveal CORRAL_API_KEY")

	entries, err := h.store.ListAudit(context.Background(), store.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	revealCount := 0
	for _, e := range entries {
		if e.Action == "session.env_reveal" && e.Username == "alice" {
			revealCount++
			if strings.Contains(string(e.Detail), secretKeyVal) {
				t.Fatalf("audit detail leaked secret value: %s", e.Detail)
			}
			if !strings.Contains(string(e.Detail), "CORRAL_API_KEY") {
				t.Fatalf("audit detail missing variable name: %s", e.Detail)
			}
		}
	}
	if revealCount != 2 {
		t.Fatalf("expected 2 session.env_reveal audit entries, got %d", revealCount)
	}

	// 4. Plain SSH session (mode=none) reports unsupported_mode clearly.
	rHostPlain := alice.do("POST", "/api/hosts", map[string]any{
		"name": "plain-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "none",
	})
	mustStatus(t, rHostPlain, http.StatusCreated, "create plain host")
	rSessPlain := alice.do("POST", "/api/sessions", map[string]any{"host_id": int64(rHostPlain.json()["id"].(float64))})
	mustStatus(t, rSessPlain, http.StatusCreated, "open plain session")
	plainSessID := rSessPlain.json()["id"].(string)

	rPlainEnv := alice.do("GET", "/api/sessions/"+plainSessID+"/pane-env", nil)
	mustStatus(t, rPlainEnv, http.StatusOK, "plain session pane-env")
	var plainResp paneEnvResponse
	if err := json.Unmarshal(rPlainEnv.Body, &plainResp); err != nil {
		t.Fatal(err)
	}
	if plainResp.Reason != "unsupported_mode" {
		t.Fatalf("expected reason=unsupported_mode, got %+v", plainResp)
	}
}
