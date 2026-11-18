package web

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/testutil"
)

// checkCase is the form a check is asked with.
func checkForm(port int, extra map[string]any) map[string]any {
	m := map[string]any{"address": "127.0.0.1", "port": port, "username": "alice", "auth": "password", "password": "alice-pass"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestHostTestOutcomes(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	a := h.member(admin, "alice")
	srv := h.sshServer(t.TempDir())
	test := func(form map[string]any) map[string]any {
		t.Helper()
		r := a.do("POST", "/api/hosts/test", form)
		mustStatus(t, r, 200, "test")
		return r.json()
	}
	want := func(got map[string]any, ok bool, stage, reason, field string) {
		t.Helper()
		if got["ok"] != ok || (stage != "" && got["stage"] != stage) || (reason != "" && got["reason"] != reason) || (field != "" && got["field"] != field) {
			t.Errorf("want ok=%v %s/%s on %s, got %v", ok, stage, reason, field, got)
		}
	}
	// A good password: in, its key's fingerprint said, nothing pinned (no host to pin it to).
	got := test(checkForm(srv.Addr.Port, nil))
	want(got, true, "", "", "")
	if got["login_tested"] != true || got["fingerprint"] != ssh.FingerprintSHA256(srv.HostKey) {
		t.Errorf("good: %v", got)
	}
	// A bad one: refused, on the password.
	want(test(checkForm(srv.Addr.Port, map[string]any{"password": "nope"})), false, "auth", "refused", "password")
	// A forbidden address (outside 127.0.0.1/32 the harness allows): said, not dialled.
	want(test(checkForm(22, map[string]any{"address": "10.255.255.1"})), false, "address", "forbidden", "address")
	// A closed port.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	want(test(checkForm(closed, nil)), false, "connect", "refused", "port")
	// Not an SSH server there.
	ln, _ = net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			c.Close()
		}
	}()
	want(test(checkForm(ln.Addr().(*net.TCPAddr).Port, nil)), false, "ssh", "handshake", "port")
	// Asked each time: up to signing in only, no password sent.
	before := srv.Attempts()
	got = test(checkForm(srv.Addr.Port, map[string]any{"auth": "ask", "password": ""}))
	want(got, true, "", "", "")
	if got["login_tested"] != false || srv.Attempts() != before {
		t.Errorf("ask: %v, %d password checks", got, srv.Attempts()-before)
	}
	// A key, which the host does not take (no keys set up): refused on the key.
	r := a.do("POST", "/api/credentials", map[string]any{"name": "laptop", "mode": "generate"})
	mustStatus(t, r, 201, "key")
	keyID := int64(r.json()["id"].(float64))
	want(test(checkForm(srv.Addr.Port, map[string]any{"auth": "key", "credential_id": keyID, "password": ""})), false, "auth", "", "credential_id")

	// Editing: the stored password used where none is typed; the pinned key compared.
	r = a.do("POST", "/api/hosts", checkForm(srv.Addr.Port, map[string]any{"name": "box"}))
	mustStatus(t, r, 201, "create")
	hostID := r.json()["id"]
	r = a.do("POST", "/api/sessions", map[string]any{"host_id": hostID})
	mustStatus(t, r, 201, "open (pins the key)")
	edit := checkForm(srv.Addr.Port, map[string]any{"id": hostID, "password": ""})
	want(test(edit), true, "", "", "")
	srv.RotateHostKey()
	got = test(edit)
	want(got, false, "hostkey", "changed", "")
	if got["fingerprint"] != ssh.FingerprintSHA256(srv.HostKey) || got["expected"] == "" {
		t.Errorf("changed key: %v", got)
	}
	// (Not pinned by a test: the key on record is still the old one.)
	r = a.do("GET", "/api/hosts/"+strconv.Itoa(int(hostID.(float64)))+"/hostkey", nil)
	if strings.Contains(string(r.Body), ssh.FingerprintSHA256(srv.HostKey)) {
		t.Errorf("the test pinned the new key: %s", r.Body)
	}
	// Another address: a new machine, its key shown, not compared (the password typed: the stored one stays home).
	srv2 := h.sshServer(t.TempDir())
	got = test(checkForm(srv2.Addr.Port, map[string]any{"id": hostID}))
	want(got, true, "", "", "")
	if got["fingerprint"] != ssh.FingerprintSHA256(srv2.HostKey) {
		t.Errorf("moved: %v", got)
	}
	// Another user's host: not found.
	b := h.member(admin, "bob")
	if r := b.do("POST", "/api/hosts/test", edit); r.Status != 404 {
		t.Errorf("another's host: %d %s", r.Status, r.Body)
	}
	// Audited, without the password.
	rows, _ := h.store.ListAudit(t.Context(), store.AuditQuery{Limit: 500})
	n := 0
	for _, e := range rows {
		if e.Action == "host.test" {
			n++
			if strings.Contains(string(e.Detail), "alice-pass") || strings.Contains(string(e.Detail), "nope") {
				t.Errorf("a password in the audit: %s", e.Detail)
			}
		}
	}
	if n == 0 {
		t.Errorf("not audited")
	}
}

