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
		// Single sign-on: off to the provider (rate-limited, the next page only this site's),
		// and back from it (the flow taken once, bound to the browser's cookie).
		"GET /api/auth/oidc/start": true, "GET /api/auth/oidc/callback": true,
	}
	// Called by another server, not a browser: no site check, no login (the
	// provider's back-channel logout, which verifies its own token).
	external := map[string]bool{"POST /api/auth/oidc/backchannel": true}
	for _, r := range h.srv.Routes() {
		key := r.Method + " " + r.Pattern
		if r.Access == Public && !public[key] {
			t.Errorf("route %s is public but not in the reviewed public list", key)
		}
		if r.Access == External && !external[key] {
			t.Errorf("route %s is external but not in the reviewed external list", key)
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
		"term/manager.go: err = sess.Start(TmuxCommand(opt.TmuxSession))":                                 true,
		"term/manager.go: err = sess.Start(TmuxNewCommand(opt.TmuxSession))":                              true,
		"term/manager.go: err = sess.Start(HerdrCommand(opt.HerdrSession))":                               true,
		"term/manager.go: err = sess.Start(TmuxAttachCommand(opt.TmuxSession))":                           true, // fixed script, the validated session name quoted; attaches only (no new session, no shell)
		"term/manager.go: err = sess.Start(HerdrAttachCommand(opt.HerdrSession))":                         true, // fixed script, the validated session name quoted; no shell where herdr is missing
		"web/api_sessions.go: sess, err := s.terms.Start(ctx, res, u.ID, h.ID, h.Name, client, opts)":     true,
		"web/api_pane_ws.go: err = sshSess.Start(herdrTerminalAttachCommand(socket, hp.TerminalID))":      true, // fixed script, discovered herdr socket and validated terminal_id (herdrTermID) quoted; direct single-pane attach only
		"web/restore.go: sess, err := s.terms.Start(ctx, res, t.OwnerID, t.HostID, h.Name, client, opts)": true, // attach only; the recorded name checked as an open's
		"herdr/client.go: out, err := sshx.Output(ctx, c, DiscoverCommand, sshx.MaxOutput)":               true,
		"herdr/keys.go: out, err := sshx.Output(ctx, c, KeysCommand, keysLimit)":                          true, // fixed text, no input
		"web/herdr.go: w.Run(ctx)": true, // watcher loop, runs no remote command itself
		"web/herdr.go: out, err := sshx.Output(ctx, sess.Client(), cmdScanCommand(), cmdScanMax)":                                                                                                                                                                                       true, // fixed script, no arguments: the SSH account's Claude Code processes' children whose output goes to a task file
		"web/herdr.go: out, err := sshx.Output(ctx, sess.Client(), bgTailCommand(sids, tail), len(sids)*(tail+256))":                                                                                                                                                                    true, // fixed script; its arguments a size (a constant) and Claude Code session ids, validated (convoUUID) and quoted
		"web/codex_panes.go: out, err := sshx.Output(ctx, sess.Client(), codexPanesCommand(socket, known, target), codexPanesMax)":                                                                                                                                                      true, // fixed python3 script (codexPanesScript); its arguments, quoted: the herdr socket from discovery, the fixed convoEndpointSh (codex_ep, run per Codex as the scan runs it), "<pid>:<start>" numbers of the gateway's own earlier look, and a pane id of herdrPaneID's form (from the press, validated) whose title herdr pane get reads. Reads the SSH account's own Codex processes (/proc) and Codex's database read only (mode=ro); nothing of the person's
		"web/herdr.go: out, err := sshx.Output(lctx, sess.Client(), convoLocateTimesCommand(sess.Persistence, sess.TmuxSession)+\"; \"+hostNowCommand, convoLocateMax)":                                                                                                                 true, // the same fixed script as conversation.go's, for when each agent last wrote to its log; then the fixed hostNowCommand (the host's time), nothing of the person's
		"web/ports.go: out, err := sshx.Output(ctx, c, listenCommand, sshx.MaxOutput)":                                                                                                                                                                                                  true,
		`sshx/exec.go: ok, err := ch.SendRequest("exec", true, ssh.Marshal(&struct{ Command string }{cmd}))`:                                                                                                                                                                            true, // Output's and Exec's own exec; their callers are listed here
		"web/code.go: r, err := sshx.Exec(ctx, c, cmd, sshx.MaxOutput, opt)":                                                                                                                                                                                                            true, // codeRun: only codeserver commands, checked below
		"web/files.go: out, err := sshx.Output(ctx, sess.Client(), cmd, 8192)":                                                                                                                                                                                                          true, // paneCwd: tmuxCwdCommand or tmuxPaneCwdCommand, fixed scripts; the validated session name quoted, and a pane id of tmuxPaneID's form ("%3") quoted
		"web/files.go: out, err := sshx.Output(ctx, sess.Client(), tmuxForegroundCommand(sess.TmuxSession), 4096)":                                                                                                                                                                      true, // fixed script, validated and quoted session name
		"web/files.go: _, err := sshx.Output(ctx, sess.Client(), removeTreeCommand(target), 64<<10)":                                                                                                                                                                                    true, // fixed script, validated and quoted absolute path
		"web/picker.go: out, err := sshx.Output(ctx, c, tmuxListCommand, 256<<10)":                                                                                                                                                                                                      true, // fixed text, no input
		"web/complete.go: out, err := sshx.Output(ctx, sess.Client(), completeCommand(cwd), completeOutMax)":                                                                                                                                                                            true, // fixed script, the folder quoted as its only argument
		"web/git_info.go: out, err := sshx.Output(ctx, sess.Client(), gitCommand(pr, cwd, root, branch), gitOutMax)":                                                                                                                                                                    true, // fixed scripts (git state; gh pr view), read only; the pane's own folder, and for the PR a checked path and branch name, quoted as arguments
		"web/git_changes.go: out, err := sshx.Output(ctx, sess.Client(), gitChangesCommand(cwd, against, file, from, nonce), gitChangesOut)":                                                                                                                                            true, // fixed read-only git diff/ls-files script, output cut on the host; the pane's own folder, "work" or "base" (checked), a path and a renamed one's old path each checked by cleanRepoPath (relative, no "..", no control), quoted, after "--", literal pathspecs; a hex nonce of the gateway's
		"web/host_check.go: out, err := sshx.Exec(ectx, res.Client, installKeyCommand(nonce), 64<<10, sshx.ExecOptions{Stdin: strings.NewReader(line + \"\\n\")})":                                                                                                                      true, // install-key: the fixed installKeyScript (no quote nor backslash in it: the same under any login shell) run by sh on a connection signed in with the password typed once (never stored or logged), after the form was validated as save validates it and the host key decided as a connection decides it; its one argument a hex nonce of the gateway's, quoted; the key line, from the user's own credential re-marshalled by ssh.MarshalAuthorizedKey with its comment cut to [A-Za-z0-9@._:+=-], only on stdin; it makes ~/.ssh (700) and authorized_keys (600) where missing, appends only a key not there, refuses links and what is not a file; output cut at 64 KiB, read only for its nonce's line; rate-limited per user and audited (host.install_key) without the password
		"web/side_question.go: lr, err := sshx.Exec(lctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession, tmuxTarget), convoLocateMax, sshx.ExecOptions{AwaitSlot: 2 * time.Second})":                                                                             true, // the locating script, as convoPress runs it: fixed, the validated session name and pane quoted
		"web/side_question.go: res, err := sshx.Exec(ctx, sess.Client(), sideQuestionCommand(b.PID, b.Start, b.Session, b.Model, nonce), sideAnswerCap+256, sshx.ExecOptions{Stdin: strings.NewReader(q), AwaitSlot: 2 * time.Second})":                                                 true, // fixed script running claude -p --resume --fork-session --no-session-persistence with no tools, no MCP servers and no hooks, under timeout, output cut on the host; only after the pane's process was checked to be that Claude Code at that session; pid and start numbers (re-checked against /proc on the host), the session a UUID, the model sideModel's, the nonce hex, each quoted; the question (sideQuestionText: capped, no control characters but \n \t) only on stdin, never in a command line; the process's start time and its session file's sessionId re-checked on the host before and after cwd and the environment of sideEnvNames are read from /proc (each variable read as one record, its own line breaks made \\001: none can pass for another; one with a line break or \\001 is not taken); run under setsid in a process group of its own, the whole group killed at its time (90s) and after it ends, the watch a group of its own killed too; marked CORRAL_SIDE_COPY=1 so the locating scan skips it; one at a time per session, held where it was not seen to end until its host timeout has surely ended it
		"web/agents_new.go: out, err := sshx.Output(ctx, sess.Client(), cmd, 64<<10)":                                                                                                                                                                                                   true, // startAgent: herdr's own agent start; the socket, name, kind, pane and session id each quoted (QuoteShell), the name made of letters, digits and dashes, the kind of agentKinds, the pane of herdrPaneID's form, the id a UUID; --settings the fixed askhook.Inline JSON, quoted
		"web/askhook.go: out, err := sshx.Output(ctx, c, askHookRead, 4<<20)":                                                                                                                                                                                                           true, // a fixed script reading ~/.claude/settings.json (and the managed settings) of the host account, nothing of the request in it
		"web/askhook.go: res, err := sshx.Exec(ctx, c, cmd, 4096, sshx.ExecOptions{Stdin: bytes.NewReader(next), Release: func() { s.askHook.forget(key); release() }})":                                                                                                                true, // a fixed script (askHookWrite) writing ~/.claude/settings.json; its arguments the sha256 hex read by the gateway (or "none"), write|remove and the new file's sha256 hex, each quoted; the new file on stdin
		"web/conversation.go: e.out, e.err = sshx.Output(sctx, sess.Client(), cmd, convoLocateMax)":                                                                                                                                                                                     true, // convoScans.run: cmd is convoLocateCommand's fixed script, its arguments the mode and the validated tmux name, quoted (its only caller, locateConvo)
		"web/conversation.go: lr, err := sshx.Exec(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession, tmuxTarget), convoLocateMax, sshx.ExecOptions{AwaitSlot: pressSlotWait(what)})":                                                                           true, // fixed script, the mode and the validated tmux name and pane quoted
		"web/conversation.go: res, err := sshx.Exec(sctx, sess.Client(), convoColorScreenCommand(sess.Persistence, b.Pane, socket), 512<<10, sshx.ExecOptions{AwaitSlot: pressSlotWait(what)})":                                                                                         true, // a Codex's screen read before a message is typed (a draft in its input): fixed script, the mode, the validated pane and the discovered socket quoted
		"web/conversation.go: tails, err := sshx.Output(ctx, sess.Client(), convoMessageTailsCommand(logs), maxScreenLogs*(72<<10))":                                                                                                                                                    true, // fixed read-only tail/awk script; quoted paths of logs held by the user's daemon, at most 64; reads 4 MiB and returns at most 64 KiB per log; no tool arguments are executed
		"web/conversation.go: out, err := sshx.Output(ctx, sess.Client(), convoScreenCommand(sess.Persistence, target, s.herdrSocks.get(sess.ID)), 256<<10)":                                                                                                                            true, // fixed script, the mode, the validated pane and the socket from discovery quoted
		"web/conversation.go: out, err := sshx.Output(ctx, sess.Client(), convoTitleCommand(log, sid, upto), 65536)":                                                                                                                                                                    true, // fixed grep, the log path found by locateConvo quoted
		"web/convo_model.go: out, err := sshx.Output(ctx, sess.Client(), codexConfigReadCommand, maxSettings+16)":                                                                                                                                                                       true, // fixed: reads Codex's config.toml (to say if it changed), nothing of the person's
		"web/convo_model.go: out, err := sshx.Output(ctx, sess.Client(), settingsReadCommand, maxSettings+16)":                                                                                                                                                                          true, // fixed: reads ~/.claude/settings.json
		"web/convo_model.go: _, err = sshx.Exec(rctx, sess.Client(), settingsWriteCommand(now, fixed), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(fixed), Release: running.Done})":                                                                                                   true, // fixed: writes it back only over what was read and only whole; its arguments hex digests and a length
		"web/convo_line_settle.go: _, err := sshx.Exec(ctx, sess.Client(), settingsWriteCommand(was, now), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(now), Release: running.Done})":                                                                                                 true, // as convo_model.go's: writes the settings back only over what was read and only whole; its arguments hex digests and a length
		"web/convo_line_settle.go: res, err := sshx.Exec(ctx, sess.Client(), cmd, limit, sshx.ExecOptions{Stdin: stdin, Release: running.Done})":                                                                                                                                        true, // lineExec, the one-line switch's commands, each a fixed script: convoScreenCommand and convoColorScreenCommand (mode, validated pane id, the socket from discovery quoted — or those of a line kept in the store, written there from the same; only read), lineAliveCommand (two numbers), convoGatedCommand with gate "free" (the line "/model " and one of four fixed words on its input) or "panel:" and a hex digest the gateway made (one key of modelKeys); validated session name, pane id and Claude session id quoted, numbers
		"web/convo_line_settle.go: out, err := sshx.Output(ctx, sess.Client(), settingsIdentCommand, 8192)":                                                                                                                                                                             true, // fixed, no arguments: prints the machine's id and name, the user's number and the settings file's path
		"web/conversation.go: res, err := sshx.Exec(ctx, sess.Client(), cmd, sendOut,":                                                                                                                                                                                                  true, // fixed script; a gate of a fixed set (Codex's "c"-prefixed too) or "reply:" and a checked hex digest (replyOK); mode, validated session name and pane id, the socket from discovery and the validated Claude session id quoted, numbers, keys of a fixed set (convoKeys) quoted; the checked text (sendText) on its input (a Codex message with pictures' paths in it: convoGatedPasteCommand, the same script, the message pasted in parts, their lengths checked numbers; a reply's text only pasted: convoGatedFillCommand, its length with \"-\", fillOK)
		"web/convo_model.go: res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,":                                                                                     true, // fixed script; mode, validated session name and pane id, the socket from discovery and the validated Claude session id quoted, numbers, keys of a fixed set (convoKeys) quoted; the checked text (sendText) on its input
		"web/convo_opencode.go: out, err := sshx.Output(ctx, sess.Client(), ocQueryCommand(p.log, \"find\", p.cwd, strconv.FormatInt(since, 10)), 4096)":                                                                                                                                true, // fixed script; the database found open by the user's OpenCode (convoLogOf), the folder it runs in (/proc, no newline) and a number, all quoted
		"web/convo_opencode.go: out, err := sshx.Output(ctx, sess.Client(), ocQueryCommand(st.log, mode, append([]string{st.sid}, args...)...), ocReadMax)":                                                                                                                             true, // fixed script; that database, a fixed mode, the session id (ocSessionID) and numbers, all quoted
		"web/convo_mode.go: res, err := sshx.Exec(ctx, sess.Client(), scrCmd, 256<<10, sshx.ExecOptions{Release: ended.Done})":                                                                                                                                                           true, // fixed script (convoScreenCommand or agyScreenCommand); mode, validated pane id and discovery socket quoted; nothing of the person's goes to the host
		"web/convo_reply.go: res, err := sshx.Exec(ctx, sess.Client(), convoScreenJoinedCommand(sess.Persistence, pane, socket), 512<<10+unwrapMax+64, sshx.ExecOptions{Release: release})":                                                                                             true, // fixed script (the screen as shown, framed by its length, then its lines as the terminal joins them, at most unwrapMax); the mode, the validated pane and the socket from discovery quoted
		"web/conversation.go: res, err := sshx.Exec(sctx, sess.Client(), convoGatedCommand(\"free\", sess.Persistence, sess.TmuxSession, b.Pane, socket, b.PID, b.Start, b.Session, 0, []string{}, 0), sendOut, sshx.ExecOptions{Stdin: strings.NewReader(\"\"), Release: ended.Done})": true, // a send's bringing the conversation's box back first (a subagent's shown), its words then read: fixed script as a send's, gate "free", no keys; validated pane id, the socket from discovery, the validated session id quoted, numbers; nothing typed
		"web/conversation.go: res, err := sshx.Exec(sctx, sess.Client(), convoColorScreenCommand(sess.Persistence, b.Pane, socket), 512<<10, sshx.ExecOptions{})":                                                                                                                       true, // a send's look at the box first (words in it already): fixed script as convoScreenCommand's, its colours kept; validated pane id and the socket from discovery quoted; nothing typed
		"web/conversation.go: if r2, err := sshx.Exec(sctx, sess.Client(), pcmd, 512<<10, sshx.ExecOptions{}); err == nil {":                                                                                                                                                             true, // fixed script (convoColorScreenCommand or agyColorScreenCommand): the screen a panel's key left, in the same answer
		"web/convo_quota.go: res, err := sshx.Exec(ctx, sess.Client(), quotaReadCommand(), quotaReadMax, sshx.ExecOptions{})":                                                                                                                                                           true, // fixed script, no arguments: prints only the account's id and the quota it keeps
		"web/convo_quota.go: _, _ = sshx.Exec(ctx, sess.Client(), quotaRefreshCommand(), 4<<10, sshx.ExecOptions{})":                                                                                                                                                                    true, // fixed script, no arguments: Claude Code answers its own /usage; what it prints is not read
		"web/convo_panel.go: res, err := sshx.Exec(ctx, sess.Client(), cmd, 512<<10, sshx.ExecOptions{Release: release})":                                                                                                                                                                true, // fixed script (convoColorScreenCommand or agyColorScreenCommand); mode, validated pane id and discovery socket quoted; only reads
		"web/convo_subscan.go: res, err := sshx.Exec(xctx, sess.Client(), subScanCommand(strings.TrimSuffix(root, \".jsonl\"), detail), subScanOutMax, sshx.ExecOptions{})":                                                                                                             true, // fixed script; the session's folder, from the log the gateway found, quoted; only reads
		"web/convo_suggest.go: res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: release})":                                                                                                         true, // fixed script as convoScreenCommand's, its colours kept; validated pane id and the socket from discovery quoted; nothing typed
		"web/convo_suggest.go: res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: running.Done})":                                                                                                    true, // greyedSuggestion, before a model/effort switch: fixed script as the suggestion's; validated pane id and the socket from discovery quoted; nothing typed
		"web/convo_sent.go: res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(freeGate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, enter, sendLines), sendOut,":                                                                                    true, // fixed script as a send's; its gate "free", or Codex's "cfree"; validated pane id, the socket from discovery, the validated session id quoted, numbers, Enter of a fixed set; nothing typed
		"web/convo_mode.go: res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, press, convoModeLines), 16<<10,":                                                                                    true, // fixed script as convoPress's; validated pane id, the socket from discovery, the validated session id quoted, numbers, keys of a fixed set (convoKeys); nothing typed
		"web/convo_codex_note.go: res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{})":                                                                                                                           true, // the pane's screen, read (fixed script; pane and socket as every screen read), to tell Codex's "Implement this plan?" before a reply
		"web/agents_codex_rewind.go: _, err := rewindSend(ctx, p.sess, p.check()+\"tmux send-keys -t \"+sshx.QuoteShell(p.pane)+\" \"+sshx.QuoteShell(k), 1<<10)":                                                                                                                       true, // a fixed key to that pane (its id checked to match), only while the pane's process is the one found
		"web/agents_codex_rewind.go: out, err := rewindExec(ctx, p.sess, p.check()+\"tmux -u capture-pane -p -t \"+sshx.QuoteShell(p.pane), 256<<10)":                                                                                                                                   true, // that pane's screen, likewise
		"web/agents_codex_rewind.go: _, err := rewindSend(ctx, p.sess, p.check()+\"tmux send-keys -l -t \"+sshx.QuoteShell(p.pane)+\" -- \"+sshx.QuoteShell(text), 1<<10)":                                                                                                              true, // "/rename <name>" typed (literal, quoted, after --) into the copy's own pane, only while its process is the one found
		"web/agents_codex_rewind.go: out, err := rewindExec(ctx, p.sess, p.check()+\"tmux -u display-message -p -t \"+sshx.QuoteShell(p.pane)+\" '#{pane_title}'\", 4<<10)":                                                                                                             true, // that pane's title, likewise
		"web/agents_codex_rewind.go: out, err := rewindSend(ctx, p.sess, \"sh -c \"+sshx.QuoteShell(p.sendSh(how, arg)), 64)":                                                                                                                                                           true, // a fixed key, or "/rename <name>", to the copy's pane by herdr's own command, in one command with the check that the copy's Codex is still that process (pid and start numbers, the pane and socket quoted), in front, in that pane
		"web/agents_codex_rewind.go: out, err := sshx.Output(ctx, sess.Client(), \"sh -c \"+sshx.QuoteShell(cmd), 128)":                                                                                                                                                                 true, // whether a process (pid and start numbers) was started "codex fork <id>": its command line read
		"web/agents_codex_rewind.go: out, err = rewindExec(ctx, p.sess, \"sh -c \"+sshx.QuoteShell(cmd), 256)":                                                                                                                                                                          true, // the copy's own tmux pane's Codex (its process, or that one's one child named codex): its pid, start and fork argument read (the pane's pid quoted)
		"web/agents_codex_rewind.go: res, err := sshx.Exec(ctx, sess.Client(), cmd, limit, sshx.ExecOptions{AwaitSlot: 5 * time.Second})":                                                                                                                                               true, // rewindExec: the reviewed commands above (each listed by its own call), run waiting for a side operation's slot
		"web/agents_codex_rewind.go: return sshx.Output(ctx, sess.Client(), cmd, limit)":                                                                                                                                                                                                true, // rewindSend: the reviewed key and text commands above (each listed by its own call), not waiting for a slot
		"web/agents_codex_sessions.go: out, err := sshx.Output(ctx, sess.Client(), codexSessionsCommand(dir), 256<<10)":                                                                                                                                                                 true, // a folder's Codex conversations from its own database, read only (fixed script; the folder quoted, a query parameter there)
		"web/agents_agy_sessions.go: out, err := sshx.Output(ctx, sess.Client(), agySessionsCommand(dir), 256<<10)":                                                                                                                                                                     true, // a folder's Antigravity CLI conversations from its own database, read only (fixed script; the folder quoted, a query parameter there)
		"web/agents_new.go: out, err := sshx.Output(ctx, sess.Client(), agentTmuxCmd(name, dir, kind, home, how), 8<<10)":                                                                                                                                                               true, // a new agent in a tmux session of its own: its name made here (agentName), the folder checked, every value quoted
		"web/convo_mode.go: if out, err := sshx.Output(ctx, sess.Client(), scrCmd, 256<<10); err == nil {":                                                                                                                                                                               true, // the pane's screen read again (convoScreenCommand or agyScreenCommand) after a Codex/Agy mode key, for its footer drawn late
		"web/convo_went.go: res, err := sshx.Exec(ctx, sess.Client(), wentCommand(strings.TrimSuffix(root, \".jsonl\")+\"/subagents\", minutes, frag), wentOut+(64<<10), sshx.ExecOptions{})":                                                                                           true, // fixed script; the subagents' folder of the located log, a number and a fragment of the message (the person's own words, sent to that agent already) quoted; only reads, through no link
		"web/convo_sent.go: res, err := sshx.Exec(ctx, sess.Client(), scrCmd, 256<<10, sshx.ExecOptions{Release: release})":                                                                                                                                                              true, // fixed script (convoScreenCommand or agyScreenCommand); mode, validated pane id and discovery socket quoted; nothing of the person's goes to the host
		"web/convo_effort.go: res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, len(text), press, read), 64<<10,":                                                                                    true, // as convo_model.go: fixed script; mode, validated session name and pane id, the socket from discovery and the validated Claude session id quoted, numbers, keys of a fixed set (modelKeys) quoted; the typed "/effort" on its input
		"web/convo_sent.go: res, err := sshx.Exec(ctx, sess.Client(), scrCmd, 256<<10, sshx.ExecOptions{Release: held.Done})":                                                                                                                                                            true, // sentEnter's look (convoScreenCommand or agyScreenCommand): as enterAgain's
		"web/convo_sent.go: _, err = sshx.Exec(ctx, sess.Client(), convoGatedCommand(freeGate, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, enter, sendLines), sendOut,":                                                                                       true, // sentEnter's Enter: as enterAgain's; validated pane id, the socket from discovery, the validated session id quoted, numbers, Enter of a fixed set; nothing typed
		"web/convo_card.go: res, err := sshx.Exec(ctx, sess.Client(), convoColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: held.Done})":                                                                                                          true, // cardPress's look: fixed script as the suggestion's; the mode, the validated pane id and the socket from discovery quoted; nothing typed
		"web/convo_card.go: _, err = sshx.Exec(ctx, sess.Client(), convoGatedCommand(\"card:\"+c.digest, sess.Persistence, sess.TmuxSession, pane, socket, pid, start, sid, 0, replyPress(sess.Persistence, []string{key}), 0), sendOut,":                                               true, // cardPress's key: the gated script, its card gate a hex digest the gateway made; the validated pane, socket and session id quoted; one digit (checked in convoPress)
		"web/conversation.go: res, err := sshx.Exec(ctx, sess.Client(), freshStatusCommand(b.PID), 64<<10, sshx.ExecOptions{})":                                                                                                                                                         true, // fixed head of Claude Code's session file in $HOME; the bound pid (a number found by locateConvo); nothing of the person's
		"web/notify_said.go: out, err := sshx.Output(ctx, sess.Client(), codexTailCommand(log, saidCodexTail), saidCodexTail+1024)":                                                                                                                                                     true, // fixed tail -c of a constant size; the Codex log the conversation located or the host's look found (an absolute rollout path, codexLogName), quoted; nothing of the person's
		"web/seen_recheck.go: out, err := sshx.Output(cctx, sess.Client(), logsWroteCommand(files)+\"; \"+hostNowCommand, 64*len(files)+256)":                                                                                                                                           true, // when logs known for sure were written: Codex rollouts (absolute paths, codexLogName), each quoted; Claude Code sessions by id (claudeSessionID, a UUID, checked) under the account's config; the host's clock; nothing of the person's
		"web/seen_recheck.go: out, err := sshx.Output(ctx, sess.Client(), logsWroteCommand([]string{ref})+\"; \"+hostNowCommand, 512)":                                                                                                                                                  true, // the same, of one pane's log known for sure (paneLogRef), before a "finished" alert
		"web/convo_image.go: out, err := sshx.Output(ctx, sess.Client(), imageRowCommand(log, uuid), imageLineMax)":                                                                                                                                                                     true, // fixed grep -m 1 -F; its pattern the validated row uuid (convo.ImageAt) in a fixed "uuid":"…" and the conversation's own located log, both quoted; nothing of the person's
		"web/convo_tmux_cmds.go: out, err := sshx.Output(ctx, sess.Client(), cmdScanCommand(), cmdScanMax)":                                                                                                                                                                             true, // the same fixed script as herdr.go's cmd prober (cmdScanCommand), no arguments; nothing of the person's
		"web/convo_codex_title.go: res, err := sshx.Exec(ctx, sess.Client(), codexTitleCommand(b.Pane), 4<<10, sshx.ExecOptions{})":                                                                                                                                                     true, // fixed tmux display-message of a pane's title; the bind's pane id (found by locateConvo) quoted; nothing of the person's
		"web/conversation.go: res, err := sshx.Exec(sctx, sess.Client(), convoScreenCommand(sess.Persistence, b.Pane, socket), 512<<10, sshx.ExecOptions{})":                                                                                                                            true, // a Codex command's look first: fixed script; the mode, the validated pane id and the socket from discovery quoted; nothing typed
		"web/conversation.go: res, err := sshx.Exec(sctx, sess.Client(), agyColorScreenCommand(sess.Persistence, b.Pane, socket), 512<<10, sshx.ExecOptions{})":                                                                                                                         true, // an agy send/command's look first: fixed script reading live bottom with ANSI (--source recent --lines 60 --format ansi, fallback visible); mode, validated pane id and discovery socket quoted; nothing typed
		"web/convo_model.go: res, err := sshx.Exec(ctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: running.Done})":                                                                                                           true, // Codex's model picker, each step looked at before its press (review493): fixed script; the mode, the validated pane id and the socket from discovery quoted; nothing typed
		"web/convo_model.go: res, err := sshx.Exec(lctx, sess.Client(), convoScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Release: running.Done})":                                                                                                          true, // the same look, leaving its picker after a failure (review498: each Escape of a screen read): fixed script; mode, validated pane id and the socket from discovery quoted; nothing typed
		"web/agents_fork.go: out, err := sshx.Output(ctx, sess.Client(), forkFindCommand(kind, id), 8<<10)":                                                                                                                                                                             true, // fixed find in the account's own folders; the session id a UUID (sessionUUID), quoted
		"web/agents_fork.go: out, err := sshx.Output(ctx, sess.Client(), forkCutCommand(src, at), 256)":                                                                                                                                                                                 true, // fixed grep/head; the log found by forkFindCommand and a UUID, quoted
		"web/agents_fork.go: out, err = sshx.Output(ctx, sess.Client(), forkCopyCommand(src, dst, off, from, id), 64)":                                                                                                                                                                  true, // fixed head|sed; that log, its folder and a UUID the gateway made, quoted; a number; the sed expression of two UUIDs
		"web/agents_fork.go: _, _ = sshx.Output(ctx, sess.Client(), forkDropCommand(src[:strings.LastIndexByte(src, '/')+1]+id+\".jsonl\"), 64)":                                                                                                                                        true, // rm of the copy forkCopy made: that log's folder and the UUID the gateway made, quoted
		"web/convo_roomy.go: res, err := sshx.Exec(lctx, sess.Client(), paneLayoutCommand(pane, socket), 64<<10, sshx.ExecOptions{})":                                                                                                                                                   true, // herdr's layout of the pane, read before its picker (2026-11-10): fixed script; the validated pane id and the socket from discovery quoted; nothing typed, nothing pressed
		"web/convo_agy_model.go: out, err := sshx.Output(mctx, sess.Client(), agyModelsCommand, 128<<10)":                                                                                                                                                                               true, // fixed read-only command listing available agy models (agy models 2>/dev/null), nothing of the request in it
		"web/convo_agy_model.go: out, err := sshx.Output(ctx, sess.Client(), agySettingsReadCommand, maxSettings+16)":                                                                                                                                                                   true, // fixed read-only script reading ~/.gemini/antigravity-cli/settings.json
		"web/convo_agy_model.go: scr, serr := sshx.Output(sctx, sess.Client(), agyScreenCommand(sess.Persistence, pane, socket), 512<<10)":                                                                                                                                              true, // fixed script reading live agy status bar; mode, validated pane id and discovered socket quoted; nothing typed
		"web/convo_agy_model.go: rawAnsi, err := sshx.Output(sctx, sess.Client(), agyColorScreenCommand(sess.Persistence, pane, socket), 512<<10)":                                                                                                                                      true, // fixed script reading live agy screen with ANSI before /model or /effort; mode, validated pane id and discovered socket quoted
		"web/convo_agy_model.go: _, werr := sshx.Exec(ctx2, sess.Client(), agySettingsWriteCommand(now, fixed), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(fixed)})":                                                                                                                true, // fixed CAS restore of ~/.gemini/antigravity-cli/settings.json top-level model key; arguments are hex sha256 digests and byte length
		"web/convo_agy_model.go: _, err = sshx.Exec(ctx, sess.Client(), cmd, sendOut, sshx.ExecOptions{Stdin: strings.NewReader(slashCmd), AwaitSlot: pressSlotWait(\"model\")})":                                                                                                      true, // convoGatedCommand with afree:<digest> gate sending validated /model <id> or /effort <level> on stdin
		"web/convo_agy_model.go: scr, serr := sshx.Output(pctx, sess.Client(), agyScreenCommand(sess.Persistence, pane, socket), 512<<10)":                                                                                                                                              true, // fixed script polling live agy status bar after /model or /effort; mode, validated pane id and discovered socket quoted
		"web/convo_quota.go: res, err := sshx.Exec(ctx, sess.Client(), agyQuotaCommand(), quotaReadMax, sshx.ExecOptions{})":                                                                                                                                                            true, // fixed read-only command running agy -p /usage --output-format json; nothing of the request in it
		"web/convo_agy_input.go: res, err = sshx.Exec(rctx, sess.Client(), agyScreenCommand(sess.Persistence, b.Pane, \"\"), 256<<10, sshx.ExecOptions{})":                                                                                                                             true, // fixed read-only tmux capture-pane for agy waiting detection; validated pane id quoted; nothing typed
		"web/convo_agy_db.go: out, err := sshx.Output(qctx, sess.Client(), agyDBErrorsCommand(dbPath, minIdx), agyDBErrorsMax)":                                                                                                                                                       true, // fixed read-only python3 sqlite3 (?mode=ro) query reading step_type=17 ERROR_MESSAGE rows from agy's conversations/<sid>.db; path derived from st.log + validated UUID, minIdx integer
		"web/herdr_agytitle.go: out, err := sshx.Output(ctx, sess.Client(), agyTitlesCommand(socket), agyTitleScanMax)":                                                                                                                                                              true, // fixed read-only python3 scan of the user's agy panes on this herdr socket, reading conversation_summaries.db (?mode=ro) and recent USER_INPUT lines; socket from discovery quoted
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
