package web

// VS Code (code-server) end to end: the scripts run in a real shell under a
// temporary HOME on the test SSH server, and "code-server" is this test
// binary in a fake mode (TestMain) serving HTTP and WebSocket on the socket.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ihainan/corral/internal/codeserver"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/testutil"
)

func TestMain(m *testing.M) {
	if os.Getenv("CORRAL_FAKE_CODE_SERVER") == "1" {
		fakeCodeServer(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// fakeCodeServer stands in for code-server: it listens on --socket, answers
// /healthz, and, like code-server, only accepts a WebSocket whose Origin
// matches the Host.
func fakeCodeServer(args []string) {
	var sock string
	for i, a := range args {
		if a == "--socket" && i+1 < len(args) {
			sock = args[i+1]
		}
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake code-server:", err)
		os.Exit(3)
	}
	time.AfterFunc(2*time.Minute, func() { os.Exit(0) }) // never outlive the test run
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":"alive"}`) })
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "http://"+r.Host {
			http.Error(w, "origin "+o+" does not match host "+r.Host, http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, m, err := c.Read(r.Context())
			if err != nil {
				return
			}
			c.Write(r.Context(), websocket.MessageText, m)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "fake vscode host=%s args=%s", r.Host, strings.Join(args, " "))
	})
	http.Serve(ln, mux)
}

func codeArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// writeCodeRelease builds a release whose bin/code-server runs the fake.
func writeCodeRelease(t *testing.T, path string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	root := "code-server-4.138.0-linux-" + codeArch() + "/"
	script := "#!/bin/sh\nexec \"$CORRAL_TEST_BINARY\" \"$@\"\n"
	tw.WriteHeader(&tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: root + "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: root + "bin/code-server", Mode: 0o755, Size: int64(len(script))})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
}

type codeEnv struct {
	h       *harness
	admin   *client
	owner   *client
	srv     *testutil.SSHServer
	home    string
	hostID  int64
	curlLog string
}

// newCodeEnv sets up a host whose "remote" commands run here under a
// temporary HOME. With upload, the release comes from CORRAL_CODE_SERVER_DIR
// and curl must not be used; otherwise a fake curl hands it out.
func newCodeEnv(t *testing.T, upload bool) *codeEnv {
	t.Helper()
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc (Linux)")
	}
	e := &codeEnv{home: t.TempDir()}
	release := filepath.Join(t.TempDir(), "code-server-4.138.0-linux-"+codeArch()+".tar.gz")
	writeCodeRelease(t, release)
	sum, err := fileSHA256(release)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	e.curlLog = filepath.Join(bin, "curl.log")
	curl := "#!/bin/sh\necho \"$*\" >>" + e.curlLog + "\n"
	if upload {
		curl += "exit 7\n"
	} else {
		curl += "out=\nwhile [ $# -gt 0 ]; do case \"$1\" in -o) shift; out=\"$1\";; esac; shift; done\ncp " + release + " \"$out\"\n"
	}
	os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755)
	e.h = newHarness(t, func(c *config.Config) {
		c.ForwardDomain = "fwd.test"
		c.CodeServer = true
		c.CodeServerVersion = "4.138.0"
		c.CodeServerURL = "https://mirror.test/v{version}/code-server-{version}-linux-{arch}.tar.gz"
		c.CodeServerSHA256 = map[string]string{codeArch(): sum}
		if upload {
			c.CodeServerDir = filepath.Dir(release)
		}
	})
	e.admin = e.h.admin()
	e.owner = e.h.member(e.admin, "alice")
	e.srv = e.h.sshServer(e.home) // SFTP paths start in HOME, as on a real host
	exe, _ := os.Executable()
	e.srv.HandleExec(testutil.ShellExec(e.home, func(cmd string) bool { return strings.HasSuffix(cmd, " "+codeserver.Tag) },
		"CORRAL_FAKE_CODE_SERVER=1", "CORRAL_TEST_BINARY="+exe, "PATH="+bin+":"+os.Getenv("PATH")))
	t.Cleanup(e.killCodeServer)
	e.hostID = e.h.hostFor(e.owner, e.srv.Addr.Port)
	r := e.owner.do("POST", "/api/sessions", map[string]any{"host_id": e.hostID}) // pins the host key
	mustStatus(t, r, 201, "terminal")
	mustStatus(t, e.owner.do("DELETE", "/api/sessions/"+r.json()["id"].(string), nil), 200, "close terminal")
	return e
}

func (e *codeEnv) pid() int {
	b, _ := os.ReadFile(filepath.Join(e.home, ".local/state/corral/code-server.pid"))
	fields := strings.Fields(string(b)) // pid, start time, instance
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fields[0])
	return n
}