func TestHostTestPasswordOff(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	srv := h.sshServer(t.TempDir())
	srv.PasswordOff()
	r := a.do("POST", "/api/hosts/test", checkForm(srv.Addr.Port, nil))
	mustStatus(t, r, 200, "test")
	if j := r.json(); j["stage"] != "auth" || j["reason"] != "not_offered" {
		t.Errorf("password off: %v", j)
	}
}

func TestHostTestRateLimited(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		r := a.do("POST", "/api/hosts/test", checkForm(1, map[string]any{"address": "10.255.255.1"}))
		limited = r.Status == 429
	}
	if !limited {
		t.Errorf("never limited")
	}
}

// The key put on the host with a password: installKeyScript run by a real sh
// in a home of the test's own; then signed in with the key.
func TestHostInstallKey(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	home := t.TempDir()
	srv := h.sshServer(home)
	srv.AcceptKeysFrom(filepath.Join(home, ".ssh", "authorized_keys"))
	srv.HandleExec(testutil.ShellExec(home, func(cmd string) bool { return strings.HasPrefix(cmd, "sh -c ") }))
	r := a.do("POST", "/api/credentials", map[string]any{"name": "laptop", "mode": "generate"})
	mustStatus(t, r, 201, "key")
	keyID := r.json()["id"]
	pub := r.json()["public_key"].(string)
	form := checkForm(srv.Addr.Port, map[string]any{"auth": "key", "credential_id": keyID, "password": "", "install_password": "alice-pass"})
	install := func(f map[string]any) map[string]any {
		t.Helper()
		r := a.do("POST", "/api/hosts/install-key", f)
		mustStatus(t, r, 200, "install")
		return r.json()
	}
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	// Something there already: kept as it was.
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(ak, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherKeyOtherKeyOtherKeyOtherKeyOtherKeyOth other@x"), 0o600) // (no newline at its end)
	got := install(form)
	if got["ok"] != true || got["installed"] != "added" || got["login_tested"] != true {
		t.Fatalf("install: %v", got)
	}
	b, _ := os.ReadFile(ak)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOther") || !strings.HasPrefix(lines[1], strings.Join(strings.Fields(pub)[:2], " ")) {
		t.Errorf("authorized_keys: %q", b)
	}
	// Again: already there, nothing added.
	got = install(form)
	if got["ok"] != true || got["installed"] != "present" {
		t.Errorf("again: %v", got)
	}
	if b2, _ := os.ReadFile(ak); string(b2) != string(b) {
		t.Errorf("changed on a second install: %q", b2)
	}
	// A wrong password: said on the install's password, nothing run.
	f := checkForm(srv.Addr.Port, map[string]any{"auth": "key", "credential_id": keyID, "password": "", "install_password": "nope"})
	if got = install(f); got["stage"] != "auth" || got["field"] != "install_password" {
		t.Errorf("wrong password: %v", got)
	}
	// authorized_keys a link: said, not replaced.
	real := filepath.Join(home, "real_keys")
	os.WriteFile(real, []byte(""), 0o600)
	os.Remove(ak)
	os.Symlink(real, ak)
	if got = install(form); got["stage"] != "install" || got["reason"] != "link" {
		t.Errorf("link: %v", got)
	}
	if fi, _ := os.Lstat(ak); fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced")
	}
	if b, _ := os.ReadFile(real); len(b) != 0 {
		t.Errorf("written through the link: %q", b)
	}
	// None there: made, 700 and 600.
	os.RemoveAll(filepath.Join(home, ".ssh"))
	if got = install(form); got["ok"] != true || got["installed"] != "added" {
		t.Errorf("fresh: %v", got)
	}
	if fi, err := os.Stat(filepath.Join(home, ".ssh")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf(".ssh: %v %v", fi, err)
	}
	if fi, err := os.Stat(ak); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys: %v %v", fi, err)
	}
	// The key on the command line: never (it goes on the script's input).
	for _, c := range srv.Commands() {
		if strings.Contains(c, strings.Fields(pub)[1]) {
			t.Errorf("the key in the command: %s", c)
		}
	}
	// Audited, without the password.
	rows, _ := h.store.ListAudit(t.Context(), store.AuditQuery{Limit: 500})
	n := 0
	for _, e := range rows {
		if e.Action == "host.install_key" {
			n++
			if strings.Contains(string(e.Detail), "alice-pass") {
				t.Errorf("the password in the audit: %s", e.Detail)
			}
		}
	}
	if n == 0 {
		t.Errorf("not audited")
	}
}

