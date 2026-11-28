package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPanelKeys(t *testing.T) {
	d := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, c := range []struct {
		keys   []string
		digest string
		ok     bool
	}{
		{[]string{"up"}, "", true},
		{[]string{"esc"}, "", true},
		{[]string{"a"}, "", true},
		{[]string{"shift+tab"}, "", false}, // (under a plan, it approves)
		{[]string{"shift+tab"}, d, true},
		{[]string{"backspace"}, "", true},
		{[]string{"enter"}, d, true},
		{[]string{"space"}, d, true},
		{[]string{"3"}, d, true},
		// (What settles something: never without the screen it goes on.)
		{[]string{"enter"}, "", false},
		{[]string{"space"}, "nope", false},
		{[]string{"3"}, "", false},
		// (One at a time, of those only: no chord, nothing a shell or tmux reads.)
		{[]string{"up", "enter"}, d, false},
		{nil, "", false},
		{[]string{"ctrl+c"}, "", false},
		{[]string{"C-c"}, "", false},
		{[]string{";"}, "", false},
		{[]string{"-"}, "", false},
		{[]string{"ab"}, "", false},
		{[]string{"\n"}, "", false},
		{[]string{"中"}, "", false},
	} {
		if got := panelOK(c.keys, c.digest); got != c.ok {
			t.Errorf("%q %q: %v", c.keys, c.digest, got)
		}
	}
}

// The gateway's digest of a screen and the host's, the same whatever it holds;
// and of two reads differing only in colours, the same; differing in words, not.
func TestPanelDigest(t *testing.T) {
	for _, s := range []string{
		"\x1b[1;38;5;75mSettings\x1b[0m\n  \x1b[7m❯ Auto-compact\x1b[0m  \x1b[32mtrue\n\n",
		"no newline at its end",
		"",
		"\x1b]8;id=1;https://a.example/\x1b\\link\x1b]8;;\x1b\\\n",
		"中文 ⌕ …\r\nnext\n",
		"with\x00nul\n",
		"\xff\xfe not utf-8\n",
		"a\x1b[38:2::1:2:3mb\n",
		"\x1b[38;5;246m\x1b[49m●\x1b[39m Needs permission\n  ● indented\n⏺ mac\n● ⏺ two\nwords ● here\n",
	} {
		c := exec.Command("sh", "-c", panelDigestPipe)
		c.Stdin = strings.NewReader(s)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if host, _, _ := strings.Cut(string(out), " "); host != panelDigest([]byte(s)) {
			t.Errorf("%q: host %s, gateway %s", s, host, panelDigest([]byte(s)))
		}
	}
	if panelDigest([]byte("● Needs permission\n")) != panelDigest([]byte("  Needs permission\n")) {
		t.Error("a tool's blinking mark counts")
	}
	if panelDigest([]byte("  a ● b\n")) == panelDigest([]byte("  a   b\n")) {
		t.Error("a mark among words does not count")
	}
	if panelDigest([]byte("\x1b[7m❯\x1b[0m a\n")) != panelDigest([]byte("❯ a\n")) {
		t.Error("colours count")
	}
	if panelDigest([]byte("❯ a\n  b\n")) == panelDigest([]byte("  a\n❯ b\n")) {
		t.Error("the choice does not count")
	}
}

