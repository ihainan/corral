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
		{"abcdef", "••••••••"},
		{"a:b@tcp(localhost:3306)/db", "••••••••"},
		{"sk-ant-secret-999999", "••••••••"},
		{"a\nb\tcdefgh", "••••••••"},
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
		if !ok || !v.Sensitive || v.Value != "" || v.Preview != "••••••••" {
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
		{"json short password", "APP_CONFIG", `{"password":"x"}`, true},
		{"json multiline password", "APP_CONFIG", "{\"password\":\n\"hunter2\"}", true},
		{"json unicode-escaped password", "APP_CONFIG", `{"pass\u0077ord":"hunter2"}`, true},
		{"yaml multiline password", "APP_YAML", "password:\n  hunter2", true},
		{"header api key colon", "EXTRA_HEADER", "X-Api-Key: secret123", true},
		{"header authorization bearer", "EXTRA_HEADER", "Authorization: Bearer abc123", true},
		{"header proxy-authorization basic", "EXTRA_HEADER", "Proxy-Authorization: Basic YWxpY2U6aHVudGVyMg==", true},
		{"yaml token colon", "APP_YAML", "host: db\ntoken: abc123", true},
		{"azure SharedAccessKey", "SERVICE_BUS", "Endpoint=sb://ns.servicebus.windows.net/;SharedAccessKeyName=Root;SharedAccessKey=abc123=", true},
		{"azure AccountKey", "STORAGE_CONN", "DefaultEndpointsProtocol=https;AccountName=dev;AccountKey=abc123==", true},
		{"odbc spaced PWD =", "CONN_STR", "Driver=SQL Server;Server=db;UID=alice;PWD = hunter2", true},
		{"json pwd", "APP_CONFIG", `{"pwd":"hunter2"}`, true},
		{"json spaced pwd colon", "APP_CONFIG", `{"pwd" : "hunter2"}`, true},
		{"json passkey", "APP_CONFIG", `{"passkey":"hunter2"}`, true},
		{"json dotted api.key", "APP_CONFIG", `{"api.key":"hunter2"}`, true},
		{"short PWD=", "SHORT_CONN", "PWD=a", true},

		// Passwords starting with punctuation or literal quotes (must never be treated as empty).
		{"libpq password starting with #", "PGCONN", "host=db password=#hunter2", true},
		{"libpq password starting with ?", "PGCONN", "password=?hunter2", true},
		{"libpq password starting with ]", "PGCONN", "password=]hunter2", true},
		{"libpq password starting with }", "PGCONN", "password=}hunter2", true},
		{"url password starting with comma", "DATABASE_URL", "postgresql://db/app?password=,hunter2", true},
		{"url literal single quotes password", "DATABASE_URL", "postgresql://db/app?password=''", true},

		// Subscripted parameter names.
		{"url subscript empty brackets", "API_URL", "https://example.com/?password[]=x", true},
		{"url subscript named key", "API_URL", "https://example.com/?credentials[db]=x", true},
		{"url nested subscript password", "API_URL", "https://example.com/?user[password]=x", true},

		// Percent-encoded, double-encoded, and encoded credential URLs.
		{"percent-encoded query password", "DATABASE_URL", "https://example.com/?%70assword=hunter2", true},
		{"percent-encoded fragment access_token", "CALLBACK_URL", "https://example.com/#access%5Ftoken=abc123", true},
		{"double-encoded query password", "DATABASE_URL", "https://example.com/?%2570assword=hunter2", true},
		{"percent-encoded credential url in query", "UPSTREAM_URL", "https://proxy.example/?upstream=https%3A%2F%2Falice%3Ahunter2%40db.example", true},
		{"json with percent-encoded password", "APP_CONFIG", `{"%70assword":"x"}`, true},
		{"json unicode then percent password", "APP_CONFIG", `{"\u002570assword":"hunter2"}`, true},
		{"double percent then json unicode password", "APP_CONFIG", `%257B%2522pass%255Cu0077ord%2522%253A%2522hunter2%2522%257D`, true},

		// Fail-closed: any sensitive key with trailing content after '=' / ':' is masked
		// (we do not try to parse syntax-specific empty values mid-string).
		{"mid-string empty libpq password fail-closed", "PGCONN", "host=localhost user=alice password= dbname=app", true},
		{"mid-string empty url password fail-closed", "DATABASE_URL", "postgresql://db/app?password=&user=alice", true},
		{"mid-string empty conn password fail-closed", "CONN_STR", "Server=db;Password=;Database=app", true},
		{"mid-string empty json password fail-closed", "APP_CONFIG", `{"user":"alice","password": ""}`, true},
		{"rust module path with non-log-level secret", "CUSTOM", "myapp::auth=secret123", true},

		// Previously leaked credential shapes (Codex review of b4dae26).
		{"json token_value", "APP_CONFIG", `{"token_value":"hunter2"}`, true},
		{"json camelCase tokenValue", "APP_CONFIG", `{"tokenValue":"abc123"}`, true},
		{"url query token_value", "API_URL", "https://api.example/?token_value=abc123", true},
		{"go mysql dsn without scheme", "DATABASE_URL", "alice:hunter2@tcp(localhost:3306)/app", true},
		{"json escaped slash cred url", "APP_CONFIG", `{"url":"https:\/\/alice:hunter2@db.example"}`, true},
		{"scp-like url with password", "GIT_REMOTE", "alice:hunter2@git.example.com:org/repo.git", true},

		// Accepted fail-closed false positives under the simplified substring rule.
		{"rust log qualified auth module", "RUST_LOG", "myapp::auth=debug,sqlx=info", true},
		{"json token_budget", "APP_CONFIG", `{"token_budget":32768}`, true},
		{"json authorization_enabled", "APP_CONFIG", `{"authorization_enabled":false,"host":"db"}`, true},
		{"mongo authSource", "MONGO_URL", "mongodb://db.internal:27017/app?authSource=admin&retryWrites=true", true},
		{"mongo authMechanism", "MONGO_URL", "mongodb://db.internal:27017/app?authMechanism=SCRAM-SHA-256", true},
		{"azure key name only", "BUS_META", "SharedAccessKeyName=RootManage;Endpoint=sb://ns.example.com/", true},
		{"empty password query trailing EOF", "DATABASE_URL", "postgresql://db/app?user=alice&password=", true},
		{"empty password libpq trailing EOF", "PGCONN", "host=localhost user=alice password=", true},

		// Basic shell/path/locale variables skip keyword substrings, but still catch structural credentials.
		{"HOME with author dir", "HOME", "/home/author", false},
		{"PATH with auth dir segment", "PATH", "/usr/local/sbin:/opt/auth:/usr/bin", false},
		{"PWD with auth dir", "PWD", "/tmp/work/auth/corral", false},
		{"OLDPWD with secret dir", "OLDPWD", "/tmp/secret-project", false},
		{"SHELL with auth path", "SHELL", "/opt/auth/bin/zsh", false},
		{"USER named author", "USER", "author", false},
		{"LOGNAME named author", "LOGNAME", "author", false},
		{"LC_ALL basic", "LC_ALL", "en_US.UTF-8", false},
		{"TMPDIR with token path", "TMPDIR", "/tmp/token-cache", false},
		{"XDG_CONFIG_HOME with auth", "XDG_CONFIG_HOME", "/home/author/.config/auth", false},
		{"XDG_RUNTIME_DIR with auth", "XDG_RUNTIME_DIR", "/run/user/1000/auth", false},
		{"XDG_CACHE_PATH with secret", "XDG_CACHE_PATH", "/var/cache/secret", false},
		{"MANPATH with auth", "MANPATH", "/opt/auth/share/man:/usr/share/man", false},
		{"INFOPATH with auth", "INFOPATH", "/opt/auth/share/info", false},
		{"HOME with embedded dsn cred still masked", "HOME", "alice:hunter2@tcp(localhost:3306)/app", true},
		{"PATH with api key prefix still masked", "PATH", "sk-ant-api03-1234567890abcdefghij", true},
		{"TMPDIR with jwt still masked", "TMPDIR", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", true},
		{"XDG non-path suffix still keyword checked", "XDG_CUSTOM", "password=hunter2", true},

		// Ordinary everyday non-sensitive values must stay visible.
		{"pg url without password", "DATABASE_URL", "postgresql://db.internal:5432/app?sslmode=require&connect_timeout=10", false},
		{"ordinary query params", "DOCS_URL", "https://example.com/docs?page=2&sort=asc&design=dark&keyboard=us", false},
		{"malformed percent sequences", "DOCS_URL", "https://example.com/?foo=100%&bar=%2&baz=%ZZ", false},
		{"git ssh url without password", "GIT_REMOTE", "git@github.com:org/repo.git", false},
		{"HOME path", "HOME", "/tmp/test-home", false},
		{"PATH standard", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", false},
		{"LANG utf8", "LANG", "en_US.UTF-8", false},
		{"TERM xterm", "TERM", "xterm-256color", false},
		{"LS_COLORS", "LS_COLORS", "di=01;34:ln=01;36:so=01;35:pi=33:ex=01;32", false},
		{"EDITOR vim", "EDITOR", "vim", false},
		{"GOPATH", "GOPATH", "/tmp/gopath", false},
		{"PYTHONPATH", "PYTHONPATH", "/opt/app/src:/opt/app/lib:/usr/lib/python3", false},
		{"PWD env var path", "PWD", "/tmp/work/corral", false},
		{"RUST_LOG value", "RUST_LOG", "info,corral=debug", false},
		{"cmake flags non-secret", "CMAKE_FLAGS", "-DKEY_SIZE=16 -DSIGNAL=TERM", false},
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

func TestIsSensitiveEnvLinearTime(t *testing.T) {
	const size = 256 << 10
	bracketChain := "password" + strings.Repeat("[password]", size/len("[password]")) + "="
	colonAt := strings.Repeat("a:", size/2) + "@"
	doublePct := strings.Repeat("%25", size/3)
	rawPct := strings.Repeat("%", size)
	mixedEscape := strings.Repeat(`%25\/`, size/5)
	keySpaces := strings.Repeat("key x ", size/6)
	plainLong := strings.Repeat("x=1;", size/4)

	inputs := []struct {
		name string
		val  string
		want bool
	}{
		{"password_bracket_chain_256k", bracketChain, true},
		{"long_colon_at_256k", colonAt, true},
		{"long_double_percent_256k", doublePct, false},
		{"long_raw_percent_256k", rawPct, false},
		{"long_mixed_percent_json_256k", mixedEscape, false},
		{"long_key_spaces_256k", keySpaces, false},
		{"long_plain_256k", plainLong, false},
	}
	const limit = 50 * time.Millisecond
	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got := isSensitiveEnv("PLAIN_VAR", tc.val)
			elapsed := time.Since(start)
			if got != tc.want {
				t.Errorf("isSensitiveEnv(%s) = %v, want %v", tc.name, got, tc.want)
			}
			if elapsed >= limit {
				t.Errorf("%s took %v, want < %v", tc.name, elapsed, limit)
			}
		})
	}
}