// installKeyScript: no single quote nor backslash in it (the same however a
// login shell — fish too — reads its quoted word); and it runs by itself.
func TestInstallKeyScriptPlain(t *testing.T) {
	if strings.ContainsAny(installKeyScript, `'\`) || strings.ContainsAny(authorizedKeysScan, `'\`) {
		t.Fatalf("a quote or backslash in the script")
	}
	home := t.TempDir()
	run := func(in string) string {
		c := exec.Command("sh", "-c", installKeyScript, "corral", "abc", authorizedKeysScan)
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		c.Stdin = strings.NewReader(in)
		out, _ := c.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	// (Its words on a line of their own, whatever came before them.)
	if got := installSaid("banner without a newline\nabc OK added\n", "abc"); got != "OK added" {
		t.Errorf("installSaid: %q", got)
	}
	if got := installSaid("bannerabc OK added", "abc"); got != "" {
		t.Errorf("installSaid run into: %q", got)
	}
	if got := run(""); got != "abc E nokey" {
		t.Errorf("no key: %q", got)
	}
	// (Readable, or said: not taken for "not there", nor the key run onto a last line with no newline.)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte("ssh-ed25519 OLD x"), 0o200)
	if os.Geteuid() != 0 {
		if got := run("ssh-ed25519 NEW x\n"); got != "abc E notreadable" {
			t.Errorf("unreadable: %q", got)
		}
		os.Chmod(filepath.Join(home, ".ssh", "authorized_keys"), 0o600)
		if b, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys")); string(b) != "ssh-ed25519 OLD x" {
			t.Errorf("unreadable, changed: %q", b)
		}
	}
	os.RemoveAll(filepath.Join(home, ".ssh"))
	// (A key only in a comment, or in another's comment: not there.)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte("# ssh-ed25519 AAAA\nssh-rsa ZZZZ note ssh-ed25519 AAAA\n"), 0o600)
	if got := run("ssh-ed25519 AAAA one\n"); got != "abc OK added" {
		t.Errorf("first: %q", got)
	}
	// The same key with another comment, or options before it: present.
	if got := run("ssh-ed25519 AAAA two\n"); got != "abc OK present" {
		t.Errorf("other comment: %q", got)
	}
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(`from="10.0.0.0/8",command="echo hi there" ssh-ed25519 CCCC c`+"\n"), 0o600)
	if got := run("ssh-ed25519 CCCC x\n"); got != "abc OK present" {
		t.Errorf("after options: %q", got)
	}
	// (Options quoting spaces, tabs and key-like words; an escaped quote in them; CRLF; no comment.)
	for _, c := range []struct{ file, key, want string }{
		{`from="10.0.0.0/8",command="echo ssh-backup ready" ssh-ed25519 DDDD` + "\n", "ssh-ed25519 DDDD x", "OK present"},
		{`command="echo ssh-ed25519 EEEE ignored" ssh-ed25519 OTHER` + "\n", "ssh-ed25519 EEEE x", "OK added"},
		{"command=\"echo\tssh-x\" ssh-ed25519 FFFF\n", "ssh-ed25519 FFFF x", "OK present"},
		{`command="say \"hi there\"" ssh-ed25519 GGGG c` + "\n", "ssh-ed25519 GGGG x", "OK present"},
		{`from="10.0.0.0/8" ssh-ed25519 HHHH` + "\r\n", "ssh-ed25519 HHHH x", "OK present"},
		{"ssh-ed25519 IIII\r\n", "ssh-ed25519 IIII x", "OK present"},
		{"   \t# ssh-ed25519 JJJJ\n\n", "ssh-ed25519 JJJJ x", "OK added"},
		// (Options sshd refuses — a value not quoted, a quote not closed, options alone: no entry.)
		{"command=unquoted ssh-ed25519 KKKK\n", "ssh-ed25519 KKKK x", "OK added"},
		{`command="never closed ssh-ed25519 LLLL` + "\n", "ssh-ed25519 LLLL x", "OK added"},
		{"no-pty,no-agent-forwarding ssh-ed25519 MMMM c\n", "ssh-ed25519 MMMM x", "OK present"},
		// (An RSA key's other names, as sshd takes them for it.)
		{"rsa-sha2-256 NNNN\n", "ssh-rsa NNNN x", "OK present"},
		{`from="1.2.3.4" rsa-sha2-512 OOOO c` + "\n", "ssh-rsa OOOO x", "OK present"},
		{"rsa-sha2-256 PPPP\n", "ssh-ed25519 PPPP x", "OK added"},
		// (A backslash escapes a quote only — "\\\"" ends no value: sshd's rule.)
		{`from="10.0.0.0/8",command="echo \\"hello\\"" ssh-ed25519 QQQQ` + "\n", "ssh-ed25519 QQQQ x", "OK present"},
		{`command="a\b" ssh-ed25519 RRRR` + "\n", "ssh-ed25519 RRRR x", "OK present"},
		// (Options sshd does not know, a valued one without its value, a flag given one: no entry.)
		{"unknown-option ssh-ed25519 SSSS\n", "ssh-ed25519 SSSS x", "OK added"},
		{"command ssh-ed25519 TTTT\n", "ssh-ed25519 TTTT x", "OK added"},
		{`no-pty="x" ssh-ed25519 UUUU` + "\n", "ssh-ed25519 UUUU x", "OK added"},
		{`NO-PTY,Restrict,Command="x" ssh-ed25519 VVVV` + "\n", "ssh-ed25519 VVVV x", "OK present"},
	} {
		os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(c.file), 0o600)
		if got := run(c.key + "\n"); got != "abc "+c.want {
			t.Errorf("%q: %q", c.file, got)
		}
	}
	os.RemoveAll(filepath.Join(home, ".ssh"))
	// A home's .ssh that is a file: said.
	os.RemoveAll(filepath.Join(home, ".ssh"))
	os.WriteFile(filepath.Join(home, ".ssh"), nil, 0o600)
	if got := run("ssh-ed25519 BBBB x\n"); got != "abc E notdir" {
		t.Errorf("notdir: %q", got)
	}
	// Not writable: said.
	os.Remove(filepath.Join(home, ".ssh"))
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte("ssh-ed25519 AAAA one\n"), 0o400)
	if os.Geteuid() != 0 {
		if got := run("ssh-ed25519 BBBB x\n"); got != "abc E notwritable" {
			t.Errorf("not writable: %q", got)
		}
	}
}