// The digest an answer goes with: the gateway's and the host's the same, on a
// screen as read with no colours, whichever way its tool's row has blinked.
func TestScreenDigestSteady(t *testing.T) {
	for _, s := range []string{"plain\n", "no newline at its end", "with\x00nul\n", "● ⏺ two marks\n", "● Needs permission\n Do you want to proceed?\n ❯ 1. Yes\n", "  ⏺ row\n\twords ● stay\n", "中文\n\n"} {
		c := exec.Command("sh", "-c", steadyPipe+" | sha256sum")
		c.Stdin = strings.NewReader(s)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		_, gw, _ := screenOf([]byte(s))
		if host, _, _ := strings.Cut(string(out), " "); host != gw {
			t.Errorf("%q: host %s, gateway %s", s, host, gw)
		}
	}
	_, a, _ := screenOf([]byte("● Needs permission\n ❯ 1. Yes\n"))
	_, b, _ := screenOf([]byte("  Needs permission\n ❯ 1. Yes\n"))
	if a != b {
		t.Error("the screen's digest changes as its row blinks")
	}
	_, c, _ := screenOf([]byte("  Needs permission\n ❯ 2. No\n"))
	if a == c {
		t.Error("another choice is the same screen")
	}
	if strings.Contains(steadyAwk, "'") {
		t.Fatal("steadyAwk contains a single quote")
	}
	// Multi-tab AskUserQuestion + Review screens: Go and every installed awk
	// agree byte-for-byte, and volatile chrome outside the dialog is ignored.
	askBase := strings.Join([]string{
		"✽ Orchestrating… (13m 18s · ↓ 44.1k tokens)",
		"────────────────────────────────────────────────────────────",
		"←  ☒ 登录范围  ☐ 密码登录  ☐ 生产应用  ☐ 写入授权  ✔ Submit  →",
		"",
		"接入统一登录后，博客原来的账号密码登录怎么处理?",
		"",
		"❯ 1. 只留给机构与导入账号 (Recommended)",
		"     个人一律走统一登录；4 个公众号机构账号和定时导入程序仍用密码。",
		"  2. 全部关闭",
		"     只能统一登录。",
		"  3. 全部保留",
		"     两种方式并存，登录页同时显示。",
		"  4. Type something.",
		"────────────────────────────────────────────────────────────",
		"  5. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
		"  ● main",
		"  ◯ general-purpose  Run Codex adversarial review of P…  13m 18s",
		"  2 background tasks running · Run Codex adversarial review  13m 18s",
		"  Tip: use Tab to switch questions",
	}, "\n") + "\n"
	askTicked := strings.Join([]string{
		"* Pondering… (13m 19s · ↓ 45.2k tokens)",
		"✻ Waiting for 2 background agents to finish",
		"────────────────────────────────────────────────────────────",
		"←  ☒ 登录范围  ☐ 密码登录  ☐ 生产应用  ☐ 写入授权  ✔ Submit  →",
		"",
		"接入统一登录后，博客原来的账号密码登录怎么处理?",
		"",
		"❯ 1. 只留给机构与导入账号 (Recommended)",
		"     个人一律走统一登录；4 个公众号机构账号和定时导入程序仍用密码。",
		"  2. 全部关闭",
		"     只能统一登录。",
		"  3. 全部保留",
		"     两种方式并存，登录页同时显示。",
		"  4. Type something.",
		"────────────────────────────────────────────────────────────",
		"  5. Chat about this",
		"",
		"Enter to submit · Tab/Arrow keys to navigate ·",
		"Esc to cancel",
		"  ● main",
		"  ◯ general-purpose  Run Codex adversarial review of P…  13m 19s",
		"  Opus 5.5 │ Ctx 182.0K │ $12.34 (13m19s)",
	}, "\n") + "\n"
	loneAsk := strings.Join([]string{
		"✶ Compacting conversation… (4s)",
		"────────────────────────────────────────────────────────────",
		" ☐ 部署策略",
		"",
		"今晚要灰度吗?",
		"❯ 1. 先灰度 10%",
		"  2. 全量发布",
		"  3. Type something.",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	}, "\n") + "\n"
	reviewAsk := strings.Join([]string{
		"✽ Orchestrating… (13m 18s)",
		"←  ☒ 登录范围  ☒ 密码登录  ✔ Submit  →",
		"",
		"Review your answers",
		"",
		" │ ● 接入统一登录后，博客原来的账号密码登录怎么处理?",
		"   → 机构账号特殊处理",
		"",
		"❯ 1. Submit answers",
		"  2. Cancel",
		"  ● main",
		"  ◯ general-purpose  Run Codex  13m 18s",
	}, "\n") + "\n"
	staleAboveBash := strings.Join([]string{
		"←  ☒ Scope  ☐ Style  ✔ Submit  →",
		"Which style?",
		"❯ 1. Minimal",
		"  2. Type something.",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
		"────────────────────────────────────────────────────────────",
		" Bash command",
		"   rm important.txt",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. No",
		" Esc to cancel",
	}, "\n") + "\n"
	for _, s := range []string{askBase, askTicked, loneAsk, reviewAsk, staleAboveBash, "✶ Compacting conversation… (12s)\n  ● main\n  ◯ worker  14s\n"} {
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk", "original-awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			pipe := strings.ReplaceAll(steadyPipe, "LC_ALL=C awk", "LC_ALL=C "+awk) + " | sha256sum"
			cmd := exec.Command("sh", "-c", pipe)
			cmd.Stdin = strings.NewReader(s)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: %v", awk, err)
			}
			_, gw, _ := screenOf([]byte(s))
			if host, _, _ := strings.Cut(string(out), " "); host != gw {
				t.Errorf("%s parity mismatch:\nhost: %s\ngw:   %s\ninput:\n%s", awk, host, gw, s)
			}
		}
	}
	_, dBase, _ := screenOf([]byte(askBase))
	_, dTicked, _ := screenOf([]byte(askTicked))
	if dBase != dTicked {
		t.Error("background timer/spinner/status/footer change changed AskUserQuestion digest")
	}
	// Panel digest also ignores multi-word spinner and agent timer changes.
	if panelDigest([]byte("✶ Compacting conversation… (1s)\n❯ Auto-compact  true\n  ● main\n  ◯ worker  9s\n")) !=
		panelDigest([]byte("✽ Waiting for 2 background tasks… (13m 18s)\n❯ Auto-compact  true\n  ● main\n  ◯ worker  10s\n")) {
		t.Error("panelDigest changed when spinner/agent timer ticked")
	}
	// Stale AskUserQuestion above a Bash permission prompt MUST NOT mask command changes.
	_, dBash1, _ := screenOf([]byte(staleAboveBash))
	_, dBash2, _ := screenOf([]byte(strings.Replace(staleAboveBash, "rm important.txt", "rm -rf /", 1)))
	if dBash1 == dBash2 {
		t.Error("stale AskUserQuestion in scrollback masked Bash permission prompt change")
	}
	// A tab-like line inside the question body (e.g. "☐ Preview ✔ Submit") must
	// NOT become the dialog's top anchor and drop the real question above it.
	fakeTabCases := []struct {
		name    string
		staging string
		timer   string
		prod    string
	}{
		{
			name: "multi-tab with fake tab line in body",
			staging: strings.Join([]string{
				"✽ Orchestrating… (13m 18s)",
				"────────────────────────────────────────────────────────────",
				"←  ☒ Scope  ☐ Action  ✔ Submit  →",
				"",
				"Delete staging?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"────────────────────────────────────────────────────────────",
				"  4. Chat about this",
				"",
				"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
				"  ● main",
				"  ◯ general-purpose  Run Codex  13m 18s",
			}, "\n") + "\n",
			timer: strings.Join([]string{
				"* Pondering… (13m 19s)",
				"────────────────────────────────────────────────────────────",
				"←  ☒ Scope  ☐ Action  ✔ Submit  →",
				"",
				"Delete staging?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"────────────────────────────────────────────────────────────",
				"  4. Chat about this",
				"",
				"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
				"  ● main",
				"  ◯ general-purpose  Run Codex  13m 19s",
			}, "\n") + "\n",
			prod: strings.Join([]string{
				"✽ Orchestrating… (13m 18s)",
				"────────────────────────────────────────────────────────────",
				"←  ☒ Scope  ☐ Action  ✔ Submit  →",
				"",
				"Delete production?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"────────────────────────────────────────────────────────────",
				"  4. Chat about this",
				"",
				"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
				"  ● main",
				"  ◯ general-purpose  Run Codex  13m 18s",
			}, "\n") + "\n",
		},
		{
			name: "lone-tab with fake tab line in body",
			staging: strings.Join([]string{
				"✽ Orchestrating… (13m 18s)",
				"────────────────────────────────────────────────────────────",
				"☐ Action",
				"",
				"Delete staging?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"Enter to select · ↑/↓ to navigate · Esc to cancel",
			}, "\n") + "\n",
			timer: strings.Join([]string{
				"* Pondering… (13m 19s)",
				"────────────────────────────────────────────────────────────",
				"☐ Action",
				"",
				"Delete staging?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"Enter to select · ↑/↓ to navigate · Esc to cancel",
			}, "\n") + "\n",
			prod: strings.Join([]string{
				"✽ Orchestrating… (13m 18s)",
				"────────────────────────────────────────────────────────────",
				"☐ Action",
				"",
				"Delete production?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"Enter to select · ↑/↓ to navigate · Esc to cancel",
			}, "\n") + "\n",
		},
		{
			name: "no rule and no real tab bar above fake tab line",
			staging: strings.Join([]string{
				"Delete staging?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"Enter to select · ↑/↓ to navigate · Esc to cancel",
			}, "\n") + "\n",
			timer: strings.Join([]string{
				"Delete staging?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"Enter to select · ↑/↓ to navigate · Esc to cancel",
			}, "\n") + "\n",
			prod: strings.Join([]string{
				"Delete production?",
				"☐ Preview ✔ Submit",
				"Choose:",
				"❯ 1. Yes",
				"  2. No",
				"  3. Type something.",
				"Enter to select · ↑/↓ to navigate · Esc to cancel",
			}, "\n") + "\n",
		},
	}
	for _, tc := range fakeTabCases {
		_, dStg, _ := screenOf([]byte(tc.staging))
		_, dTim, _ := screenOf([]byte(tc.timer))
		_, dPrd, _ := screenOf([]byte(tc.prod))
		if dStg != dTim {
			t.Errorf("%s: background timer tick changed digest", tc.name)
		}
		if dStg == dPrd {
			t.Errorf("%s: fake tab line masked question change (staging vs production)", tc.name)
		}
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk", "original-awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			hashAwk := func(in string) string {
				pipe := strings.ReplaceAll(steadyPipe, "LC_ALL=C awk", "LC_ALL=C "+awk) + " | sha256sum"
				cmd := exec.Command("sh", "-c", pipe)
				cmd.Stdin = strings.NewReader(in)
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("%s (%s): %v", tc.name, awk, err)
				}
				h, _, _ := strings.Cut(string(out), " ")
				return h
			}
			hStg, hTim, hPrd := hashAwk(tc.staging), hashAwk(tc.timer), hashAwk(tc.prod)
			if hStg != dStg || hTim != dTim || hPrd != dPrd {
				t.Errorf("%s (%s): awk/Go parity mismatch", tc.name, awk)
			}
			if hStg == hPrd {
				t.Errorf("%s (%s): awk masked question change (staging vs production)", tc.name, awk)
			}
		}
	}
}

