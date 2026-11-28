package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
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

func TestIsSensitiveEnvURLAndConnStrings(t *testing.T) {
	cases := []struct {
		name      string
		envName   string
		envVal    string
		sensitive bool
	}{
		// Sensitive URL query parameters and fragments (case-insensitive).
		{"pg query password", "DATABASE_URL", "postgresql://db/app?user=alice&password=hunter2", true},
		{"pg query uppercase PASSWORD", "DATABASE_URL", "postgresql://db/app?user=alice&PASSWORD=hunter2", true},
		{"mysql query passwd", "DB_URI", "mysql://db.internal:3306/app?passwd=secret", true},
		{"mssql query pwd", "DB_URI", "mssql://db.internal/app?pwd=secret", true},
		{"webhook query token", "WEBHOOK_URL", "https://hooks.example.com/cb?token=abc123", true},
		{"fragment access_token", "CALLBACK_URL", "https://example.com/cb#access_token=xyz123&expires=3600", true},
		{"query api_key", "API_ENDPOINT", "https://api.example.com/v1?api_key=k123", true},
		{"query apikey", "API_ENDPOINT", "https://api.example.com/v1?apikey=k123", true},
		{"query camelCase apiKey", "API_ENDPOINT", "https://api.example.com/v1?apiKey=k123", true},
		{"query key", "API_ENDPOINT", "https://api.example.com/v1?key=k123", true},
		{"query X-Amz-Signature", "SIGNED_URL", "https://s3.example.com/obj?X-Amz-Signature=deadbeef", true},
		{"query sig", "SIGNED_URL", "https://cdn.example.com/file?sig=deadbeef", true},
		{"query auth", "REDIS_URL", "redis://cache.internal:6379/0?auth=mysecret", true},
		{"query client_secret", "OAUTH_URL", "https://idp.example.com/token?client_secret=s123", true},
		{"fragment id_token", "FRAG_URL", "https://example.com/app#id_token=abc&state=1", true},

		// Common connection string shapes (including asymmetric whitespace and colon forms).
		{"adonet Password;", "CONN_STR", "Server=myServer;Database=myDB;User Id=sa;Password=hunter2;", true},
		{"adonet space before eq", "CONN_STR", "Server=db;Password =hunter2;Database=app", true},
		{"adonet space after eq", "CONN_STR", "Server=db;Password= hunter2;Database=app", true},
		{"odbc PWD=", "CONN_STR", "Driver={ODBC Driver 18};Server=tcp:host,1433;UID=sa;PWD=secret123", true},
		{"spaced Password =", "CONN_STR", "Server = db; Password = hunter2;", true},
		{"libpq space-delimited password", "PGCONN", "host=localhost port=5432 user=alice password=hunter2 dbname=app", true},
		{"libpq spaces around eq", "PGCONN", "host=db password = hunter2 dbname=app", true},
		{"libpq quoted password", "PGCONN", "host=localhost user=alice password='my secret' dbname=app", true},
		{"json password colon", "APP_CONFIG", `{"user":"alice","password": "hunter2"}`, true},
		{"header api key colon", "EXTRA_HEADER", "X-Api-Key: secret123", true},
		{"yaml token colon", "APP_YAML", "host: db\ntoken: abc123", true},
		{"azure SharedAccessKey", "SERVICE_BUS", "Endpoint=sb://ns.servicebus.windows.net/;SharedAccessKeyName=Root;SharedAccessKey=abc123=", true},
		{"azure AccountKey", "STORAGE_CONN", "DefaultEndpointsProtocol=https;AccountName=dev;AccountKey=abc123==", true},
		{"short PWD=", "SHORT_CONN", "PWD=a", true},

		// Percent-encoded and double-encoded parameter names.
		{"percent-encoded query password", "DATABASE_URL", "https://example.com/?%70assword=hunter2", true},
		{"percent-encoded fragment access_token", "CALLBACK_URL", "https://example.com/#access%5Ftoken=abc123", true},
		{"double-encoded query password", "DATABASE_URL", "https://example.com/?%2570assword=hunter2", true},

		// Fail-closed: unquoted space-delimited "password= dbname=app" is indistinguishable
		// from "password= foo=bar" (a space-padded password containing '='), so mask it.
		{"ambiguous unquoted empty libpq password fail-closed", "PGCONN", "host=localhost user=alice password= dbname=app", true},

		// Ordinary non-sensitive values must NOT be misclassified.
		{"pg url without password", "DATABASE_URL", "postgresql://db.internal:5432/app?sslmode=require&connect_timeout=10", false},
		{"mongo authSource", "MONGO_URL", "mongodb://db.internal:27017/app?authSource=admin&retryWrites=true", false},
		{"mongo authMechanism", "MONGO_URL", "mongodb://db.internal:27017/app?authMechanism=SCRAM-SHA-256", false},
		{"ordinary query params", "DOCS_URL", "https://example.com/docs?page=2&sort=asc&author=alice&design=dark&keyboard=us", false},
		{"malformed percent sequences", "DOCS_URL", "https://example.com/?foo=100%&bar=%2&baz=%ZZ", false},
		{"empty password query trailing", "DATABASE_URL", "postgresql://db/app?user=alice&password=", false},
		{"empty password query mid", "DATABASE_URL", "postgresql://db/app?password=&user=alice", false},
		{"empty password conn semicolon", "CONN_STR", "Server=db;Password=;Database=app", false},
		{"empty password conn spaces semicolon", "CONN_STR", "Server=db;Password = ;Database=app", false},
		{"empty password libpq trailing", "PGCONN", "host=localhost user=alice password=", false},
		{"empty single-quoted password", "PGCONN", "host=localhost password='' dbname=app", false},
		{"empty double-quoted password", "CONN_STR", `Server=db;Password="";Database=app`, false},
		{"empty json password", "APP_CONFIG", `{"user":"alice","password": ""}`, false},
		{"PWD env var path", "PWD", "/tmp/work/corral", false},
		{"PATH with auth dir segment", "PATH", "/usr/local/sbin:/opt/auth:/usr/bin", false},
		{"RUST_LOG value", "RUST_LOG", "info,corral=debug", false},
		{"cmake flags non-secret", "CMAKE_FLAGS", "-DKEY_SIZE=16 -DSIGNAL=TERM", false},
		{"azure key name only", "BUS_META", "SharedAccessKeyName=RootManage;Endpoint=sb://ns.example.com/", false},
		{"bypass and passport words", "FLAGS", "bypass=true;passport=false", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSensitiveEnv(tc.envName, tc.envVal); got != tc.sensitive {
				t.Errorf("isSensitiveEnv(%q, %q) = %v, want %v", tc.envName, tc.envVal, got, tc.sensitive)
			}
		})
	}
}