func TestAuthorizedLine(t *testing.T) {
	line, typ, data, ok := authorizedLine("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGrkSXq4CmYDN5ZHPDSDn4ZjSm9D4t0kGJ8F0aBHNfJv me@box `rm -rf ~` $(x)\\y'z")
	if !ok || typ != "ssh-ed25519" || !strings.HasPrefix(data, "AAAA") {
		t.Fatalf("%q %q %q %v", line, typ, data, ok)
	}
	if strings.ContainsAny(line, "`$()\\' ~") && strings.Count(line, " ") != 2 {
		t.Errorf("comment not cleaned: %q", line)
	}
	if strings.ContainsAny(strings.SplitN(line, " ", 3)[2], "`$()\\'~") {
		t.Errorf("comment not cleaned: %q", line)
	}
	if _, _, _, ok := authorizedLine("not a key"); ok {
		t.Errorf("not a key taken")
	}
}

// The name left out: the address, as the form says.
func TestHostNameDefaultsToAddress(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	srv := h.sshServer(t.TempDir())
	r := a.do("POST", "/api/hosts", checkForm(srv.Addr.Port, map[string]any{"name": "  "}))
	mustStatus(t, r, 201, "create")
	if r.json()["name"] != "127.0.0.1" {
		t.Errorf("name: %v", r.json()["name"])
	}
}

