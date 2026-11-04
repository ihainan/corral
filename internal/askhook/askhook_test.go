package askhook

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const ours = `{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"true # corral: AskUserQuestion to the log at once"}]}`

func TestRead(t *testing.T) {
	cases := []struct {
		name, settings, managed string
		want                    State
	}{
		{"no file", "", "", Off},
		{"empty object", "{}", "", Off},
		{"another event's hook", `{"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"x"}]}]}}`, "", Off},
		{"another tool's", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x"}]}]}}`, "", Off},
		{"ours", `{"hooks":{"PreToolUse":[` + ours + `]}}`, "", On},
		{"ours beside the person's", `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"x"}]},` + ours + `]}}`, "", On},
		{"theirs: no matcher", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"x"}]}]}}`, "", OnOwn},
		{"theirs: empty", `{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"command","command":"x"}]}]}}`, "", OnOwn},
		{"theirs: *", `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"x"}]}]}}`, "", OnOwn},
		{"theirs: the name", `{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"x"}]}]}}`, "", OnOwn},
		{"theirs: a | list", `{"hooks":{"PreToolUse":[{"matcher":"Bash|AskUserQuestion","hooks":[{"type":"command","command":"x"}]}]}}`, "", OnOwn},
		{"theirs: a , list with spaces", `{"hooks":{"PreToolUse":[{"matcher":"Bash, AskUserQuestion","hooks":[{"type":"command","command":"x"}]}]}}`, "", OnOwn},
		// (Claude Code takes a plain name exactly: "Ask" does not match; a
		// regular expression is not modelled, so it does not count.)
		{"not theirs: Ask", `{"hooks":{"PreToolUse":[{"matcher":"Ask","hooks":[{"type":"command","command":"x"}]}]}}`, "", Off},
		{"not counted: Ask.*", `{"hooks":{"PreToolUse":[{"matcher":"Ask.*","hooks":[{"type":"command","command":"x"}]}]}}`, "", Off},
		{"not counted: (?i)", `{"hooks":{"PreToolUse":[{"matcher":"(?i)askuserquestion","hooks":[{"type":"command","command":"x"}]}]}}`, "", Off},
		{"not theirs: no command", `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"prompt","prompt":"x"}]}]}}`, "", Off},
		{"blocked: disableAllHooks", `{"disableAllHooks":true,"hooks":{"PreToolUse":[` + ours + `]}}`, "", Blocked},
		{"blocked: managed only", `{}`, `{"allowManagedHooksOnly":true}`, Blocked},
		{"blocked: managed disables", `{}`, `{"disableAllHooks":true}`, Blocked},
		{"a managed file not read: ignored", `{}`, `{oops`, Off},
	}
	for _, c := range cases {
		var m []byte
		if c.managed != "" {
			m = []byte(c.managed)
		}
		got, why, err := Read([]byte(c.settings), m)
		if err != nil || got != c.want || (got == Blocked) != (why != "") {
			t.Errorf("%s: %q %q %v, want %q", c.name, got, why, err, c.want)
		}
	}
	for _, bad := range []string{`[]`, `{"a":1} x`, "\xef\xbb\xbf{}", `{"hooks":[]}`, `{"hooks":{"PreToolUse":{}}}`, `{"a": // c` + "\n}"} {
		if _, _, err := Read([]byte(bad), nil); err == nil {
			t.Errorf("%q read without an error", bad)
		}
	}
}

