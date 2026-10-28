package web

// These tests encode the six constraints from the design review. Each one is a
// class of bug that comparable projects shipped in 2025–2026; they run in CI so
// a regression fails the build rather than reaching users.

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/sshx"
)

// 0. Every API route declares an access level, and the public surface is exactly what we expect.
func TestRouteTableIsExplicit(t *testing.T) {
	h := newHarness(t)
	public := map[string]bool{
		"GET /api/bootstrap": true, "POST /api/setup": true, "POST /api/login": true,
		"POST /api/login/code":     true, // the code step; needs the token the password step returned
		"GET /api/invites/{token}": true, "POST /api/invites/{token}/redeem": true,
		"GET /fwd/{id}": true, // checks the sign-in itself and redirects to it
	}
	for _, r := range h.srv.Routes() {
		key := r.Method + " " + r.Pattern
		if r.Access == Public && !public[key] {
			t.Errorf("route %s is public but not in the reviewed public list", key)
		}
		if strings.HasPrefix(r.Pattern, "/api/admin/") && r.Access != AdminOnly {
			t.Errorf("admin route %s is not admin-only", key)
		}
	}
	// Host-scoped routes added later are signed-in (and owner-checked by ownedHost).
	signedIn := map[string]bool{"GET /api/hosts/{id}/code": false, "POST /api/hosts/{id}/code": false, "POST /api/hosts/{id}/code/stop": false}
	for _, r := range h.srv.Routes() {
		if _, ok := signedIn[r.Method+" "+r.Pattern]; ok {
			signedIn[r.Method+" "+r.Pattern] = r.Access == SignedIn
		}
	}
	for k, ok := range signedIn {
		if !ok {
			t.Errorf("route %s missing or not signed-in", k)
		}
	}
	// An unknown API path must not fall through to anything else.
	c := &client{h: h}
	mustStatus(t, c.do("GET", "/api/hosts/1/../../admin/users", nil), http.StatusUnauthorized, "path traversal on API")
}

// 1. Ownership: user B can never see or touch user A's objects; every miss is "not found".
func TestCrossUserAccessIsNotFound(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	a := h.member(admin, "alice")
	b := h.member(admin, "bob")
	srv := h.sshServer(t.TempDir())

	r := a.do("POST", "/api/credentials", map[string]any{"name": "laptop", "mode": "generate"})
	mustStatus(t, r, 201, "create key")
	keyID := int64(r.json()["id"].(float64))

	r = a.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass"})
	mustStatus(t, r, 201, "create host")
	hostID := int64(r.json()["id"].(float64))

	r = a.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, r, 201, "open session")
	sessID := r.json()["id"].(string)
	mustStatus(t, a.upload(sessID, []byte("\x89PNG\r\n\x1a\nfake")), 201, "owner upload")

	hostPath := fmt.Sprintf("/api/hosts/%d", hostID)
	checks := []struct {
		method, path string
		body         any
	}{
		{"GET", hostPath, nil},
		{"PUT", hostPath, map[string]any{"name": "x", "address": "127.0.0.1", "username": "bob", "auth": "ask"}},
		{"DELETE", hostPath, nil},
		{"GET", hostPath + "/hostkey", nil},
		{"DELETE", hostPath + "/hostkey", nil},
		{"PUT", fmt.Sprintf("/api/credentials/%d", keyID), map[string]any{"name": "mine"}},
		{"DELETE", fmt.Sprintf("/api/credentials/%d", keyID), nil},
		{"POST", "/api/sessions", map[string]any{"host_id": hostID}},
		{"DELETE", "/api/sessions/" + sessID, nil},
		{"GET", "/api/sessions/" + sessID + "/ws", nil},
	}
	for _, c := range checks {
		r := b.do(c.method, c.path, c.body)
		if r.Status != http.StatusNotFound {
			t.Errorf("bob %s %s: status %d, want 404: %s", c.method, c.path, r.Status, r.Body)
		}
	}
	if r := b.upload(sessID, []byte("x")); r.Status != http.StatusNotFound {
		t.Errorf("bob upload into alice's session: %d", r.Status)
	}
	// Referencing someone else's key from your own host is rejected as invalid.
	r = b.do("POST", "/api/hosts", map[string]any{"name": "steal", "address": "127.0.0.1", "username": "alice", "auth": "key", "credential_id": keyID})
	mustStatus(t, r, http.StatusUnprocessableEntity, "bob using alice's key")

	// Bob's lists show nothing of Alice's.
	for _, p := range []string{"/api/hosts", "/api/credentials", "/api/sessions"} {
		if r := b.do("GET", p, nil); strings.TrimSpace(string(r.Body)) != "[]" {
			t.Errorf("bob GET %s leaked: %s", p, r.Body)
		}
	}
	// And Alice's objects are still intact.
	mustStatus(t, a.do("GET", hostPath, nil), 200, "alice still owns host")
	if a.do("GET", "/api/sessions", nil).Body[0] != '[' {
		t.Error("alice session list broken")
	}
}