// installKeyCommand as each login shell here reads it (the command a host's
// sshd hands to the user's shell): the same script runs, the nonce its one
// argument.
func TestInstallKeyCommandShells(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "dash", "fish"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s: not here", sh)
			continue
		}
		home := t.TempDir()
		c := exec.Command(path, "-c", installKeyCommand("n0nce"))
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		c.Stdin = strings.NewReader("ssh-ed25519 AAAA x\n")
		out, err := c.CombinedOutput()
		if got := strings.TrimSpace(string(out)); err != nil || got != "n0nce OK added" {
			t.Errorf("%s: %q %v", sh, got, err)
		}
	}
}

// Against a real sshd (opt in: CORRAL_TEST_SSHD=host:port, CORRAL_TEST_SSHD_USER,
// CORRAL_TEST_SSHD_PASS — the corral-sshd container's alice): a check with a
// good and a bad password, the key put on and found there again, the key
// then signed in with, and a linked authorized_keys said, not replaced. The
// user's ~/.ssh is put back as it was after.
func TestHostCheckRealSSHD(t *testing.T) {
	addr := os.Getenv("CORRAL_TEST_SSHD")
	user, pass := os.Getenv("CORRAL_TEST_SSHD_USER"), os.Getenv("CORRAL_TEST_SSHD_PASS")
	if addr == "" || user == "" || pass == "" {
		t.Skip("set CORRAL_TEST_SSHD, CORRAL_TEST_SSHD_USER and CORRAL_TEST_SSHD_PASS")
	}
	hostPart, portPart, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portPart)
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	form := func(extra map[string]any) map[string]any {
		m := map[string]any{"address": hostPart, "port": port, "username": user, "auth": "password", "password": pass}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	// (As the user, by password: ~/.ssh kept aside, put back after.)
	sh := func(script string) string {
		t.Helper()
		c, err := sshDialPassword(addr, user, pass)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		s, _ := c.NewSession()
		defer s.Close()
		out, _ := s.CombinedOutput("sh -c " + quoteForTest(script))
		return string(out)
	}
	tag := "corral-hostcheck-" + strconv.FormatInt(int64(os.Getpid()), 10)
	sh(`rm -rf ~/` + tag + `; mkdir ~/` + tag + ` && if [ -e ~/.ssh ]; then mv ~/.ssh ~/` + tag + `/ssh.kept; fi`)
	t.Cleanup(func() {
		sh(`rm -rf ~/.ssh; if [ -e ~/` + tag + `/ssh.kept ]; then mv ~/` + tag + `/ssh.kept ~/.ssh; fi; rm -rf ~/` + tag)
	})
	r := a.do("POST", "/api/hosts/test", form(nil))
	if j := r.json(); j["ok"] != true || j["login_tested"] != true {
		t.Fatalf("good password: %s", r.Body)
	}
	r = a.do("POST", "/api/hosts/test", form(map[string]any{"password": "not-" + pass}))
	if j := r.json(); j["stage"] != "auth" || j["reason"] != "refused" {
		t.Errorf("bad password: %s", r.Body)
	}
	r = a.do("POST", "/api/credentials", map[string]any{"name": "laptop", "mode": "generate"})
	mustStatus(t, r, 201, "key")
	keyID := r.json()["id"]
	kf := form(map[string]any{"auth": "key", "credential_id": keyID, "password": ""})
	r = a.do("POST", "/api/hosts/test", kf)
	if j := r.json(); j["ok"] == true {
		t.Fatalf("the key signed in before it was put there: %s", r.Body)
	}
	kf["install_password"] = pass
	r = a.do("POST", "/api/hosts/install-key", kf)
	if j := r.json(); j["ok"] != true || j["installed"] != "added" {
		t.Fatalf("install: %s", r.Body)
	}
	r = a.do("POST", "/api/hosts/install-key", kf)
	if j := r.json(); j["ok"] != true || j["installed"] != "present" {
		t.Errorf("again: %s", r.Body)
	}
	if got := strings.TrimSpace(sh(`grep -c . ~/.ssh/authorized_keys; stat -c %a ~/.ssh ~/.ssh/authorized_keys`)); got != "1\n700\n600" {
		t.Errorf("authorized_keys: %q", got)
	}
	delete(kf, "install_password")
	r = a.do("POST", "/api/hosts/test", kf)
	if j := r.json(); j["ok"] != true || j["login_tested"] != true {
		t.Errorf("the key, after: %s", r.Body)
	}
	// Linked: said, not replaced.
	sh(`rm -f ~/.ssh/authorized_keys; : > ~/` + tag + `/real; ln -s ~/` + tag + `/real ~/.ssh/authorized_keys`)
	kf["install_password"] = pass
	r = a.do("POST", "/api/hosts/install-key", kf)
	if j := r.json(); j["stage"] != "install" || j["reason"] != "link" {
		t.Errorf("link: %s", r.Body)
	}
	if got := strings.TrimSpace(sh(`[ -L ~/.ssh/authorized_keys ] && echo link; wc -c < ~/` + tag + `/real`)); got != "link\n0" {
		t.Errorf("after the link: %q", got)
	}
}