func TestEnableDisable(t *testing.T) {
	cases := []struct{ name, before, after string }{
		{"no file", "", "{\n  \"hooks\": {\n    \"PreToolUse\": [\n      " + indent(ours, 6) + "\n    ]\n  }\n}\n"},
		{"keys kept in their order, other hooks kept", `{"model":"opus","hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"a 2>&1 >/dev/null"}]}]},"tui":"fullscreen"}`, ""},
		{"another PreToolUse kept, ours after it", `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"b"}]}]}}`, ""},
		{"no newline at the end kept so", `{"a":1}`, ""},
	}
	for _, c := range cases {
		on, changed, err := Enable([]byte(c.before))
		if err != nil || !changed {
			t.Fatalf("%s: %v %v", c.name, changed, err)
		}
		if c.after != "" && string(on) != c.after {
			t.Errorf("%s: got\n%s\nwant\n%s", c.name, on, c.after)
		}
		if st, _, _ := Read(on, nil); st != On {
			t.Errorf("%s: read back %q", c.name, st)
		}
		if strings.Contains(string(on), "\\u003e") {
			t.Errorf("%s: escaped: %s", c.name, on)
		}
		if c.before != "" && strings.HasSuffix(c.before, "\n") != strings.HasSuffix(string(on), "\n") {
			t.Errorf("%s: its end changed: %q", c.name, on)
		}
		again, changed, _ := Enable(on)
		if changed || string(again) != string(on) {
			t.Errorf("%s: put in twice", c.name)
		}
		off, empty, changed, err := Disable(on)
		if err != nil || !changed {
			t.Fatalf("%s: off: %v %v", c.name, changed, err)
		}
		if same(off, []byte(c.before)) != nil {
			t.Errorf("%s: off is not as before: %v\n%s", c.name, same(off, []byte(c.before)), off)
		}
		if empty != (c.before == "" || c.before == "{}") {
			t.Errorf("%s: empty %v", c.name, empty)
		}
		if _, _, changed, _ := Disable(off); changed {
			t.Errorf("%s: taken out twice", c.name)
		}
	}
	// The order of keys: as they were.
	on, _, _ := Enable([]byte(`{"model":"opus","hooks":{"SessionStart":[]},"tui":"fullscreen"}`))
	if i, j, k := strings.Index(string(on), "model"), strings.Index(string(on), "SessionStart"), strings.Index(string(on), "tui"); !(i < j && j < k) {
		t.Errorf("order: %s", on)
	}
}

