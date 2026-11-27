package web

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSideQuestionText(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"what is this file for?", "what is this file for?", true},
		{"  two\nlines\tand a tab  ", "two\nlines\tand a tab", true},
		{"中文的问题", "中文的问题", true},
		{"", "", false},
		{"   \n ", "", false},
		{"- a list item?", "- a list item?", true},
		{"  --help", "--help", true},
		{"bell\a", "", false},
		{"esc\x1b[2J", "", false},
		{"nul\x00", "", false},
		{"cr\rx", "", false},
		{"del\x7f", "", false},
		{"sep\u2028x", "", false},
		{"bad\xffutf8", "", false},
		{strings.Repeat("问", sideQuestionMax), strings.Repeat("问", sideQuestionMax), true},
		{strings.Repeat("问", sideQuestionMax+1), "", false},
	} {
		got, ok := sideQuestionText(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%q: %q %v", c.in, got, ok)
		}
	}
	for _, m := range []string{"opus", "claude-opus-5-5", "claude-sonnet-5-5[1m]", "us.anthropic.claude-opus-5-5-v1:0", "arn:aws:bedrock:us-east-1:1:inference-profile/x"} {
		if !sideModel.MatchString(m) {
			t.Errorf("model %q refused", m)
		}
	}
	for _, m := range []string{"-p", "--bare", "a b", "a'b", "a\nb", "$(x)", ""} {
		if sideModel.MatchString(m) {
			t.Errorf("model %q taken", m)
		}
	}
}

func TestSideQuestionCommandFlags(t *testing.T) {
	cmd := sideQuestionCommand(42, 4242, "0b7c2a1e-1111-4222-8333-444455556666", "claude-opus-5-5", "abcd")
	for _, want := range []string{"--resume", "--fork-session", "--no-session-persistence", `--tools ""`, "--strict-mcp-config", `--mcp-config '\''{"mcpServers":{}}'\''`, `--settings '\''{"disableAllHooks":true}'\''`, "--output-format json", "-p "} {
		if !strings.Contains(cmd, want) {
			t.Errorf("no %q in %s", want, cmd)
		}
	}
	for _, not := range []string{"--bare", "--dangerously", "--allowedTools", "--permission-mode"} {
		if strings.Contains(cmd, not) {
			t.Errorf("%q in %s", not, cmd)
		}
	}
	if !strings.HasSuffix(cmd, " corral 42 4242 '0b7c2a1e-1111-4222-8333-444455556666' 'claude-opus-5-5' 'abcd' 90") {
		t.Errorf("arguments: %s", cmd)
	}
}

func TestParseSideAnswer(t *testing.T) {
	const n = "abcd"
	ok := `{"type":"result","subtype":"success","is_error":false,"result":"It **parses** it.","duration_ms":2300,"total_cost_usd":0.0031}`
	for _, c := range []struct {
		out  string
		want sideAnswer
		err  bool
	}{
		{"abcd OUT\n" + ok + "\n\nabcd RC 0\n", sideAnswer{OK: true, Answer: "It **parses** it.", DurationMs: 2300, CostUSD: 0.0031}, false},
		{"E gone\n", sideAnswer{Reason: "gone"}, false},
		{"E noclaude\n", sideAnswer{Reason: "noclaude"}, false},
		{"E what\n", sideAnswer{}, true},
		{"garbage", sideAnswer{}, true},
		{"abcd OUT\n" + ok[:40], sideAnswer{Reason: "cut"}, false},
		{"abcd OUT\n\nabcd RC 124\n", sideAnswer{Reason: "timeout"}, false},
		{"abcd OUT\n\nabcd RC 137\n", sideAnswer{Reason: "timeout"}, false},
		{"abcd OUT\nNot logged in\n\nabcd RC 1\n", sideAnswer{Reason: "failed"}, false},
		{"abcd OUT\n" + `{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","duration_ms":10}` + "\n\nabcd RC 1\n", sideAnswer{Reason: "error", Answer: "Not logged in · Please run /login", DurationMs: 10}, false},
		{"abcd OUT\n" + `{"type":"result","subtype":"error_max_turns","is_error":false,"duration_ms":10}` + "\n\nabcd RC 0\n", sideAnswer{Reason: "error", DurationMs: 10}, false},
		// (A marker in the answer itself is not its end: the nonce is the gateway's.)
		{"abcd OUT\n" + `{"type":"result","subtype":"success","is_error":false,"result":"x\nRC 0\nzz RC 1"}` + "\n\nabcd RC 0\n", sideAnswer{OK: true, Answer: "x\nRC 0\nzz RC 1"}, false},
	} {
		got, err := parseSideAnswer(c.out, n)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%q: %+v %v", c.out, got, err)
		}
	}
}