func sshDialPassword(addr, user, pass string) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
}

func quoteForTest(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Editing a host with a stored password, its address or port changed: the
// password is not sent to the new one (it must be typed).
func TestHostTestStoredPasswordStaysHome(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	srv := h.sshServer(t.TempDir())
	r := a.do("POST", "/api/hosts", checkForm(srv.Addr.Port, map[string]any{"name": "box"}))
	mustStatus(t, r, 201, "create")
	id := r.json()["id"]
	other := h.sshServer(t.TempDir())
	r = a.do("POST", "/api/hosts/test", checkForm(other.Addr.Port, map[string]any{"id": id, "password": ""}))
	if ex, _ := r.json()["extra"].(map[string]any); r.Status != 422 || ex["field"] != "password" {
		t.Errorf("moved: %d %s", r.Status, r.Body)
	}
	if other.Attempts() != 0 {
		t.Errorf("the stored password was sent to the new address")
	}
	// (As it was: the stored one used.)
	r = a.do("POST", "/api/hosts/test", checkForm(srv.Addr.Port, map[string]any{"id": id, "password": ""}))
	if r.Status != 200 || r.json()["ok"] != true {
		t.Errorf("same address: %d %s", r.Status, r.Body)
	}
}

// The key signed in with after it is put on: to the machine the password
// went to (its key then), not one that took its place meanwhile.
func TestHostInstallKeyVerifiesTheSameMachine(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	home := t.TempDir()
	srv := h.sshServer(home)
	srv.AcceptKeysFrom(filepath.Join(home, ".ssh", "authorized_keys"))
	run := testutil.ShellExec(home, func(cmd string) bool { return strings.HasPrefix(cmd, "sh -c ") })
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		srv.RotateHostKey() // (another machine answers from now on)
		return run(cmd)
	})
	r := a.do("POST", "/api/credentials", map[string]any{"name": "laptop", "mode": "generate"})
	mustStatus(t, r, 201, "key")
	f := checkForm(srv.Addr.Port, map[string]any{"auth": "key", "credential_id": r.json()["id"], "password": "", "install_password": "alice-pass"})
	r = a.do("POST", "/api/hosts/install-key", f)
	mustStatus(t, r, 200, "install")
	if j := r.json(); j["ok"] == true || j["stage"] != "verify:hostkey" {
		t.Errorf("another machine took it: %s", r.Body)
	}
}