// 2. No endpoint ever returns stored secrets, including admin and audit views.
func TestSecretsNeverLeaveTheServer(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	a := h.member(admin, "alice")
	const password = "Sup3r-Secret-Host-Pass"
	const passphrase = "key-phrase-xyz"

	r := a.do("POST", "/api/credentials", map[string]any{"name": "gen", "mode": "generate"})
	mustStatus(t, r, 201, "generate key")
	r = a.do("POST", "/api/hosts", map[string]any{"name": "h", "address": "127.0.0.1", "username": "alice", "auth": "password", "password": password})
	mustStatus(t, r, 201, "create host")
	hostID := int64(r.json()["id"].(float64))
	// Rotate the password too, which re-seals it.
	mustStatus(t, a.do("PUT", fmt.Sprintf("/api/hosts/%d", hostID), map[string]any{"name": "h", "address": "127.0.0.1",
		"username": "alice", "auth": "password", "password": password + "2"}), 200, "update host")

	var bodies []string
	for _, p := range []string{"/api/hosts", fmt.Sprintf("/api/hosts/%d", hostID), "/api/credentials", "/api/me", "/api/me/settings", "/api/sessions", "/api/bootstrap"} {
		bodies = append(bodies, string(a.do("GET", p, nil).Body))
	}
	for _, p := range []string{"/api/admin/users", "/api/admin/audit"} {
		bodies = append(bodies, string(admin.do("GET", p, nil).Body))
	}
	for i, body := range bodies {
		for _, needle := range []string{password, passphrase, "PRIVATE KEY", "ciphertext", "wrapped_key", "password_hash", "argon2"} {
			if strings.Contains(body, needle) {
				t.Errorf("response %d contains %q: %s", i, needle, body)
			}
		}
	}
}

// 3. The gateway connects only to permitted networks, checked after resolution.
func TestTargetsOutsideAllowedNetworksAreRefused(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.AllowedNetworks = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	admin := h.admin()
	srv := h.sshServer(t.TempDir())
	for _, addr := range []string{"127.0.0.1", "localhost", "169.254.169.254", "::ffff:169.254.169.254", "0.0.0.0"} {
		r := admin.do("POST", "/api/hosts", map[string]any{"name": addr, "address": addr, "port": srv.Addr.Port, "username": "alice", "auth": "ask"})
		if r.Status != 201 {
			t.Fatalf("create host %s: %d %s", addr, r.Status, r.Body)
		}
		id := int64(r.json()["id"].(float64))
		r = admin.do("POST", "/api/sessions", map[string]any{"host_id": id, "password": "alice-pass"})
		if r.Status != http.StatusForbidden || r.code() != "target_forbidden" {
			t.Errorf("connect to %s: status %d %s, want 403 target_forbidden", addr, r.Status, r.Body)
		}
	}

	// An install that allows everything still never reaches denied ranges, in any spelling.
	open := newHarness(t, func(c *config.Config) {
		c.AllowedNetworks = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	})
	oadmin := open.admin()
	for _, addr := range []string{"169.254.169.254", "::ffff:169.254.169.254", "::ffff:a9fe:a9fe"} {
		r := oadmin.do("POST", "/api/hosts", map[string]any{"name": addr, "address": addr, "username": "alice", "auth": "ask"})
		if r.Status != 201 {
			t.Fatalf("create host %s: %d %s", addr, r.Status, r.Body)
		}
		r = oadmin.do("POST", "/api/sessions", map[string]any{"host_id": int64(r.json()["id"].(float64)), "password": "x"})
		if r.Status != http.StatusForbidden {
			t.Errorf("allow-all install reached denied %s: %d %s", addr, r.Status, r.Body)
		}
	}
}