func TestTmuxPaneEnvOwnershipCheckAndRename(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc/<pid>/environ required")
	}

	ownPID, stopOwn := startPTYChild(t, []string{
		"CORRAL_OWN_MARKER=from-work-pane",
		"CORRAL_DB_URL=postgresql://db/app?user=alice&password=hunter2",
	})
	defer stopOwn()

	otherSecret := "sk-other-session-secret-999888777"
	otherPID, stopOther := startPTYChild(t, []string{
		"CORRAL_OTHER_SECRET=" + otherSecret,
	})
	defer stopOther()

	linkedPID, stopLinked := startPTYChild(t, []string{
		"CORRAL_LINKED_MARKER=shared-linked-window",
	})
	defer stopLinked()

	h := newHarness(t)
	admin := h.admin()

	home := t.TempDir()
	stateFile := filepath.Join(home, "tmux_sessions.txt")
	if err := os.WriteFile(stateFile, []byte("work\nother\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pane membership table: paneID \t session \t pid.
	// %1 is only in "work", %2 is only in "other", %3 is a linked window in BOTH "work" and "other".
	paneFile := filepath.Join(home, "tmux_panes.txt")
	paneRows := fmt.Sprintf("%%1\twork\t%d\n%%2\tother\t%d\n%%3\tother\t%d\n%%3\twork\t%d\n", ownPID, otherPID, linkedPID, linkedPID)
	if err := os.WriteFile(paneFile, []byte(paneRows), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
STATE="` + stateFile + `"
PANES="` + paneFile + `"
if [ "$1" = "-u" ]; then shift; fi
cmd="$1"; shift
case "$cmd" in
  list-sessions)
    while IFS= read -r n; do
      [ -n "$n" ] && printf '%s|1|1\n' "$n"
    done < "$STATE"
    exit 0
    ;;
  has-session)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) target="$2"; shift 2;;
        *) shift;;
      esac
    done
    target="${target#=}"
    while IFS= read -r n; do
      [ "$n" = "$target" ] && exit 0
    done < "$STATE"
    exit 1
    ;;
  rename-session)
    old=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -t) old="$2"; shift 2;;
        --) shift; break;;
        *) break;;
      esac
    done
    new="$1"
    old="${old#=}"
    tmp="$STATE.tmp"
    : > "$tmp"
    while IFS= read -r n; do
      if [ "$n" = "$old" ]; then printf '%s\n' "$new" >> "$tmp"
      elif [ -n "$n" ]; then printf '%s\n' "$n" >> "$tmp"
      fi
    done < "$STATE"
    mv "$tmp" "$STATE"
    ptmp="$PANES.tmp"
    : > "$ptmp"
    while IFS='	' read -r pid sess ppid; do
      if [ "$sess" = "$old" ]; then sess="$new"; fi
      [ -n "$pid" ] && printf '%s\t%s\t%s\n' "$pid" "$sess" "$ppid" >> "$ptmp"
    done < "$PANES"
    mv "$ptmp" "$PANES"
    exit 0
    ;;
  list-panes)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -s) shift;;
        -t) target="$2"; shift 2;;
        -F) shift 2;;
        *) shift;;
      esac
    done
    want="${target#=}"
    want="${want%:*}"
    found_sess=0
    while IFS= read -r n; do
      [ "$n" = "$want" ] && found_sess=1
    done < "$STATE"
    [ "$found_sess" = 1 ] || exit 1
    while IFS='	' read -r pid sess ppid; do
      if [ "$sess" = "$want" ]; then
        printf '%s\n' "$pid"
      fi
    done < "$PANES"
    exit 0
    ;;
  display-message)
    target=""
    fmt=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -p) shift;;
        -t) target="$2"; shift 2;;
        *) fmt="$1"; shift;;
      esac
    done
    case "$target" in
      %*)
        # Note: for linked pane %3, first matching row is "other", mirroring real tmux
        # where display-message on %N only reports one session name.
        while IFS='	' read -r pid sess ppid; do
          if [ "$pid" = "$target" ]; then
            case "$fmt" in
              *'#{session_name}'*'#{pane_pid}'*) printf '%s\t%s\n' "$sess" "$ppid"; exit 0;;
              '#{session_name}') printf '%s\n' "$sess"; exit 0;;
              '#{pane_pid}') printf '%s\n' "$ppid"; exit 0;;
              *'#{pane_id}|#{pane_pid}'*) printf '%s|%s||sh|0\n' "$pid" "$ppid"; exit 0;;
            esac
          fi
        done < "$PANES"
        exit 1
        ;;
      =*:*)
        want="${target#=}"
        want="${want%:*}"
        while IFS='	' read -r pid sess ppid; do
          if [ "$sess" = "$want" ]; then
            case "$fmt" in
              *'#{pane_id}|#{pane_pid}'*) printf '%s|%s||sh|0\n' "$pid" "$ppid"; exit 0;;
              *) printf '%s\n' "$ppid"; exit 0;;
            esac
          fi
        done < "$PANES"
        exit 1
        ;;
    esac
    exit 1
    ;;
  new-session|attach-session)
    read x
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(t.TempDir(), "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "mv", "sed", "cut", "tr", "head", "ps"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(sys, tool))
		}
	}

	srv := h.sshServer(home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, "tmux") || strings.Contains(cmd, "/proc/")
	}, "PATH="+sys))

	rHost := admin.do("POST", "/api/hosts", map[string]any{
		"name": "tmux-env-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	hostID := rHost.json()["id"].(float64)

	rSess := admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "work"},
	})
	mustStatus(t, rSess, http.StatusCreated, "open work tmux session")
	sessID := rSess.json()["id"].(string)

	rOtherSess := admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "other"},
	})
	mustStatus(t, rOtherSess, http.StatusCreated, "open other tmux session")
	otherSessID := rOtherSess.json()["id"].(string)

	// 1. Own pane %1 succeeds on both pane-env and pane-inspect, and masks query-param password.
	rOwn := admin.do("GET", "/api/sessions/"+sessID+"/pane-env?pane=%251", nil)
	mustStatus(t, rOwn, http.StatusOK, "GET own pane env")
	if strings.Contains(string(rOwn.Body), "hunter2") {
		t.Fatalf("query-param password leaked in GET pane-env: %s", rOwn.Body)
	}
	var ownResp paneEnvResponse
	if err := json.Unmarshal(rOwn.Body, &ownResp); err != nil {
		t.Fatal(err)
	}
	if ownResp.Reason != "" || ownResp.PID != ownPID {
		t.Fatalf("expected own pane pid=%d reason='', got %+v", ownPID, ownResp)
	}
	foundOwn, foundDBMasked := false, false
	for _, v := range ownResp.Vars {
		if v.Name == "CORRAL_OWN_MARKER" && !v.Sensitive && v.Value == "from-work-pane" {
			foundOwn = true
		}
		if v.Name == "CORRAL_DB_URL" && v.Sensitive && v.Value == "" && v.Preview != "" {
			foundDBMasked = true
		}
	}
	if !foundOwn || !foundDBMasked {
		t.Fatalf("expected own marker and masked db url, got %+v", ownResp.Vars)
	}

	rOwnInspect := admin.do("GET", "/api/sessions/"+sessID+"/pane-inspect?pane=%251", nil)
	mustStatus(t, rOwnInspect, http.StatusOK, "GET own pane inspect")
	var ownInspect paneInspectResponse
	if err := json.Unmarshal(rOwnInspect.Body, &ownInspect); err != nil {
		t.Fatal(err)
	}
	if ownInspect.Pane != "%1" || ownInspect.ShellPID != ownPID {
		t.Fatalf("expected own pane inspect %%1/%d, got %+v", ownPID, ownInspect)
	}

	// 2. Foreign pane %2 (belongs only to tmux session "other") MUST be rejected on pane-env, reveal, and pane-inspect.
	rForeign := admin.do("GET", "/api/sessions/"+sessID+"/pane-env?pane=%252", nil)
	mustStatus(t, rForeign, http.StatusOK, "GET foreign pane env")
	if strings.Contains(string(rForeign.Body), otherSecret) || strings.Contains(string(rForeign.Body), "CORRAL_OTHER_SECRET") {
		t.Fatalf("foreign session env leaked: %s", rForeign.Body)
	}
	var foreignResp paneEnvResponse
	if err := json.Unmarshal(rForeign.Body, &foreignResp); err != nil {
		t.Fatal(err)
	}
	if foreignResp.Reason != "pane_not_in_session" || len(foreignResp.Vars) != 0 {
		t.Fatalf("expected pane_not_in_session with 0 vars, got %+v", foreignResp)
	}

	rRevForeign := admin.do("POST", "/api/sessions/"+sessID+"/pane-env/reveal", map[string]any{
		"pane": "%2",
		"name": "CORRAL_OTHER_SECRET",
	})
	mustStatus(t, rRevForeign, http.StatusConflict, "reveal foreign pane env rejected")
	if rRevForeign.code() != "pane_not_in_session" || strings.Contains(string(rRevForeign.Body), otherSecret) {
		t.Fatalf("unexpected foreign reveal response: %d %s", rRevForeign.Status, rRevForeign.Body)
	}

	rForeignInspect := admin.do("GET", "/api/sessions/"+sessID+"/pane-inspect?pane=%252", nil)
	mustStatus(t, rForeignInspect, http.StatusOK, "GET foreign pane inspect")
	var foreignInspect paneInspectResponse
	if err := json.Unmarshal(rForeignInspect.Body, &foreignInspect); err != nil {
		t.Fatal(err)
	}
	if foreignInspect.Pane != "" || foreignInspect.ShellPID != 0 {
		t.Fatalf("expected empty inspect for foreign pane %%2, got %+v", foreignInspect)
	}

	// 3. Linked-window pane %3 (in BOTH "work" and "other") MUST succeed from BOTH sessions
	//    on both pane-env and pane-inspect even though display-message reports only "other".
	for label, sid := range map[string]string{"work": sessID, "other": otherSessID} {
		rLinkedEnv := admin.do("GET", "/api/sessions/"+sid+"/pane-env?pane=%253", nil)
		mustStatus(t, rLinkedEnv, http.StatusOK, "GET linked pane env from "+label)
		var linkedResp paneEnvResponse
		if err := json.Unmarshal(rLinkedEnv.Body, &linkedResp); err != nil {
			t.Fatal(err)
		}
		if linkedResp.Reason != "" || linkedResp.PID != linkedPID {
			t.Fatalf("linked pane env from %s: expected pid=%d reason='', got %+v", label, linkedPID, linkedResp)
		}
		foundLinked := false
		for _, v := range linkedResp.Vars {
			if v.Name == "CORRAL_LINKED_MARKER" && v.Value == "shared-linked-window" {
				foundLinked = true
			}
		}
		if !foundLinked {
			t.Fatalf("linked pane env from %s missing marker: %+v", label, linkedResp.Vars)
		}

		rLinkedInspect := admin.do("GET", "/api/sessions/"+sid+"/pane-inspect?pane=%253", nil)
		mustStatus(t, rLinkedInspect, http.StatusOK, "GET linked pane inspect from "+label)
		var linkedInspect paneInspectResponse
		if err := json.Unmarshal(rLinkedInspect.Body, &linkedInspect); err != nil {
			t.Fatal(err)
		}
		if linkedInspect.Pane != "%3" || linkedInspect.ShellPID != linkedPID {
			t.Fatalf("linked pane inspect from %s: expected %%3/%d, got %+v", label, linkedPID, linkedInspect)
		}
	}

	// 4. After renaming tmux session "work" -> "work-renamed", own pane %1 and linked pane %3
	//    still succeed via CurrentTmuxSession() and foreign pane %2 is still rejected.
	rRen := admin.do("PUT", "/api/sessions/"+sessID+"/name", map[string]any{"name": "work-renamed"})
	mustStatus(t, rRen, http.StatusOK, "rename tmux session")

	for _, paneQ := range []struct {
		q   string
		pid int
	}{{"%251", ownPID}, {"%253", linkedPID}} {
		rAfter := admin.do("GET", "/api/sessions/"+sessID+"/pane-env?pane="+paneQ.q, nil)
		mustStatus(t, rAfter, http.StatusOK, "GET pane after rename "+paneQ.q)
		var afterResp paneEnvResponse
		if err := json.Unmarshal(rAfter.Body, &afterResp); err != nil {
			t.Fatal(err)
		}
		if afterResp.Reason != "" || afterResp.PID != paneQ.pid {
			t.Fatalf("expected pane %s pid=%d after rename, got %+v", paneQ.q, paneQ.pid, afterResp)
		}
	}

	rAfterForeign := admin.do("GET", "/api/sessions/"+sessID+"/pane-env?pane=%252", nil)
	mustStatus(t, rAfterForeign, http.StatusOK, "GET foreign pane after rename")
	var afterForeignResp paneEnvResponse
	if err := json.Unmarshal(rAfterForeign.Body, &afterForeignResp); err != nil {
		t.Fatal(err)
	}
	if afterForeignResp.Reason != "pane_not_in_session" {
		t.Fatalf("expected foreign pane still rejected after rename, got %+v", afterForeignResp)
	}
}