// The host's script run here, with a stand-in claude (no model is asked):
// the copy runs where the process runs, with its config folder and sign-in
// and nothing else of its environment, the question its input only.
func TestSideQuestionCommandRuns(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("no timeout")
	}
	home := t.TempDir()
	// (Its own HOME, its stand-in first on the script's PATH: the real
	// Claude Code in ~/.local/bin is never what runs.)
	bin := filepath.Join(home, ".local", "bin")
	seen := filepath.Join(home, "seen")
	work := filepath.Join(home, "work dir")
	for _, d := range []string{bin, seen, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stand := "#!/bin/sh\n" +
		"S='" + seen + "'\n" +
		"cat > \"$S/stdin\"\n" +
		"printf '%s\\n' \"$@\" > \"$S/args\"\n" +
		"pwd > \"$S/cwd\"\n" +
		"env > \"$S/env\"\n" +
		"[ -n \"$SIDE_SLEEP\" ] && sleep \"$SIDE_SLEEP\"\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"result":"stand-in **answer**","duration_ms":2300,"total_cost_usd":0.003}\n'` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stand), 0o755); err != nil {
		t.Fatal(err)
	}
	// The "agent": a process of its own, its folder and environment its own.
	agent := exec.Command("sleep", "60")
	agent.Dir = work
	// (Its PATH: a folder by a relative name, which is not looked in, and one of nvm's, say.)
	nvm := filepath.Join(home, "nvm", "bin")
	os.MkdirAll(nvm, 0o755)
	os.MkdirAll(filepath.Join(work, "rel"), 0o755)
	agent.Env = []string{"PATH=rel:" + nvm + ":/usr/bin:/bin", "AWS_SHARED_CREDENTIALS_FILE=/agent/creds", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "cfg"), "ANTHROPIC_BASE_URL=https://relay.example.com/x", "CLAUDE_CODE_OAUTH_TOKEN=agent-token", "TMUX_PANE=%9", "HERDR_PANE_ID=p1", "SECRET_OTHER=no",
		// (A value with a line break that reads as another variable: neither taken.)
		"SMUGGLE=x\nANTHROPIC_AUTH_TOKEN=stolen", "ANTHROPIC_API_KEY=k1\nANTHROPIC_BASE_URL=https://evil.example",
		"SMUGGLE2=y\x01ANTHROPIC_AUTH_TOKEN=stolen2"}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Process.Kill(); agent.Wait() })
	pid := int64(agent.Process.Pid)
	var start int64
	for i := 0; i < 50 && start == 0; i++ {
		b, _ := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/stat")
		s := string(b)
		if j := strings.LastIndex(s, ") "); j >= 0 {
			if f := strings.Fields(s[j+2:]); len(f) > 19 {
				start, _ = strconv.ParseInt(f[19], 10, 64)
			}
		}
		if start == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	// (Its session file, as Claude Code keeps one under its CLAUDE_CONFIG_DIR: at that session.)
	sessions := filepath.Join(home, "cfg", "sessions")
	os.MkdirAll(sessions, 0o755)
	setSession := func(sid string) {
		doc := `{"pid":` + strconv.FormatInt(pid, 10) + `,"sessionId":"` + sid + `","procStart":"` + strconv.FormatInt(start, 10) + `"}`
		if err := os.WriteFile(filepath.Join(sessions, strconv.FormatInt(pid, 10)+".json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setSession("0b7c2a1e-1111-4222-8333-444455556666")
	// (Its environment read: what /proc says once it has exec'd.)
	for i := 0; i < 100; i++ {
		if b, _ := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/environ"); strings.Contains(string(b), "TMUX_PANE") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	run := func(start int64, model, q string, extra ...string) string {
		c := exec.Command("sh", "-c", sideQuestionCommand(pid, start, "0b7c2a1e-1111-4222-8333-444455556666", model, "f00d"))
		// (The login shell's own: a key and a pane of its own, none of them to reach the copy.)
		c.Env = append([]string{"HOME=" + home, "PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=outer", "TMUX_PANE=%1", "CLAUDE_CONFIG_DIR=/outer", "AWS_PROFILE=outer", "AWS_CONFIG_FILE=/outer/aws", "CLAUDE_CODE_OAUTH_TOKEN=outer-token"}, extra...)
		c.Stdin = strings.NewReader(q)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return string(out)
	}
	q := "-- what does `x` do?\n'; rm -rf / #$(id)"
	out := run(start, "claude-opus-5-5", q)
	a, err := parseSideAnswer(out, "f00d")
	if err != nil || !a.OK || a.Answer != "stand-in **answer**" || a.DurationMs != 2300 || a.CostUSD != 0.003 {
		t.Fatalf("answer %+v %v: %q", a, err, out)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(seen, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("stdin"); got != q {
		t.Errorf("stdin %q", got)
	}
	wantArgs := "-p\n--resume\n0b7c2a1e-1111-4222-8333-444455556666\n--fork-session\n--no-session-persistence\n--tools\n\n--strict-mcp-config\n--mcp-config\n{\"mcpServers\":{}}\n--settings\n{\"disableAllHooks\":true}\n--output-format\njson\n--model\nclaude-opus-5-5\n"
	if got := read("args"); got != wantArgs {
		t.Errorf("args %q", got)
	}
	if got := strings.TrimSpace(read("cwd")); got != work {
		t.Errorf("cwd %q", got)
	}
	env := "\n" + read("env")
	for _, want := range []string{"\nCLAUDE_CONFIG_DIR=" + filepath.Join(home, "cfg") + "\n", "\nANTHROPIC_BASE_URL=https://relay.example.com/x\n", "\nCLAUDE_CODE_OAUTH_TOKEN=agent-token\n", "\nAWS_SHARED_CREDENTIALS_FILE=/agent/creds\n", "\nCORRAL_SIDE_COPY=1\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("no %q in env", want)
		}
	}
	for _, not := range []string{"\nANTHROPIC_API_KEY=", "\nANTHROPIC_AUTH_TOKEN=", "evil.example", "stolen", "\x01", "\nTMUX_PANE=", "\nHERDR_PANE_ID=", "\nSECRET_OTHER=", "\nSMUGGLE=", "/outer", "\nAWS_PROFILE=", "\nAWS_CONFIG_FILE=", "outer-token"} {
		if strings.Contains(env, not) {
			t.Errorf("%q in env", not)
		}
	}
	// (No model asked for: none given.)
	run(start, "", "q")
	if strings.Contains(read("args"), "--model") {
		t.Errorf("model given: %q", read("args"))
	}
	// Another process at that pid (its start not this one's): nothing run.
	os.Remove(filepath.Join(seen, "args"))
	if a, _ := parseSideAnswer(run(start+1, "", "q"), "f00d"); a.Reason != "gone" {
		t.Errorf("another process: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(seen, "args")); err == nil {
		t.Errorf("ran for another process")
	}
	// The process now at another session (/clear, /resume since it was found): nothing run.
	setSession("9f9f9f9f-1111-4222-8333-444455556666")
	if a, _ := parseSideAnswer(run(start, "", "q"), "f00d"); a.Reason != "gone" {
		t.Errorf("another session: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(seen, "args")); err == nil {
		t.Errorf("ran for another session")
	}
	setSession("0b7c2a1e-1111-4222-8333-444455556666")
	// Claude Code on the agent's own PATH only (nvm's, say): found there, its folder first on
	// the copy's PATH and no more of the agent's; a folder by a relative name not looked in.
	os.WriteFile(filepath.Join(work, "rel", "claude"), []byte("#!/bin/sh\necho wrong > '"+seen+"/wrong'\n"), 0o755)
	os.Rename(filepath.Join(bin, "claude"), filepath.Join(nvm, "claude"))
	// (One on the login shell's PATH that may not be run — BusyBox's command -v names it — passed over.)
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho wrong > '"+seen+"/wrong'\n"), 0o644)
	os.Remove(filepath.Join(seen, "env"))
	if a, err := parseSideAnswer(run(start, "", "q"), "f00d"); err != nil || !a.OK {
		t.Errorf("on the agent's PATH: %+v %v", a, err)
	}
	if _, err := os.Stat(filepath.Join(seen, "wrong")); err == nil {
		t.Errorf("a relative PATH folder looked in")
	}
	if p := "\n" + read("env"); !strings.Contains(p, "\nPATH="+nvm+":"+home+"/.local/bin:") || strings.Contains(p, "rel:") {
		t.Errorf("the copy's PATH: %q", p[strings.Index(p, "\nPATH="):][:200])
	}
	os.Remove(filepath.Join(bin, "claude"))
	os.Rename(filepath.Join(nvm, "claude"), filepath.Join(bin, "claude"))
	// Nothing of the run left behind, the copy quick: no watch still sleeping.
	runSecs := func(secs int, q string) (sideAnswer, time.Duration) {
		began := time.Now()
		c := exec.Command("sh", "-c", sideQuestionRun(pid, start, "0b7c2a1e-1111-4222-8333-444455556666", "", "f00d", secs))
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		c.Stdin = strings.NewReader(q)
		out, _ := c.Output()
		a, err := parseSideAnswer(string(out), "f00d")
		if err != nil {
			t.Errorf("%v: %q", err, out)
		}
		return a, time.Since(began)
	}
	sleeping := func(secs string) bool {
		ds, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, d := range ds {
			if b, _ := os.ReadFile(d); string(b) == "sleep\x00"+secs+"\x00" {
				return true
			}
		}
		return false
	}
	if a, _ := runSecs(37, "q"); !a.OK {
		t.Errorf("quick: %+v", a)
	}
	time.Sleep(200 * time.Millisecond)
	if sleeping("37") {
		t.Errorf("the watch's sleep left running")
	}
	// A copy that ends at once, before the watch's setsid may have run: never waited out.
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"x\"}\\n'\n"), 0o755)
	for i := 0; i < 30; i++ {
		if a, took := runSecs(37, ""); !a.OK || took > 3*time.Second {
			t.Fatalf("instant copy %d: %+v after %s", i, a, took)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if sleeping("37") {
		t.Errorf("an instant copy's watch left sleeping")
	}
	child := func() bool {
		b, err := os.ReadFile(filepath.Join(seen, "child"))
		if err != nil {
			return false
		}
		_, err = os.Stat("/proc/" + strings.TrimSpace(string(b)))
		return err == nil
	}
	for _, c := range []struct{ how, script string }{
		// (A wrapper, its child the one that would ask the model, both ignoring TERM: its time up.)
		{"hangs", "#!/bin/sh\ntrap '' TERM\nsh -c \"trap '' TERM; echo \\$\\$ > '" + seen + "/child'; sleep 30\" &\nwait\n"},
		// (A wrapper that ends at once, its child left holding the answer's pipe.)
		{"leaves", "#!/bin/sh\nsh -c \"echo \\$\\$ > '" + seen + "/child'; sleep 30\" &\nsleep 0.3\nexit 0\n"},
	} {
		os.WriteFile(filepath.Join(bin, "claude"), []byte(c.script), 0o755)
		os.Remove(filepath.Join(seen, "child"))
		a, took := runSecs(2, "q")
		if took > 8*time.Second || (c.how == "hangs" && a.Reason != "timeout") {
			t.Errorf("%s: %+v after %s", c.how, a, took)
		}
		for i := 0; i < 20 && child(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if child() {
			t.Errorf("%s: the wrapper's child still runs", c.how)
		}
	}
}

func TestSideQuestionEndpoint(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	home := t.TempDir()
	srv := h.sshServer(home)
	ran := false
	// (Answered here, nothing run: the scan finds no process at all — not
	// this machine's own looked at — and the side question is noted if asked.)
	srv.HandleExec(func(cmd string) func(ssh.Channel) {
		if !strings.Contains(cmd, "--fork-session") && !strings.Contains(cmd, "hostnow=") {
			return nil // (the session's own terminal, as the server keeps it)
		}
		return func(ch ssh.Channel) {
			if strings.Contains(cmd, "--fork-session") {
				ran = true
				ch.Write([]byte("E gone\n"))
			} else {
				ch.Write([]byte("tmux=\x00panetty=\x00hostnow=1\x00"))
			}
			ch.CloseWrite()
			ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
			ch.Close()
		}
	})
	r := admin.do("POST", "/api/hosts", map[string]any{"name": "box", "address": "127.0.0.1", "port": srv.Addr.Port,
		"username": "alice", "auth": "password", "password": "alice-pass", "persistence": "tmux", "tmux_session": "work"})
	mustStatus(t, r, 201, "create host")
	r = admin.do("POST", "/api/sessions", map[string]any{"host_id": r.json()["id"]})
	mustStatus(t, r, 201, "open session")
	url := "/api/sessions/" + r.json()["id"].(string) + "/conversation/side"
	good := func() map[string]any {
		return map[string]any{"pane": "%3", "pid": 4321, "start": 99, "session": "0b7c2a1e-1111-4222-8333-444455556666", "question": "why?"}
	}
	for i, bad := range []func(m map[string]any){
		func(m map[string]any) { m["question"] = "" },
		func(m map[string]any) { m["question"] = "a\x1bb" },
		func(m map[string]any) { m["question"] = strings.Repeat("x", sideQuestionMax+1) },
		func(m map[string]any) { m["model"] = "--bare" },
		func(m map[string]any) { m["model"] = "a b" },
		func(m map[string]any) { m["pane"] = "3" },
		func(m map[string]any) { m["pid"] = 0 },
		func(m map[string]any) { m["start"] = -1 },
		func(m map[string]any) { m["session"] = "x" },
		func(m map[string]any) { m["session"] = convoNoSession },
	} {
		m := good()
		bad(m)
		if r := admin.do("POST", url, m); r.Status != http.StatusBadRequest {
			t.Errorf("%d: %d %s", i, r.Status, r.Body)
		}
	}
	// No such agent in the pane: refused, and nothing asked of a model.
	if r := admin.do("POST", url, good()); r.Status != http.StatusConflict || !strings.Contains(string(r.Body), "agent_changed") {
		t.Errorf("no agent: %d %s", r.Status, r.Body)
	}
	if ran {
		t.Errorf("the side question ran")
	}
}

// A side question's copy (claude -p, its session file's entrypoint
// "sdk-cli", marked in its environment) is no agent: the scan leaves it
// out; a Claude Code beside it, and a claude -p of the person's own, are
// found as before.
func TestConvoLocateSkipsSideCopy(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	home := t.TempDir()
	sessions := filepath.Join(home, ".claude", "sessions")
	os.MkdirAll(sessions, 0o755)
	startOf := func(pid int) string {
		for i := 0; i < 50; i++ {
			b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if j := strings.LastIndex(string(b), ") "); j >= 0 {
				if f := strings.Fields(string(b)[j+2:]); len(f) > 19 {
					return f[19]
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("no start time")
		return ""
	}
	procs := map[string]int{}
	for name, entry := range map[string]string{"agent": "cli", "copy": "sdk-cli", "own": "sdk-cli"} {
		p := exec.Command("sleep", "60")
		p.Env = []string{"PATH=/usr/bin:/bin"}
		if name == "copy" {
			p.Env = append(p.Env, sideCopyMark)
		}
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Process.Kill(); p.Wait() })
		procs[name] = p.Process.Pid
		sid := map[string]string{"agent": "0b7c2a1e-1111-4222-8333-444455556666", "copy": "9f9f9f9f-1111-4222-8333-444455556666", "own": "7e7e7e7e-1111-4222-8333-444455556666"}[name]
		doc := `{"pid":` + strconv.Itoa(p.Process.Pid) + `,"sessionId":"` + sid + `","procStart":"` + startOf(p.Process.Pid) + `","kind":"interactive","entrypoint":"` + entry + `"}`
		if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(p.Process.Pid)+".json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := exec.Command("sh", "-c", convoLocateCommand("herdr", "", ""))
	c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	l := parseConvoLocate(string(out))
	found := map[int64]string{}
	for _, p := range l.procs {
		found[p.pid] = p.sid
	}
	if found[int64(procs["agent"])] != "0b7c2a1e-1111-4222-8333-444455556666" {
		t.Errorf("the agent not found: %v", found)
	}
	if _, ok := found[int64(procs["copy"])]; ok {
		t.Errorf("the side question's copy taken for an agent")
	}
	// (A "claude -p" of the person's own, unmarked: found as before.)
	if found[int64(procs["own"])] != "7e7e7e7e-1111-4222-8333-444455556666" {
		t.Errorf("a claude -p of the person's own not found: %v", found)
	}
}