func (e *codeEnv) killCodeServer() {
	if pid := e.pid(); pid > 0 {
		syscall.Kill(-pid, syscall.SIGKILL)
		syscall.Kill(pid, syscall.SIGKILL)
	}
	if sock, err := codeserver.SocketPath(e.home, strconv.Itoa(os.Getuid()), ""); err == nil && strings.HasPrefix(sock, "/tmp/corral-") {
		os.RemoveAll(filepath.Dir(sock))
	}
}

// waitCode polls the state until it settles, returning it and every state seen.
func (e *codeEnv) waitCode(c *client) (map[string]any, []string) {
	e.h.t.Helper()
	var seen []string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		r := c.do("GET", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil)
		mustStatus(e.h.t, r, 200, "code status")
		st := r.json()
		s, _ := st["state"].(string)
		if len(seen) == 0 || seen[len(seen)-1] != s {
			seen = append(seen, s)
		}
		if s == codeReady || s == codeFailed {
			return st, seen
		}
	}
	e.h.t.Fatalf("VS Code never settled: %v", seen)
	return nil, nil
}

func (e *codeEnv) start(c *client) (map[string]any, []string) {
	e.h.t.Helper()
	r := c.do("POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil)
	mustStatus(e.h.t, r, http.StatusAccepted, "start VS Code")
	return e.waitCode(c)
}

// openCode follows the ready URL's ticket hand-off to the code subdomain.
func (e *codeEnv) openCode(c *client, st map[string]any) (*fwdClient, string) {
	e.h.t.Helper()
	u, _ := st["url"].(string)
	i := strings.LastIndex(u, "/fwd/")
	if i < 0 {
		e.h.t.Fatalf("no URL when ready: %v", st)
	}
	id := u[i+len("/fwd/"):]
	f, code := e.h.openForward(c, id)
	if code != http.StatusFound {
		e.h.t.Fatalf("open VS Code: %d", code)
	}
	if !strings.HasPrefix(f.host, "code-"+id+".fwd.test") {
		e.h.t.Fatalf("VS Code on %q", f.host)
	}
	return f, id
}

func (e *codeEnv) dialCode(f *fwdClient, id string) (*websocket.Conn, error) {
	hdr := http.Header{}
	hdr.Set("Cookie", "corral_fwd="+f.cookie)
	hdr.Set("Origin", e.h.srv.forwardOriginFor(0, id))
	ws, _, err := websocket.Dial(context.Background(), strings.Replace(e.h.ts.URL, "http", "ws", 1)+"/ws",
		&websocket.DialOptions{HTTPHeader: hdr, Host: f.host})
	return ws, err
}

func TestCodeServerInstallStartReuseStop(t *testing.T) {
	e := newCodeEnv(t, false)
	st, seen := e.start(e.owner)
	if st["state"] != codeReady {
		log, _ := os.ReadFile(filepath.Join(e.home, ".local/state/corral/code-server.log"))
		t.Fatalf("VS Code did not start: %v (%v), log: %s", st, seen, log)
	}
	if strings.Join(seen, ",") == codeReady {
		t.Logf("states seen: %v", seen) // the install can be quicker than one poll
	}
	if b, _ := os.ReadFile(e.curlLog); !strings.Contains(string(b), "https://mirror.test/v4.138.0/code-server-4.138.0-linux-"+codeArch()+".tar.gz") {
		t.Fatalf("download URL: %s", b)
	}
	for _, d := range []string{".local/lib/corral", ".local/share/corral/code-server", ".local/state/corral"} {
		if fi, err := os.Stat(filepath.Join(e.home, d)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi.Mode(), err)
		}
	}

	f, id := e.openCode(e.owner, st)
	res, body := f.get("/", nil)
	if res.StatusCode != 200 || !strings.Contains(body, "fake vscode host=localhost") {
		t.Fatalf("VS Code page: %d %s", res.StatusCode, body)
	}
	for _, want := range []string{"--socket-mode 600", "--auth none", "--disable-telemetry", "--disable-update-check", "--disable-proxy", "--user-data-dir"} {
		if !strings.Contains(body, want) {
			t.Errorf("code-server started without %s: %s", want, body)
		}
	}
	ws, err := e.dialCode(f, id)
	if err != nil {
		t.Fatalf("VS Code WebSocket: %v", err)
	}
	if !echoes(ws) {
		t.Fatal("VS Code WebSocket does not echo")
	}
	ws.CloseNow()

	// Opening again reuses the running code-server (and its subdomain).
	pid := e.pid()
	if pid <= 0 {
		t.Fatal("no pid recorded")
	}
	st2, _ := e.start(e.owner)
	if st2["state"] != codeReady || st2["url"] != st["url"] || e.pid() != pid {
		t.Fatalf("not reused: %v (pid %d, was %d)", st2, e.pid(), pid)
	}

	// Stopping ends it, and the open tab with it.
	ws, _ = e.dialCode(f, id)
	mustStatus(t, e.owner.do("POST", fmt.Sprintf("/api/hosts/%d/code/stop", e.hostID), nil), 200, "stop")
	mustClose(t, ws, "VS Code after stop")
	if err := syscall.Kill(pid, 0); err == nil {
		eventually(t, "code-server still running after stop", func() bool { return syscall.Kill(pid, 0) != nil })
	}
	if r := e.owner.do("GET", fmt.Sprintf("/api/hosts/%d/code", e.hostID), nil); r.json()["state"] != codeStopped {
		t.Fatalf("state after stop: %s", r.Body)
	}
	if res, _ := f.get("/", nil); res.StatusCode == 200 {
		t.Fatal("VS Code still served after stop")
	}
}