// What names the terminal shown, on the gateway and the host alike; what moves
// by itself apart, what a key acts on not.
func TestLookDigest(t *testing.T) {
	for _, s := range []string{
		"plain\n", "no newline", "123", "A\n123", "", "with\x00nul\n",
		"\x1b[1;38;5;75m✽ Nucleating… (23s)\x1b[0m\n❯ 1. Yes\n  2. No\n",
		"* Nucleating… (23s · ↓ 1.2k tokens)\n◯ main\n● general-purpose  Count the files      14s\n",
		"❯ ● general-purpose  Count the files      1m 5s\n  ◯ other x 2h\n",
		"● ⏺ two\n  ● indented\nwords ● stay\n\ttabbed 12\n",
		"· not a spinner line\n✻ Worked for 3s\n",
		"中文 … 1234\n",
		"✽ Nucleating… (3s)",
		"● Bash(kill -TERM 1234)\n",
		"● 5s\n",
		"● task\n  5s\n",
		"  ◯ main\n❯ ● general-purpose  Count the files  1m 5s\n\n\n",
		"  ● timeout 5s\n Esc to cancel\n",
		"  ● timeout 5s\n",
		"中文\n◯ main\n● 子代理 x 2h 3m 4s  \n",
		// (OpenCode 2's, as its screens are.)
		"  ┃  ⠼ touch needs-ok\n  ┃\n  ┃  △ Permission required\n  ┃   Allow once   Always allow   Reject\n",
		"\t┃\t⠋ x\n",
		"   ⬝⬝■■■■■■ esc interrupt                 1.0K (1%)\n",
		"■ not a bar\n",
		"  ┃  ⠼\n",
		"⠼ not after a bar of its\n",
	} {
		// (By every awk there is here: a host's may be any.)
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk", "original-awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			c := exec.Command("sh", "-c", strings.ReplaceAll(lookDigestPipe, "LC_ALL=C awk", "LC_ALL=C "+awk))
			c.Stdin = strings.NewReader(s)
			out, err := c.Output()
			if err != nil {
				t.Fatalf("%s, %q: %v", awk, s, err)
			}
			if host, _, _ := strings.Cut(string(out), " "); host != lookDigest([]byte(s)) {
				t.Errorf("%s, %q: host %s, gateway %s", awk, s, host, lookDigest([]byte(s)))
			}
		}
	}
	same := [][2]string{
		{"✽ Nucleating… (23s)\nbox\n", "* Pondering… (24s · ↓ 3k tokens)\nbox\n"},
		{"✶ Compacting conversation… (1s)\nbox\n", "✽ Waiting for 2 background tasks… (13m 18s)\nbox\n"},
		{"✻ Waiting for 1 background agent to finish\nbox\n", "✽ Orchestrating… (13m 18s)\nbox\n"},
		{"  ◯ main\n  ● general-purpose  Count      9s\n", "  ◯ main\n  ● general-purpose  Count     10s\n"},
		{"❯ ● general-purpose  Count 22s\n  ◯ main\n", "❯ ● general-purpose  Count 23s\n  ◯ main\n"},
		{"◯ main\n● task 59s\n\n", "◯ main\n● task 1m 0s\n\n"},
		{"● Needs permission\n", "  Needs permission\n"},
		{"  ┃  ⠼ touch needs-ok\n  ┃   Allow once\n", "  ┃  ⠙ touch needs-ok\n  ┃   Allow once\n"},
		{"   ⬝⬝■■■■■■ esc interrupt   1.0K\n", "   ■■⬝⬝■■■■ esc interrupt   1.0K\n"},
	}
	for _, c := range same {
		if lookDigest([]byte(c[0])) != lookDigest([]byte(c[1])) {
			t.Errorf("what moves by itself counts: %q, %q", c[0], c[1])
		}
	}
	other := [][2]string{
		{"❯ Auto-compact  true\n  Theme\n", "  Auto-compact  true\n❯ Theme\n"},
		{"❯ 1. Yes\n  2. No\n", "  1. Yes\n❯ 2. No\n"},
		{"❯ draft\n", "❯ another draft\n"},
		{"Delete staging?\n❯ 1. Yes\n", "Delete production?\n❯ 1. Yes\n"},
		{"kill -TERM 1234\n❯ 1. Yes\n", "kill -TERM 5678\n❯ 1. Yes\n"},
		{"echo a b\n", "echo ab\n"},
		{"❯ 1. release/123\n  2. release/124\n", "❯ 1. release/124\n  2. release/123\n"},
		{"● Bash(sleep 5s)\n", "● Bash(sleep 9s)  x\n"},
		// (Words of a prompt that look like an agent's row.)
		{"  ● timeout 5s\n ❯ 1. Yes\n Esc to cancel\n", "  ● timeout 9s\n ❯ 1. Yes\n Esc to cancel\n"},
		{"  ● timeout 5s\n", "  ● timeout 9s\n"},
		{"◯ main\n● task 5s\nmore\n", "◯ main\n● task 9s\nmore\n"},
		{"● task\n  5s\n", "● task\n  9s\n"},
		{"  ┃  ⠼ touch a\n", "  ┃  ⠼ touch b\n"},
		{"  ┃   Allow once   Always allow\n", "  ┃   Always allow   Allow once\n"},
		{"⠼ x\n", "⠙ x\n"},
	}
	for _, c := range other {
		if lookDigest([]byte(c[0])) == lookDigest([]byte(c[1])) {
			t.Errorf("what a key acts on does not count: %q, %q", c[0], c[1])
		}
	}
}