func TestPaneEnvLinearScaling(t *testing.T) {
	const smallSize = 128 << 10
	const largeSize = 256 << 10

	buildCases := []struct {
		name  string
		build func(n int) string
		want  bool
	}{
		{"repeated_percent25", func(n int) string { return strings.Repeat("%25", n/3) }, false},
		{"repeated_percent", func(n int) string { return strings.Repeat("%", n) }, false},
		{"repeated_percent_and_json", func(n int) string { return strings.Repeat(`%25\/`, n/5) }, false},
		{"repeated_bracket_nonmatch", func(n int) string { return "pass" + strings.Repeat("[pass]", n/len("[pass]")) + "=" }, false},
		{"repeated_key_spaces", func(n int) string { return strings.Repeat("key x ", n/6) }, false},
		{"password_bracket_256k", func(n int) string { return "password" + strings.Repeat("[password]", n/len("[password]")) + "=" }, true},
	}

	for _, tc := range buildCases {
		t.Run(tc.name, func(t *testing.T) {
			small := tc.build(smallSize)
			large := tc.build(largeSize)
			// Warm up once and verify classification.
			if got := isSensitiveEnv("PLAIN_VAR", small); got != tc.want {
				t.Fatalf("small got %v, want %v", got, tc.want)
			}
			if got := isSensitiveEnv("PLAIN_VAR", large); got != tc.want {
				t.Fatalf("large got %v, want %v", got, tc.want)
			}
			// Pick enough iterations so the 128 KiB run clears timer/scheduler noise.
			iters := 0
			var dSmall time.Duration
			start := time.Now()
			for iters < 200 && time.Since(start) < 10*time.Millisecond {
				isSensitiveEnv("PLAIN_VAR", small)
				iters++
			}
			dSmall = time.Since(start)

			startLarge := time.Now()
			for i := 0; i < iters; i++ {
				isSensitiveEnv("PLAIN_VAR", large)
			}
			dLarge := time.Since(startLarge)

			perCallLarge := dLarge / time.Duration(iters)
			if perCallLarge >= 50*time.Millisecond {
				t.Errorf("%s 256 KiB took %v/op, want < 50ms", tc.name, perCallLarge)
			}
			if dSmall >= 2*time.Millisecond {
				ratio := float64(dLarge) / float64(dSmall)
				if ratio > 2.5 {
					t.Errorf("%s scaling ratio 256KiB/128KiB = %.2fx (%v vs %v over %d iters), want <= 2.5x",
						tc.name, ratio, dLarge, dSmall, iters)
				}
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

// TestTmuxPaneEnvAndInspectRealTmux exercises pane-env, pane-env/reveal, and
// pane-inspect against a real tmux server (isolated via its own -L socket and
// TMUX_TMPDIR, killed in t.Cleanup) covering:
//  1. A window in another session named identically to this session ("work"),
//     verifying "=work:" resolves the session rather than that window.
//  2. A linked window shared by two sessions, readable from both.
//  3. External `tmux rename-session` + reuse of the old name, verifying the
//     bound #{session_id} refuses both pane and no-pane reads of the new session.
func TestTmuxPaneEnvAndInspectRealTmux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc/<pid>/environ required")
	}
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}

	// Keep the tmux socket path short (<108 bytes sun_path limit even when GOTMPDIR is long).
	tmuxTmp, err := os.MkdirTemp("/tmp", "ctm-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmuxTmp, 0o700); err != nil {
		_ = os.RemoveAll(tmuxTmp)
		t.Fatal(err)
	}
	sockName := fmt.Sprintf("ct-%d-%d", os.Getpid(), time.Now().UnixNano()%1000000)
	runTmux := func(args ...string) string {
		t.Helper()
		full := append([]string{"-L", sockName}, args...)
		cmd := exec.Command(realTmux, full...)
		cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxTmp)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		cmd := exec.Command(realTmux, "-L", sockName, "kill-server")
		cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxTmp)
		_ = cmd.Run()
		_ = os.RemoveAll(tmuxTmp)
	})

	// 1. Session "work" with window "normal", and session "other" with a window
	//    literally named "work" (the exact window-target trap for '=work').
	runTmux("-f", "/dev/null", "new-session", "-d", "-s", "work", "-n", "normal",
		"sh", "-c", "export CORRAL_WORK_VAR=from-work; exec sleep 60")
	runTmux("new-session", "-d", "-s", "other", "-n", "work",
		"sh", "-c", "export CORRAL_OTHER_SECRET=secret-from-other; exec sleep 60")
	// Add a second window "shared" in "other" and link it into "work".
	runTmux("new-window", "-d", "-t", "=other:", "-n", "shared",
		"sh", "-c", "export CORRAL_LINKED_VAR=from-linked-win; exec sleep 60")
	runTmux("link-window", "-d", "-s", "=other:shared", "-t", "=work:")

	otherID := runTmux("display-message", "-p", "-t", "=other:", "#{session_id}")
	workID := runTmux("display-message", "-p", "-t", "=work:", "#{session_id}")
	workPane := runTmux("display-message", "-p", "-t", "=work:normal", "#{pane_id}")
	otherPane := runTmux("display-message", "-p", "-t", "=other:work", "#{pane_id}")
	linkedPane := runTmux("display-message", "-p", "-t", "=other:shared", "#{pane_id}")

	// Build a wrapper tmux on the fake SSH host's PATH pointing to our isolated -L server.
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexport TMUX_TMPDIR=%q\nfor a in \"$@\"; do\n  case \"$a\" in\n    new-session|attach-session) read x; exit 0;;\n  esac\ndone\nexec %q -L %q \"$@\"\n", tmuxTmp, realTmux, sockName)
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(t.TempDir(), "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "sed", "cut", "tr", "head", "ps"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(sys, tool))
		}
	}

	h := newHarness(t)
	admin := h.admin()
	srv := h.sshServer(home)
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool {
		return strings.Contains(cmd, "tmux") || strings.Contains(cmd, "/proc/")
	}, "PATH="+sys))

	rHost := admin.do("POST", "/api/hosts", map[string]any{
		"name": "real-tmux-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	hostID := rHost.json()["id"].(float64)

	rWork := admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "work"},
	})
	mustStatus(t, rWork, http.StatusCreated, "open work session")
	workSessID := rWork.json()["id"].(string)

	rOther := admin.do("POST", "/api/sessions", map[string]any{
		"host_id": hostID,
		"open":    map[string]any{"kind": "tmux", "name": "other"},
	})
	mustStatus(t, rOther, http.StatusCreated, "open other session")
	otherSessID := rOther.json()["id"].(string)

	// Session ID is NOT looked up on open (no extra remote command at open);
	// it binds lazily on the first pane-env or pane-inspect call.
	me := admin.do("GET", "/api/me", nil)
	mustStatus(t, me, http.StatusOK, "me")
	uid := int64(me.json()["id"].(float64))
	workSess, err := h.srv.terms.Get(uid, workSessID)
	if err != nil {
		t.Fatal(err)
	}
	otherSess, err := h.srv.terms.Get(uid, otherSessID)
	if err != nil {
		t.Fatal(err)
	}
	if got := workSess.CurrentTmuxSessionID(); got != "" {
		t.Fatalf("expected unbound tmux session_id immediately after open, got %q", got)
	}

	// Wait briefly for the spawned `exec sleep 60` processes to have their exported vars in /proc/<pid>/environ.
	waitEnvVar := func(sid, pane, wantName, wantVal string) paneEnvResponse {
		t.Helper()
		q := ""
		if pane != "" {
			q = "?pane=" + strings.ReplaceAll(pane, "%", "%25")
		}
		deadline := time.Now().Add(3 * time.Second)
		var last paneEnvResponse
		for time.Now().Before(deadline) {
			r := admin.do("GET", "/api/sessions/"+sid+"/pane-env"+q, nil)
			mustStatus(t, r, http.StatusOK, "GET pane-env")
			if err := json.Unmarshal(r.Body, &last); err != nil {
				t.Fatal(err)
			}
			for _, v := range last.Vars {
				if v.Name == wantName && v.Value == wantVal {
					return last
				}
			}
			time.Sleep(40 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s=%s on sid=%s pane=%s; last=%+v", wantName, wantVal, sid, pane, last)
		return last
	}

	// --- Case 1: Window in "other" named "work" must NOT fool membership check ---
	waitEnvVar(workSessID, workPane, "CORRAL_WORK_VAR", "from-work")
	if workSess.CurrentTmuxSessionID() != workID {
		t.Fatalf("expected tmux session_id bound to %q on first use, got %q", workID, workSess.CurrentTmuxSessionID())
	}
	waitEnvVar(workSessID, "", "CORRAL_WORK_VAR", "from-work") // no-pane path

	rBadEnv := admin.do("GET", "/api/sessions/"+workSessID+"/pane-env?pane="+strings.ReplaceAll(otherPane, "%", "%25"), nil)
	mustStatus(t, rBadEnv, http.StatusOK, "GET other:work pane from work")
	if strings.Contains(string(rBadEnv.Body), "secret-from-other") || strings.Contains(string(rBadEnv.Body), "CORRAL_OTHER_SECRET") {
		t.Fatalf("other session's window named 'work' leaked env: %s", rBadEnv.Body)
	}
	var badEnv paneEnvResponse
	if err := json.Unmarshal(rBadEnv.Body, &badEnv); err != nil {
		t.Fatal(err)
	}
	if badEnv.Reason != "pane_not_in_session" {
		t.Fatalf("expected pane_not_in_session for other:work pane %s, got %+v", otherPane, badEnv)
	}

	rBadRev := admin.do("POST", "/api/sessions/"+workSessID+"/pane-env/reveal", map[string]any{
		"pane": otherPane,
		"name": "CORRAL_OTHER_SECRET",
	})
	mustStatus(t, rBadRev, http.StatusConflict, "reveal other:work pane rejected")
	if rBadRev.code() != "pane_not_in_session" {
		t.Fatalf("expected pane_not_in_session reveal code, got %s", rBadRev.Body)
	}

	rBadInspect := admin.do("GET", "/api/sessions/"+workSessID+"/pane-inspect?pane="+strings.ReplaceAll(otherPane, "%", "%25"), nil)
	mustStatus(t, rBadInspect, http.StatusOK, "inspect other:work pane")
	var badInspect paneInspectResponse
	if err := json.Unmarshal(rBadInspect.Body, &badInspect); err != nil {
		t.Fatal(err)
	}
	if badInspect.Pane != "" || badInspect.ShellPID != 0 {
		t.Fatalf("expected empty inspect for other:work pane %s, got %+v", otherPane, badInspect)
	}

	// --- Case 2: Linked window pane readable from BOTH "work" and "other" ---
	// First use of "other" via pane-inspect must lazily bind its session_id too.
	if got := otherSess.CurrentTmuxSessionID(); got != "" {
		t.Fatalf("expected unbound other session_id before first use, got %q", got)
	}
	rOtherFirst := admin.do("GET", "/api/sessions/"+otherSessID+"/pane-inspect?pane="+strings.ReplaceAll(linkedPane, "%", "%25"), nil)
	mustStatus(t, rOtherFirst, http.StatusOK, "inspect linked pane from other (first use)")
	if got := otherSess.CurrentTmuxSessionID(); got != otherID {
		t.Fatalf("expected other tmux session_id bound to %q on first pane-inspect, got %q", otherID, got)
	}
	for label, sid := range map[string]string{"work": workSessID, "other": otherSessID} {
		waitEnvVar(sid, linkedPane, "CORRAL_LINKED_VAR", "from-linked-win")
		rLI := admin.do("GET", "/api/sessions/"+sid+"/pane-inspect?pane="+strings.ReplaceAll(linkedPane, "%", "%25"), nil)
		mustStatus(t, rLI, http.StatusOK, "inspect linked pane from "+label)
		var li paneInspectResponse
		if err := json.Unmarshal(rLI.Body, &li); err != nil {
			t.Fatal(err)
		}
		if li.Pane != linkedPane || li.ShellPID <= 0 {
			t.Fatalf("linked pane inspect from %s failed: got %+v want pane %s", label, li, linkedPane)
		}
	}

	// Valid Corral rename on a bound session succeeds and preserves its bound #{session_id}.
	rValidRename := admin.do("PUT", "/api/sessions/"+otherSessID+"/name", map[string]any{"name": "other-renamed"})
	mustStatus(t, rValidRename, http.StatusOK, "valid Corral rename on bound session")
	if got := otherSess.CurrentTmuxSessionID(); got != otherID {
		t.Fatalf("bound tmux session_id changed after valid rename: got %q want %q", got, otherID)
	}
	if got := runTmux("display-message", "-p", "-t", otherID+":", "#{session_name}"); got != "other-renamed" {
		t.Fatalf("remote tmux session was not renamed: got %q want other-renamed", got)
	}

	// --- Case 3: External rename + name reuse must be refused via bound #{session_id} ---
	runTmux("rename-session", "-t", "=work:", "archive")
	runTmux("new-session", "-d", "-s", "work", "-n", "hijack",
		"sh", "-c", "export CORRAL_HIJACK_SECRET=from-new-work; exec sleep 60")
	newWorkPane := runTmux("display-message", "-p", "-t", "=work:hijack", "#{pane_id}")

	// Explicit pane of the new "work" session -> refused with session_mismatch.
	rHijackPane := admin.do("GET", "/api/sessions/"+workSessID+"/pane-env?pane="+strings.ReplaceAll(newWorkPane, "%", "%25"), nil)
	mustStatus(t, rHijackPane, http.StatusOK, "GET pane-env on reused name with pane")
	if strings.Contains(string(rHijackPane.Body), "from-new-work") || strings.Contains(string(rHijackPane.Body), "CORRAL_HIJACK_SECRET") {
		t.Fatalf("reused tmux session name leaked env: %s", rHijackPane.Body)
	}
	var hijackResp paneEnvResponse
	if err := json.Unmarshal(rHijackPane.Body, &hijackResp); err != nil {
		t.Fatal(err)
	}
	if hijackResp.Reason != "session_mismatch" {
		t.Fatalf("expected session_mismatch on reused name (pane path), got %+v", hijackResp)
	}

	// Reveal on reused name -> 409 session_mismatch.
	rHijackRev := admin.do("POST", "/api/sessions/"+workSessID+"/pane-env/reveal", map[string]any{
		"pane": newWorkPane,
		"name": "CORRAL_HIJACK_SECRET",
	})
	mustStatus(t, rHijackRev, http.StatusConflict, "reveal on reused name rejected")
	if rHijackRev.code() != "session_mismatch" {
		t.Fatalf("expected session_mismatch on reveal, got %s", rHijackRev.Body)
	}

	// No-pane path on reused name -> refused with session_mismatch.
	rHijackNoPane := admin.do("GET", "/api/sessions/"+workSessID+"/pane-env", nil)
	mustStatus(t, rHijackNoPane, http.StatusOK, "GET pane-env on reused name without pane")
	if strings.Contains(string(rHijackNoPane.Body), "from-new-work") || strings.Contains(string(rHijackNoPane.Body), "CORRAL_HIJACK_SECRET") {
		t.Fatalf("reused tmux session name leaked env on no-pane path: %s", rHijackNoPane.Body)
	}
	var hijackNoPane paneEnvResponse
	if err := json.Unmarshal(rHijackNoPane.Body, &hijackNoPane); err != nil {
		t.Fatal(err)
	}
	if hijackNoPane.Reason != "session_mismatch" {
		t.Fatalf("expected session_mismatch on no-pane path, got %+v", hijackNoPane)
	}

	// pane-inspect on reused name (both with pane and no-pane) -> refused (empty).
	for _, q := range []string{"?pane=" + strings.ReplaceAll(newWorkPane, "%", "%25"), ""} {
		rHI := admin.do("GET", "/api/sessions/"+workSessID+"/pane-inspect"+q, nil)
		mustStatus(t, rHI, http.StatusOK, "pane-inspect on reused name "+q)
		var hi paneInspectResponse
		if err := json.Unmarshal(rHI.Body, &hi); err != nil {
			t.Fatal(err)
		}
		if hi.Pane != "" || hi.ShellPID != 0 {
			t.Fatalf("expected empty pane-inspect on reused name (%q), got %+v", q, hi)
		}
	}

	// Corral rename on old session after external rename+reuse MUST be refused (409 session_mismatch),
	// leaving the bound ID unchanged and the replacement session untouched.
	newWorkID := runTmux("display-message", "-p", "-t", "=work:", "#{session_id}")
	rHijackRename := admin.do("PUT", "/api/sessions/"+workSessID+"/name", map[string]any{"name": "should-not-rename"})
	mustStatus(t, rHijackRename, http.StatusConflict, "Corral rename refused after external rename+reuse")
	if rHijackRename.code() != "session_mismatch" {
		t.Fatalf("expected session_mismatch on Corral rename after reuse, got %s", rHijackRename.Body)
	}
	if got := workSess.CurrentTmuxSessionID(); got != workID {
		t.Fatalf("bound tmux session_id changed after refused rename: got %q want %q", got, workID)
	}
	if got := workSess.CurrentTmuxSession(); got != "work" {
		t.Fatalf("session name changed after refused rename: got %q want work", got)
	}
	if got := runTmux("display-message", "-p", "-t", newWorkID+":", "#{session_name}"); got != "work" {
		t.Fatalf("replacement tmux session was wrongly renamed to %q", got)
	}

	// Also verify external rename WITHOUT name reuse refuses cleanly with session_mismatch.
	runTmux("kill-session", "-t", "=work:")
	rRenamedOnly := admin.do("GET", "/api/sessions/"+workSessID+"/pane-env", nil)
	mustStatus(t, rRenamedOnly, http.StatusOK, "GET pane-env after external rename without reuse")
	var renamedOnly paneEnvResponse
	if err := json.Unmarshal(rRenamedOnly.Body, &renamedOnly); err != nil {
		t.Fatal(err)
	}
	if renamedOnly.Reason != "session_mismatch" {
		t.Fatalf("expected session_mismatch when original id exists under another name, got %+v", renamedOnly)
	}
}

func TestTmuxPaneEnvAndInspectConcurrentBindConflict(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	home := t.TempDir()
	srv := h.sshServer(home)

	envStarted := make(chan struct{})
	envRelease := make(chan struct{})
	revealStarted := make(chan struct{})
	revealRelease := make(chan struct{})
	inspectStarted := make(chan struct{})
	inspectRelease := make(chan struct{})

	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if strings.Contains(cmd, "new-session") || strings.Contains(cmd, "attach-session") {
			return func(ch ssh.Channel) {
				buf := make([]byte, 16)
				_, _ = ch.Read(buf)
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		if strings.Contains(cmd, "/proc/$pid/environ") {
			return func(ch ssh.Channel) {
				// Distinguish first call (pane-env) from second (reveal) by checking channel state.
				select {
				case <-envRelease:
					close(revealStarted)
					<-revealRelease
				default:
					close(envStarted)
					<-envRelease
				}
				_, _ = ch.Write([]byte("ID\t$2\nOK\t4242\tsh\nSECRET_FROM_REPLACEMENT=super-secret-leak\x00"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		if strings.Contains(cmd, "#{pane_id}|#{pane_pid}|#{pane_tty}") {
			return func(ch ssh.Channel) {
				close(inspectStarted)
				<-inspectRelease
				_, _ = ch.Write([]byte("ID\t$2\npane\t%9|4242|/dev/pts/1|sh|0\nproc\t4242 sh sh\n"))
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			}
		}
		return nil
	})

	rHost := admin.do("POST", "/api/hosts", map[string]any{
		"name": "race-box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass",
	})
	mustStatus(t, rHost, http.StatusCreated, "create host")
	hostID := rHost.json()["id"].(float64)

	openUnbound := func() (string, int64) {
		rSess := admin.do("POST", "/api/sessions", map[string]any{
			"host_id": hostID,
			"open":    map[string]any{"kind": "tmux", "name": "work"},
		})
		mustStatus(t, rSess, http.StatusCreated, "open tmux work")
		me := admin.do("GET", "/api/me", nil)
		mustStatus(t, me, http.StatusOK, "me")
		return rSess.json()["id"].(string), int64(me.json()["id"].(float64))
	}

	// 1. Concurrent bind conflict on GET /pane-env: request starts unbound, another
	//    request binds "$0" while in flight, remote returns ID "$2" -> must refuse
	//    with session_mismatch and return zero vars.
	sid1, uid := openUnbound()
	sess1, err := h.srv.terms.Get(uid, sid1)
	if err != nil {
		t.Fatal(err)
	}
	envDone := make(chan resp, 1)
	go func() {
		envDone <- admin.do("GET", "/api/sessions/"+sid1+"/pane-env", nil)
	}()
	<-envStarted
	if !sess1.SetTmuxSessionID("$0") {
		t.Fatal("expected initial bind to $0")
	}
	close(envRelease)
	rEnv := <-envDone
	mustStatus(t, rEnv, http.StatusOK, "GET pane-env on bind conflict")
	if strings.Contains(string(rEnv.Body), "super-secret-leak") || strings.Contains(string(rEnv.Body), "SECRET_FROM_REPLACEMENT") {
		t.Fatalf("pane-env leaked replacement session vars on bind conflict: %s", rEnv.Body)
	}
	var envResp paneEnvResponse
	if err := json.Unmarshal(rEnv.Body, &envResp); err != nil {
		t.Fatal(err)
	}
	if envResp.Reason != "session_mismatch" || len(envResp.Vars) != 0 || envResp.PID != 0 {
		t.Fatalf("expected session_mismatch with empty vars/pid, got %+v", envResp)
	}
	if got := sess1.CurrentTmuxSessionID(); got != "$0" {
		t.Fatalf("bound id changed to %q, want $0", got)
	}

	// 2. Concurrent bind conflict on POST /pane-env/reveal (fresh unbound session):
	//    must return 409 session_mismatch without revealing the secret.
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+sid1, nil), http.StatusOK, "close sid1")
	sid2, _ := openUnbound()
	sess2, err := h.srv.terms.Get(uid, sid2)
	if err != nil {
		t.Fatal(err)
	}
	revDone := make(chan resp, 1)
	go func() {
		revDone <- admin.do("POST", "/api/sessions/"+sid2+"/pane-env/reveal", map[string]any{
			"name": "SECRET_FROM_REPLACEMENT",
		})
	}()
	<-revealStarted
	if !sess2.SetTmuxSessionID("$0") {
		t.Fatal("expected initial bind to $0")
	}
	close(revealRelease)
	rRev := <-revDone
	mustStatus(t, rRev, http.StatusConflict, "POST reveal on bind conflict")
	if rRev.code() != "session_mismatch" || strings.Contains(string(rRev.Body), "super-secret-leak") {
		t.Fatalf("reveal did not reject bind conflict cleanly: %s", rRev.Body)
	}
	if got := sess2.CurrentTmuxSessionID(); got != "$0" {
		t.Fatalf("bound id changed to %q, want $0", got)
	}

	// 3. Concurrent bind conflict on GET /pane-inspect (fresh unbound session):
	//    must return 409 session_mismatch without returning inspect payload.
	mustStatus(t, admin.do("DELETE", "/api/sessions/"+sid2, nil), http.StatusOK, "close sid2")
	sid3, _ := openUnbound()
	sess3, err := h.srv.terms.Get(uid, sid3)
	if err != nil {
		t.Fatal(err)
	}
	inspDone := make(chan resp, 1)
	go func() {
		inspDone <- admin.do("GET", "/api/sessions/"+sid3+"/pane-inspect", nil)
	}()
	<-inspectStarted
	if !sess3.SetTmuxSessionID("$0") {
		t.Fatal("expected initial bind to $0")
	}
	close(inspectRelease)
	rInsp := <-inspDone
	mustStatus(t, rInsp, http.StatusConflict, "GET pane-inspect on bind conflict")
	if rInsp.code() != "session_mismatch" || strings.Contains(string(rInsp.Body), "4242") || strings.Contains(string(rInsp.Body), "%9") {
		t.Fatalf("pane-inspect did not reject bind conflict cleanly: %s", rInsp.Body)
	}
	if got := sess3.CurrentTmuxSessionID(); got != "$0" {
		t.Fatalf("bound id changed to %q, want $0", got)
	}
}