// A host that says, before its key is decided on, words a refused sign-in
// would have: not taken for one (nor a probe for reached).
func TestHostTestDisconnectWordsNotAuth(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("SSH-2.0-liar\r\n"))
			desc := "ssh: unable to authenticate, unexpected message type 51 (expected 60)"
			payload := []byte{1, 0, 0, 0, 2}
			payload = append(payload, byte(len(desc)>>24), byte(len(desc)>>16), byte(len(desc)>>8), byte(len(desc)))
			payload = append(payload, desc...)
			payload = append(payload, 0, 0, 0, 0)
			pad := 8 - (5+len(payload))%8
			if pad < 4 {
				pad += 8
			}
			n := 1 + len(payload) + pad
			pkt := []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), byte(pad)}
			pkt = append(pkt, payload...)
			pkt = append(pkt, make([]byte, pad)...)
			c.Write(pkt)
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	for _, f := range []map[string]any{checkForm(port, map[string]any{"auth": "ask", "password": ""}), checkForm(port, nil)} {
		r := a.do("POST", "/api/hosts/test", f)
		mustStatus(t, r, 200, "test")
		if j := r.json(); j["ok"] == true || j["stage"] == "auth" {
			t.Errorf("%v: %s", f["auth"], r.Body)
		}
	}
}

// A host's pinned key that cannot be read (not "none yet"): the check stops
// before dialling — no password sent to whatever key a host shows.
func TestHostTestPinnedKeyUnreadable(t *testing.T) {
	h := newHarness(t)
	a := h.member(h.admin(), "alice")
	srv := h.sshServer(t.TempDir())
	r := a.do("POST", "/api/hosts", checkForm(srv.Addr.Port, map[string]any{"name": "box"}))
	mustStatus(t, r, 201, "create")
	id := r.json()["id"]
	old := checkHostKey
	checkHostKey = func(*Server, context.Context, int64, int64) (*store.HostKey, error) { return nil, errors.New("database is locked") }
	t.Cleanup(func() { checkHostKey = old })
	before := srv.Attempts()
	r = a.do("POST", "/api/hosts/test", checkForm(srv.Addr.Port, map[string]any{"id": id, "password": ""}))
	if r.Status != 500 || srv.Attempts() != before {
		t.Errorf("unreadable pin: %d %s, %d password checks", r.Status, r.Body, srv.Attempts()-before)
	}
	// (None yet — ErrNotFound — is a host not pinned: checked.)
	checkHostKey = func(*Server, context.Context, int64, int64) (*store.HostKey, error) { return nil, store.ErrNotFound }
	r = a.do("POST", "/api/hosts/test", checkForm(srv.Addr.Port, map[string]any{"id": id, "password": ""}))
	if r.Status != 200 || r.json()["ok"] != true {
		t.Errorf("not pinned: %d %s", r.Status, r.Body)
	}
}