// Regression test for Fix Round 3:
// Claude Code renders transient notices (~8s) and effort status on the top-edge
// `▔` overline bar above a dialog or input box. Both Go (steadyScreen /
// panelDigest) and every POSIX awk (steadyPipe / panelDigestPipe) must
// normalize that top-edge row identically, while preserving all dialog body,
// option, cursor, arbitrary prose ending with `▔`, and non-`▔` rule diffs.
func TestScreenDigestOverlineNotice(t *testing.T) {
	// Real Claude Code plan card captures differing ONLY on line 3:
	// before: transient focus-events notice ending in ` ▔`
	// after:  overline bar with effort status ending in ` ▔`
	planBody := strings.Join([]string{
		"",
		"  ──────────────────────────────────────────────────────────────────────────────────────────────",
		"   Ready to code?",
		"",
		"   Here is Claude's plan:",
		"  ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
		"   Plan: add search",
		"",
		"   1. Add a search table                                                                        ↓",
		"  ──────────────────────────────────────────────────────────────────────────────────────────────",
		"   Claude has written up a plan and is ready to execute. Would you like to proceed?",
		"",
		"   ❯ 1. Yes, and use auto mode",
		"     2. Yes, manually approve edits",
		"     3. Tell Claude what to change",
		"        shift+tab to approve with this feedback",
		"",
		"   ctrl+g to edit in Vim · ~/.claude/plans/set-up-the-service-snug-pond.md",
	}, "\n") + "\n"

	noticeBefore := "\n  ⎿ \u00a0/plan to preview\n" +
		" tmux focus-events off · add 'set -g focus-events on' to ~/.tmux.conf and reattach for focus tr… ▔\n" +
		planBody
	noticeAfter := "\n  ⎿ \u00a0/plan to preview\n" +
		"▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔ ◐ medium · /effort ▔\n" +
		planBody
	noticePure := "\n  ⎿ \u00a0/plan to preview\n" +
		"▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔\n" +
		planBody

	// Coloured herdr (`--format ansi`) equivalents with SGR sequences on the
	// overline bar, cursor, and borders.
	ansiBefore := "\n  \x1b[38;5;244m⎿ \u00a0/plan to preview\x1b[0m\n" +
		"\x1b[38;5;244m tmux focus-events off · add 'set -g focus-events on' to ~/.tmux.conf and reattach for focus tr… \x1b[38;5;238m▔\x1b[0m\n" +
		strings.Replace(planBody, "❯", "\x1b[38;5;75m❯\x1b[0m", 1)
	ansiAfter := "\n  \x1b[38;5;244m⎿ \u00a0/plan to preview\x1b[0m\n" +
		"\x1b[38;5;238m▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔ \x1b[38;5;244m◐ medium · /effort \x1b[38;5;238m▔\x1b[0m\n" +
		strings.Replace(planBody, "❯", "\x1b[38;5;75m❯\x1b[0m", 1)

	// Input prompt & dialog-title screens with overline notice vs effort status.
	promptBefore := " tmux focus-events off · add focus-events on ▔\n❯ \n"
	promptAfter := "▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔ ◐ medium · /effort ▔\n❯ \n"
	titleBefore := " transient notice ▔\n   Switch model?\n   ❯ 1. Sonnet\n     2. Opus\n"
	titleAfter := "▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔▔\n   Switch model?\n   ❯ 1. Sonnet\n     2. Opus\n"

	// Counterexamples: MUST remain distinct!
	planDiffBody := strings.Replace(noticeAfter, "1. Add a search table", "1. Drop the users table", 1)
	planDiffOpt := strings.Replace(noticeAfter, "2. Yes, manually approve edits", "2. Reject all edits", 1)
	planDiffCursor := strings.Replace(
		strings.Replace(noticeAfter, "   ❯ 1. Yes, and use auto mode", "     1. Yes, and use auto mode", 1),
		"     2. Yes, manually approve edits", "   ❯ 2. Yes, manually approve edits", 1,
	)
	proseOverline1 := "Here is prose ending with ▔\nNormal follow-up line\n"
	proseOverline2 := "Different prose ending with ▔\nNormal follow-up line\n"
	indentedPlanOverline1 := strings.Replace(noticeAfter, "1. Add a search table                                                                        ↓", "1. Keep staging border ▔", 1)
	indentedPlanOverline2 := strings.Replace(noticeAfter, "1. Add a search table                                                                        ↓", "1. Drop production border ▔", 1)
	dashNotice := "\n  ⎿ \u00a0/plan to preview\n some notice ─\n" + planBody
	dashRule := "\n  ⎿ \u00a0/plan to preview\n────────────────────────────────────────────────────────────\n" + planBody

	// 1. Go vs every installed awk parity on plain screens.
	allPlain := []string{
		noticeBefore, noticeAfter, noticePure,
		promptBefore, promptAfter, titleBefore, titleAfter,
		planDiffBody, planDiffOpt, planDiffCursor,
		proseOverline1, proseOverline2,
		indentedPlanOverline1, indentedPlanOverline2,
		dashNotice, dashRule,
	}
	for idx, s := range allPlain {
		_, gwDigest, _ := screenOf([]byte(s))
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk", "original-awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			pipe := strings.ReplaceAll(steadyPipe, "LC_ALL=C awk", "LC_ALL=C "+awk) + " | sha256sum"
			cmd := exec.Command("sh", "-c", pipe)
			cmd.Stdin = strings.NewReader(s)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("case %d (%s): %v", idx, awk, err)
			}
			if host, _, _ := strings.Cut(string(out), " "); host != gwDigest {
				t.Errorf("case %d (%s) steadyPipe parity mismatch:\nhost: %s\ngw:   %s\ninput:\n%s", idx, awk, host, gwDigest, s)
			}
		}
	}

	// 2. Go panelDigest vs shell panelDigestPipe parity on ANSI-coloured screens.
	for idx, s := range []string{ansiBefore, ansiAfter, "\x1b[38;5;244m" + promptBefore + "\x1b[0m", "\x1b[38;5;238m" + promptAfter + "\x1b[0m"} {
		gwDig := panelDigest([]byte(s))
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk", "original-awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			pipe := strings.ReplaceAll(panelDigestPipe, "LC_ALL=C awk", "LC_ALL=C "+awk)
			cmd := exec.Command("sh", "-c", pipe)
			cmd.Stdin = strings.NewReader(s)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("ansi case %d (%s): %v", idx, awk, err)
			}
			if host, _, _ := strings.Cut(string(out), " "); host != gwDig {
				t.Errorf("ansi case %d (%s) panelDigestPipe mismatch:\nhost: %s\ngw:   %s", idx, awk, host, gwDig)
			}
		}
	}

	// 3. Equal-digest assertions (plain + ANSI).
	_, dBefore, _ := screenOf([]byte(noticeBefore))
	_, dAfter, _ := screenOf([]byte(noticeAfter))
	_, dPure, _ := screenOf([]byte(noticePure))
	if dBefore != dAfter || dBefore != dPure {
		t.Errorf("plan card overline notice/status/pure must share digest: before=%s after=%s pure=%s", dBefore, dAfter, dPure)
	}
	if panelDigest([]byte(ansiBefore)) != panelDigest([]byte(ansiAfter)) || panelDigest([]byte(ansiBefore)) != dAfter {
		t.Errorf("ANSI plan card overline notice/status must match plain digest: ansiBefore=%s ansiAfter=%s plain=%s",
			panelDigest([]byte(ansiBefore)), panelDigest([]byte(ansiAfter)), dAfter)
	}
	_, dp1, _ := screenOf([]byte(promptBefore))
	_, dp2, _ := screenOf([]byte(promptAfter))
	if dp1 != dp2 {
		t.Errorf("prompt overline notice/status must share digest: %s vs %s", dp1, dp2)
	}
	_, dt1, _ := screenOf([]byte(titleBefore))
	_, dt2, _ := screenOf([]byte(titleAfter))
	if dt1 != dt2 {
		t.Errorf("dialog-title overline notice/pure must share digest: %s vs %s", dt1, dt2)
	}

	// 4. Counterexamples MUST produce different digests.
	mustDiffer := []struct {
		name string
		a, b string
	}{
		{"plan body change", noticeAfter, planDiffBody},
		{"option text change", noticeAfter, planDiffOpt},
		{"cursor position change", noticeAfter, planDiffCursor},
		{"ANSI cursor position change", ansiAfter, strings.Replace(ansiAfter, "\x1b[38;5;75m❯\x1b[0m 1. Yes, and use auto mode\n     2. Yes, manually approve edits", "  1. Yes, and use auto mode\n   \x1b[38;5;75m❯\x1b[0m 2. Yes, manually approve edits", 1)},
		{"arbitrary prose ending with overline", proseOverline1, proseOverline2},
		{"indented plan line ending with overline", indentedPlanOverline1, indentedPlanOverline2},
		{"non-overline dash rule unchanged", dashNotice, dashRule},
	}
	for _, tc := range mustDiffer {
		if panelDigest([]byte(tc.a)) == panelDigest([]byte(tc.b)) {
			t.Errorf("%s: digests unexpectedly collided", tc.name)
		}
	}
}