// 4. Client-supplied forwarding headers are ignored unless the peer is a trusted proxy.
func TestForwardedHeadersAreNotTrustedByDefault(t *testing.T) {
	h := newHarness(t)
	h.admin()
	c := &client{h: h, extra: map[string]string{}}
	limited := false
	for i := 0; i < 40; i++ {
		c.extra["X-Forwarded-For"] = fmt.Sprintf("10.9.%d.%d", i/250, i%250)
		r := c.do("POST", "/api/login", map[string]string{"username": fmt.Sprintf("nobody%d", i), "password": "wrong-password"})
		if r.code() == "rate_limited" {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("rotating X-Forwarded-For bypassed the per-address login limit")
	}
	// The audit trail records the real peer, not the claimed one.
	entries, _ := h.store.ListAudit(context.Background(), storeAuditQuery())
	for _, e := range entries {
		if strings.HasPrefix(e.IP, "10.9.") {
			t.Fatalf("audit recorded spoofed address %s", e.IP)
		}
	}

	// With the peer configured as a trusted proxy, the header is honoured.
	h2 := newHarness(t, func(c *config.Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	})
	h2.admin()
	c2 := &client{h: h2, extra: map[string]string{}}
	for i := 0; i < 40; i++ {
		c2.extra["X-Forwarded-For"] = fmt.Sprintf("10.9.%d.%d", i/250, i%250)
		if r := c2.do("POST", "/api/login", map[string]string{"username": fmt.Sprintf("nobody%d", i), "password": "wrong-password"}); r.code() == "rate_limited" {
			t.Fatal("trusted proxy: distinct client addresses should not share a limit")
		}
	}
}

// 5. Disabling a user ends everything at once: logins, terminals, sockets.
func TestDisablingUserEndsAllAccess(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	a := h.member(admin, "alice")
	srv := h.sshServer(t.TempDir())
	r := a.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port, "username": "alice", "auth": "password", "password": "alice-pass"})
	mustStatus(t, r, 201, "create host")
	r = a.do("POST", "/api/sessions", map[string]any{"host_id": int64(r.json()["id"].(float64))})
	mustStatus(t, r, 201, "open session")
	sessID := r.json()["id"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := strings.Replace(h.ts.URL, "http", "ws", 1) + "/api/sessions/" + sessID + "/ws"
	hdr := http.Header{}
	hdr.Set("Origin", h.cfg.Origin())
	hdr.Set("Cookie", h.srv.cookieName()+"="+a.cookie)
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if _, _, err := ws.Read(ctx); err != nil { // hello
		t.Fatal(err)
	}

	me := a.do("GET", "/api/me", nil)
	aliceID := int64(me.json()["id"].(float64))
	mustStatus(t, admin.do("PUT", fmt.Sprintf("/api/admin/users/%d", aliceID), map[string]any{"disabled": true}), 200, "disable")

	closed := make(chan struct{})
	go func() {
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				close(closed)
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal socket stayed open after the user was disabled")
	}
	mustStatus(t, a.do("GET", "/api/me", nil), http.StatusUnauthorized, "disabled user's cookie")
	for _, s := range h.terms.List(aliceID) {
		if s.State == "running" {
			t.Fatal("disabled user still has a running session")
		}
	}
	// Signing in again is refused too.
	c := &client{h: h}
	mustStatus(t, c.do("POST", "/api/login", map[string]string{"username": "alice", "password": "alice-password-1"}), http.StatusUnauthorized, "disabled login")
}

// 6a. Remote commands never splice user input into shell text unquoted.
func TestShellQuotingRoundTrips(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cases := []string{"", "plain", "it's", `"double"`, "$(touch /tmp/pwned)", "`id`", "a;b|c&d", "new\nline", "back\\slash", "*?[]~{}"}
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		b := make([]byte, rnd.Intn(24))
		for j := range b {
			b[j] = byte(1 + rnd.Intn(126))
		}
		cases = append(cases, string(b))
	}
	for _, s := range cases {
		out, err := exec.Command("sh", "-c", "printf %s "+sshx.QuoteShell(s)).Output()
		if err != nil || string(out) != s {
			t.Fatalf("quoting %q round-tripped to %q (err %v)", s, out, err)
		}
	}
}