func TestCodeServerUploadFromDir(t *testing.T) {
	e := newCodeEnv(t, true)
	st, seen := e.start(e.owner)
	if st["state"] != codeReady {
		t.Fatalf("VS Code did not start from the uploaded release: %v (%v)", st, seen)
	}
	if b, _ := os.ReadFile(e.curlLog); len(b) != 0 {
		t.Fatalf("the host downloaded although the gateway has the release: %s", b)
	}
	if left := e.staging(); len(left) != 0 {
		t.Fatalf("install files left behind: %v", left)
	}
	f, id := e.openCode(e.owner, st)
	ws, err := e.dialCode(f, id)
	if err != nil || !echoes(ws) {
		t.Fatalf("VS Code WebSocket after upload: %v", err)
	}
	ws.CloseNow()
}

// A failing download explains what to do on an intranet.
func TestCodeServerDownloadFailureIsExplained(t *testing.T) {
	e := newCodeEnv(t, false)
	os.WriteFile(filepath.Join(filepath.Dir(e.curlLog), "curl"), []byte("#!/bin/sh\necho 'Could not resolve host: mirror.test' >&2\nexit 6\n"), 0o755)
	st, _ := e.start(e.owner)
	if st["state"] != codeFailed || st["error"] != "download" || !strings.Contains(st["message"].(string), "CORRAL_CODE_SERVER_DIR") ||
		!strings.Contains(st["message"].(string), "resolve") {
		t.Fatalf("failed download: %v", st)
	}
}

func TestCodeServerAccessControl(t *testing.T) {
	e := newCodeEnv(t, false)
	bob := e.h.member(e.admin, "bob")
	for _, c := range []struct{ method, path string }{
		{"GET", fmt.Sprintf("/api/hosts/%d/code", e.hostID)},
		{"POST", fmt.Sprintf("/api/hosts/%d/code", e.hostID)},
		{"POST", fmt.Sprintf("/api/hosts/%d/code/stop", e.hostID)},
	} {
		if r := bob.do(c.method, c.path, nil); r.Status != http.StatusNotFound {
			t.Errorf("bob %s %s: %d", c.method, c.path, r.Status)
		}
	}
	st, _ := e.start(e.owner)
	u := st["url"].(string)
	id := u[strings.LastIndex(u, "/")+1:]
	// Always private: no sharing, not even by the owner, and nobody else gets in.
	if r := e.owner.do("PUT", "/api/forwards/"+id, map[string]any{"sharing": "users"}); r.Status != http.StatusConflict || r.code() != "code_private" {
		t.Fatalf("sharing VS Code: %d %s", r.Status, r.Body)
	}
	if _, code := e.h.openForward(bob, id); code != http.StatusNotFound {
		t.Fatalf("bob opened alice's VS Code: %d", code)
	}
	// Not in the list of forwarded ports.
	if r := e.owner.do("GET", "/api/forwards", nil); strings.Contains(string(r.Body), id) {
		t.Fatalf("code forward listed with ports: %s", r.Body)
	}
	// A port label does not reach a code forward, nor the other way round.
	other := &fwdClient{h: e.h, host: "0-" + id + ".fwd.test", cookie: "x"}
	if _, body := other.get("/", nil); strings.Contains(body, "fake vscode") {
		t.Fatal("code forward served on a port label")
	}
	// Sign-out ends an open VS Code tab.
	f, _ := e.openCode(e.owner, st)
	ws, err := e.dialCode(f, id)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, e.owner.do("POST", "/api/logout", nil), 200, "logout")
	mustClose(t, ws, "VS Code after sign-out")
}