// Taking ours out leaves another hook the person put in the same entry.
func TestDisableLeavesTheirs(t *testing.T) {
	in := `{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"true # corral: AskUserQuestion to the log at once"},{"type":"command","command":"mine"}]}]}}`
	off, empty, changed, err := Disable([]byte(in))
	if err != nil || !changed || empty {
		t.Fatal(changed, empty, err)
	}
	want := `{"hooks":{"PreToolUse":[{"matcher":"AskUserQuestion","hooks":[{"type":"command","command":"mine"}]}]}}`
	if err := same(off, []byte(want)); err != nil {
		t.Errorf("%v\n%s", err, off)
	}
	if st, _, _ := Read(off, nil); st != OnOwn {
		t.Errorf("left: %q", st)
	}
	// An entry of the person's with the same command but another matcher: not ours.
	other := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"true # corral: AskUserQuestion to the log at once"}]}]}}`
	if _, _, changed, _ := Disable([]byte(other)); changed {
		t.Error("took one with another matcher")
	}
}

func TestEnableRefusesOddShapes(t *testing.T) {
	for _, bad := range []string{`[]`, `{"hooks":[]}`, `{"hooks":{"PreToolUse":"x"}}`, "\xef\xbb\xbf{}"} {
		if _, _, err := Enable([]byte(bad)); err == nil {
			t.Errorf("%q changed", bad)
		}
		if _, _, _, err := Disable([]byte(bad)); err == nil {
			t.Errorf("%q changed (off)", bad)
		}
	}
}

func TestInline(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(Inline()), &v); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := Read([]byte(Inline()), nil); st != On {
		t.Errorf("inline reads as %q", st)
	}
}

// same: the two say the same as JSON (nothing, or blank, as {}).
func same(a, b []byte) error {
	norm := func(x []byte) (any, error) {
		if strings.TrimSpace(string(x)) == "" {
			x = []byte("{}")
		}
		var v any
		err := json.Unmarshal(x, &v)
		return v, err
	}
	va, err := norm(a)
	if err != nil {
		return err
	}
	vb, err := norm(b)
	if err != nil {
		return err
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	if string(ja) != string(jb) {
		return &mismatch{string(ja), string(jb)}
	}
	return nil
}

type mismatch struct{ a, b string }

func (m *mismatch) Error() string { return m.a + " != " + m.b }

func indent(s string, n int) string {
	var b bytes.Buffer
	_ = json.Indent(&b, []byte(s), strings.Repeat(" ", n), "  ")
	return b.String()
}

// TestEmitForLive writes, for the live check (hooklive4.py), the hook inline
// to ASKHOOK_INLINE and the file at ASKHOOK_EMIT as Enable makes it, to
// ASKHOOK_EMIT.enabled. Skipped without them.
func TestEmitForLive(t *testing.T) {
	f, i := os.Getenv("ASKHOOK_EMIT"), os.Getenv("ASKHOOK_INLINE")
	if f == "" || i == "" {
		t.Skip("ASKHOOK_EMIT and ASKHOOK_INLINE not set")
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	on, _, err := Enable(b)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(f+".enabled", on, 0o600) != nil || os.WriteFile(i, []byte(Inline()), 0o600) != nil {
		t.Fatal("not written")
	}
}

// review534-5/7: spaces only around a name; hooks turned off: ours not put in twice.
func TestReview534(t *testing.T) {
	for m, want := range map[string]State{" ": Off, " * ": Off, "Bash,\tAskUserQuestion": Off, "Bash , AskUserQuestion": OnOwn, "Bash|AskUserQuestion ": OnOwn} {
		in := `{"hooks":{"PreToolUse":[{"matcher":` + strings.ReplaceAll(strings.ReplaceAll(`"`+m+`"`, "\t", `\t`), "\n", `\n`) + `,"hooks":[{"type":"command","command":"x"}]}]}}`
		if got, _, err := Read([]byte(in), nil); got != want || err != nil {
			t.Errorf("%q: %q %v, want %q", m, got, err, want)
		}
	}
	on := `{"disableAllHooks":true,"hooks":{"PreToolUse":[` + ours + `]}}`
	if _, changed, _ := Enable([]byte(on)); changed {
		t.Error("put in twice where hooks are off")
	}
	if c := Created(); !strings.Contains(string(c), "AskUserQuestion") || !strings.HasSuffix(string(c), "\n") {
		t.Errorf("%s", c)
	}
}

// The bell hook: a Stop entry of no matcher, kept apart from the question
// hook (each put in and taken out alone); its command, run by sh as Claude
// Code runs it, says the bell for Claude Code to write — outside herdr only.
func TestBell(t *testing.T) {
	both, changed, err := Bell.Enable(Created())
	if err != nil || !changed {
		t.Fatal(err)
	}
	if st, _, _ := Bell.Read(both, nil); st != On {
		t.Fatalf("bell %s", st)
	}
	if st, _, _ := Read(both, nil); st != On {
		t.Fatalf("ask %s", st)
	}
	if st, _, _ := Bell.Read(Created(), nil); st != Off {
		t.Fatalf("bell in the ask's file: %s", st)
	}
	// (A Stop hook of the person's own is not the bell: not taken as on, kept.)
	own := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"make test"}]}]}}`)
	if st, _, _ := Bell.Read(own, nil); st != Off {
		t.Fatalf("own stop hook: %s", st)
	}
	withOwn, _, _ := Bell.Enable(own)
	back, empty, changed, err := Bell.Disable(withOwn)
	if err != nil || !changed || empty || !strings.Contains(string(back), "make test") || strings.Contains(string(back), "corral") {
		t.Fatalf("off: %s %v %v %v", back, empty, changed, err)
	}
	askOnly, _, changed, _ := Bell.Disable(both)
	if !changed || !bytes.Equal(askOnly, Created()) {
		t.Fatalf("the bell off, the ask's left: %s", askOnly)
	}
	_, empty, _, _ = Bell.Disable(Bell.Created())
	if !empty {
		t.Fatal("the bell's own file: empty once off")
	}
	if st, why, _ := Bell.Read(Bell.Created(), []byte(`{"disableAllHooks":true}`)); st != Blocked || why != "disableAllHooks" {
		t.Fatalf("blocked: %s %s", st, why)
	}
	for _, c := range []struct {
		env  []string
		want string
	}{{nil, `{"terminalSequence":"\u0007"}`}, {[]string{"HERDR_PANE_ID=w1:p1"}, ``}} {
		cmd := exec.Command("sh", "-c", BellCommand)
		cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop"}`)
		cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, c.env...)
		out, err := cmd.Output()
		if err != nil || string(out) != c.want {
			t.Fatalf("env %v: %q %v", c.env, out, err)
		}
		if c.want != "" {
			var v struct {
				TerminalSequence string `json:"terminalSequence"`
			}
			if json.Unmarshal(out, &v) != nil || v.TerminalSequence != "\a" {
				t.Fatalf("not a bell: %q", out)
			}
		}
	}
}