// 6b. Only reviewed call sites start remote processes.
func TestRemoteExecCallSitesAreReviewed(t *testing.T) {
	allowed := map[string]bool{
		"term/manager.go: err = sess.Start(TmuxCommand(opt.TmuxSession))":                             true,
		"term/manager.go: err = sess.Start(TmuxNewCommand(opt.TmuxSession))":                          true,
		"term/manager.go: err = sess.Start(HerdrCommand(opt.HerdrSession))":                           true,
		"web/api_sessions.go: sess, err := s.terms.Start(ctx, res, u.ID, h.ID, h.Name, client, opts)": true,
		"herdr/client.go: out, err := sshx.Output(ctx, c, DiscoverCommand, sshx.MaxOutput)":           true,
		"herdr/keys.go: out, err := sshx.Output(ctx, c, KeysCommand, keysLimit)":                      true, // fixed text, no input
		"web/herdr.go: w.Run(ctx)": true, // watcher loop, runs no remote command itself
		"web/herdr.go: out, err := sshx.Output(lctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession, \"\"), convoLocateMax)":                                                                   true, // the same fixed script as conversation.go's, for when each agent last wrote to its log
		"web/ports.go: out, err := sshx.Output(ctx, c, listenCommand, sshx.MaxOutput)":                                                                                                                               true,
		`sshx/exec.go: ok, err := ch.SendRequest("exec", true, ssh.Marshal(&struct{ Command string }{cmd}))`:                                                                                                         true, // Output's and Exec's own exec; their callers are listed here
		"web/code.go: r, err := sshx.Exec(ctx, c, cmd, sshx.MaxOutput, opt)":                                                                                                                                         true, // codeRun: only codeserver commands, checked below
		"web/files.go: out, err := sshx.Output(ctx, sess.Client(), tmuxCwdCommand(sess.TmuxSession), 8192)":                                                                                                          true, // fixed script, validated and quoted session name
		"web/files.go: out, err := sshx.Output(ctx, sess.Client(), tmuxForegroundCommand(sess.TmuxSession), 4096)":                                                                                                   true, // fixed script, validated and quoted session name
		"web/files.go: _, err := sshx.Output(ctx, sess.Client(), removeTreeCommand(target), 64<<10)":                                                                                                                 true, // fixed script, validated and quoted absolute path
		"web/picker.go: out, err := sshx.Output(ctx, c, tmuxListCommand, 256<<10)":                                                                                                                                   true, // fixed text, no input
		"web/complete.go: out, err := sshx.Output(ctx, sess.Client(), completeCommand(cwd), completeOutMax)":                                                                                                         true, // fixed script, the folder quoted as its only argument
		"web/conversation.go: out, err := sshx.Output(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession, \"\"), convoLocateMax)":                                                             true, // fixed script, the mode and the validated tmux name quoted
		"web/conversation.go: out, err := sshx.Output(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession, tmuxTarget), convoLocateMax)":                                                       true, // fixed script, the mode and the validated tmux name and pane quoted
		"web/conversation.go: tails, err := sshx.Output(ctx, sess.Client(), convoTailsCommand(logs), maxScreenLogs*(32<<10))":                                                                                        true, // fixed script; its arguments the logs a daemon holds, as the locating script found them, quoted
		"web/conversation.go: out, err := sshx.Output(ctx, sess.Client(), convoScreenCommand(sess.Persistence, target, s.herdrSocks.get(sess.ID)), 256<<10)":                                                         true, // fixed script, the mode, the validated pane and the socket from discovery quoted
		"web/conversation.go: out, err := sshx.Output(ctx, sess.Client(), convoTitleCommand(log, sid), 65536)":                                                                                                       true, // fixed grep, the log path found by locateConvo quoted
		"web/convo_model.go: out, err := sshx.Output(ctx, sess.Client(), settingsReadCommand, maxSettings+16)":                                                                                                       true, // fixed: reads ~/.claude/settings.json
		"web/convo_model.go: _, err = sshx.Exec(rctx, sess.Client(), settingsWriteCommand(now, fixed), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(fixed), Release: running.Done})":                                true, // fixed: writes it back only over what was read and only whole; its arguments hex digests and a length
		"web/conversation.go: res, err := sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, b.Pane, socket, b.PID, b.Start, b.Session, len(text), press, modeLines(what)), 16<<10,": true, // fixed script; mode, validated session name and pane id, the socket from discovery and the validated Claude session id quoted, numbers, keys of a fixed set (convoKeys) quoted; the checked text (sendText) on its input
		"web/convo_model.go: res, err := sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,":                         true, // fixed script; mode, validated session name and pane id, the socket from discovery and the validated Claude session id quoted, numbers, keys of a fixed set (convoKeys) quoted; the checked text (sendText) on its input
		"web/convo_opencode.go: out, err := sshx.Output(ctx, sess.Client(), ocQueryCommand(p.log, \"find\", p.cwd, strconv.FormatInt(since, 10)), 4096)":                                                             true, // fixed script; the database found open by the user's OpenCode (convoLogOf), the folder it runs in (/proc, no newline) and a number, all quoted
		"web/convo_opencode.go: out, err := sshx.Output(ctx, sess.Client(), ocQueryCommand(st.log, mode, append([]string{st.sid}, args...)...), ocReadMax)":                                                          true, // fixed script; that database, a fixed mode, the session id (ocSessionID) and numbers, all quoted
		"web/convo_mode.go: res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 256<<10, sshx.ExecOptions{Release: ended.Done})":                                           true, // fixed script; the mode, the validated pane id and the socket from discovery, all quoted; nothing of the person's goes to the host
		"web/convo_mode.go: res, err := sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, press, convoModeLines), 16<<10,":                        true, // fixed script as convoPress's; validated pane id, the socket from discovery, the validated session id quoted, numbers, keys of a fixed set (convoKeys); nothing typed
		"web/convo_sent.go: res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 256<<10, sshx.ExecOptions{Release: release})":                                              true, // fixed script; the mode, the validated pane id and the socket from discovery, all quoted; nothing of the person's goes to the host
		"web/convo_effort.go: res, err := sshx.Exec(ctx, sess.Client(), convoSendCommand(sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,":                        true, // as convo_model.go: fixed script; mode, validated session name and pane id, the socket from discovery and the validated Claude session id quoted, numbers, keys of a fixed set (modelKeys) quoted; the typed "/effort" on its input
		"term/session.go: s.rec.Output(buf[:n])": true, // the session recorder, not a process
	}
	re := regexp.MustCompile(`\.(Start|Run|Output|CombinedOutput|Exec)\(|SendRequest\("exec"`)
	root, _ := filepath.Abs("..")
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.Contains(p, "testutil") {
			return nil
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "//") || !re.MatchString(line) {
				continue
			}
			if !allowed[rel+": "+line] {
				t.Errorf("unreviewed process start: %s: %s", rel, line)
			}
		}
		return nil
	})
	// codeRun runs whatever it is given, so it must only ever be given the
	// fixed scripts of the codeserver package.
	b, _ := os.ReadFile("code.go")
	calls := regexp.MustCompile(`s\.codeRun\(([^)]*)`).FindAllStringSubmatch(string(b), -1)
	if len(calls) == 0 {
		t.Fatal("no codeRun calls found")
	}
	for _, c := range calls {
		if args := strings.Split(c[1], ","); len(args) < 3 || !strings.HasPrefix(strings.TrimSpace(args[2]), "codeserver.") {
			t.Errorf("codeRun called with a command not from the codeserver package: %s", c[0])
		}
	}
}

// Cross-site requests and sockets are refused.
func TestCrossSiteRequestsAreRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	evil := &client{h: h, cookie: admin.cookie, extra: map[string]string{"Origin": "https://evil.example"}}
	mustStatus(t, evil.do("POST", "/api/hosts", map[string]any{"name": "x", "address": "10.0.0.1", "username": "u", "auth": "ask"}), http.StatusForbidden, "cross-site POST")
	mustStatus(t, evil.do("GET", "/api/sessions/abc/ws", nil), http.StatusForbidden, "cross-site socket")

	req, _ := http.NewRequest("POST", h.ts.URL+"/api/logout", nil)
	req.AddCookie(&http.Cookie{Name: h.srv.cookieName(), Value: admin.cookie})
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without Origin: %d, want 403", res.StatusCode)
	}
}