// VS Code runs over a forwarding connection of its own, never the terminal's,
// refuses pages of other origins, and ends with its host.
func TestCodeServerConnectionOriginAndHost(t *testing.T) {
	e := newCodeEnv(t, false)
	r := e.owner.do("POST", "/api/sessions", map[string]any{"host_id": e.hostID})
	mustStatus(t, r, 201, "terminal")
	st, _ := e.start(e.owner)
	if st["state"] != codeReady {
		t.Fatalf("start: %v", st)
	}
	f, id := e.openCode(e.owner, st)
	ws, err := e.dialCode(f, id)
	if err != nil {
		t.Fatal(err)
	}
	e.h.srv.fwd.mu.Lock()
	borrowed, pooled := len(e.h.srv.fwd.borrowed), len(e.h.srv.fwd.conns)
	e.h.srv.fwd.mu.Unlock()
	if borrowed != 0 || pooled != 1 {
		t.Fatalf("VS Code used the terminal's connection (%d borrowed, %d own)", borrowed, pooled)
	}
	// Another forwarded port's page cannot drive it.
	hdr := http.Header{}
	hdr.Set("Cookie", "corral_fwd="+f.cookie)
	hdr.Set("Origin", e.h.srv.forwardOriginFor(8080, "zzzz"))
	if c, _, err := websocket.Dial(context.Background(), strings.Replace(e.h.ts.URL, "http", "ws", 1)+"/ws",
		&websocket.DialOptions{HTTPHeader: hdr, Host: f.host}); err == nil {
		c.CloseNow()
		t.Fatal("WebSocket from another origin accepted")
	}
	// Deleting the host ends the open tab and what Corral knows of it.
	mustStatus(t, e.owner.do("DELETE", fmt.Sprintf("/api/hosts/%d", e.hostID), nil), 200, "delete host")
	mustClose(t, ws, "VS Code after deleting the host")
	if e.h.srv.code.socket(e.h.userID(e.owner), e.hostID) != "" {
		t.Fatal("code-server state kept for a deleted host")
	}
}

func TestCodeServerNeedsSetUp(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ForwardDomain = "fwd.test" })
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	hostID := h.hostFor(admin, srv.Addr.Port)
	if r := admin.do("POST", fmt.Sprintf("/api/hosts/%d/code", hostID), nil); r.Status != http.StatusNotFound || r.code() != "code_disabled" {
		t.Fatalf("code-server off: %d %s", r.Status, r.Body)
	}
	if b := admin.do("GET", "/api/bootstrap", nil).json(); b["code"] != false {
		t.Fatalf("bootstrap: %v", b)
	}
	h2 := newHarness(t, func(c *config.Config) {
		c.ForwardDomain = "fwd.test"
		c.CodeServer = true
		c.CodeServerVersion = "4.138.0"
	})
	admin2 := h2.admin()
	r := admin2.do("POST", "/api/hosts", map[string]any{"name": "ask", "address": "127.0.0.1", "port": srv.Addr.Port, "username": "alice", "auth": "ask"})
	mustStatus(t, r, 201, "host without a stored credential")
	if r := admin2.do("POST", fmt.Sprintf("/api/hosts/%d/code", int64(r.json()["id"].(float64))), nil); r.Status != http.StatusConflict ||
		r.code() != "needs_credential" {
		t.Fatalf("ask host: %d %s", r.Status, r.Body)
	}
	if b := admin2.do("GET", "/api/bootstrap", nil).json(); b["code"] != true {
		t.Fatalf("bootstrap: %v", b)
	}
}

// staging lists what installs leave in the state directory: there should be
// nothing once they have ended.
func (e *codeEnv) staging() []string {
	var left []string
	for _, pat := range []string{"install-*", ".upload-*", ".download-*"} {
		m, _ := filepath.Glob(filepath.Join(e.home, ".local/state/corral", pat))
		left = append(left, m...)
	}
	return left
}
