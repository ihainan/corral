package web

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A row of a subagent's log as Claude Code 2.1.314 writes a message the
// person typed with that subagent's conversation in front (the real one's
// shape, its words made up).
func wentRow(t *testing.T, content any, kind string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"parentUuid": "43b849cb-0a94-4c38-9c68-a593d349eb50", "isSidechain": true, "agentId": "a2ee6a653f4cbc097",
		"type": "user", "message": map[string]any{"role": "user", "content": content}, "isMeta": true,
		"uuid": "41ac932b-8e20-44ca-b4e2-61941fbf3bf0", "timestamp": "2026-11-02T12:44:39.266Z", "origin": map[string]any{"kind": kind},
		"userType": "external", "sessionId": "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const wentTail = "\n\nThis is how Claude Code surfaces messages the user sends mid-turn — within the running turn, often alongside the next tool result, rather than as a separate conversation turn. Address the message above as you continue this turn."

func TestWentFragment(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"部署", "部署"},
		{"ok", ""}, // (too short to look for)
		{"short\nthe longest line of it\nend", "the longest line of it"},
		{`say "hi" to C:\temp now`, "temp now"},
		{strings.Repeat("长", 40), strings.Repeat("长", 32)}, // (96 bytes, at a character's end)
	} {
		if got := wentFragment(c.in); got != c.want {
			t.Errorf("wentFragment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The same words said to it many times before: the latest row still seen.
func TestWentLatest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1", "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString(strings.Replace(wentRow(t, wentMidTurn+"go on"+wentTail, "human"), "2026-11-02T12:44:39.266Z", "2026-11-01T10:00:00.000Z", 1) + "\n")
	}
	b.WriteString(strings.Replace(wentRow(t, wentMidTurn+"go on"+wentTail, "human"), "2026-11-02T12:44:39.266Z", "2026-11-02T13:00:00.000Z", 1) + "\n")
	if err := os.WriteFile(filepath.Join(dir, "agent-a2ee6a653f4cbc097.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", wentCommand(dir, 60, wentFragment("go on"))).Output()
	if err != nil {
		t.Fatal(err)
	}
	if w := wentOf(out, "go on", time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)); w == nil {
		t.Fatalf("the latest row not seen among %d bytes", len(out))
	}
}

// The same words taken in by two subagents since: which, not known; logs
// left unread: not known either.
func TestWentUnsure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1", "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	row := wentRow(t, wentMidTurn+"ok"+wentTail, "human") + "\n"
	for _, id := range []string{"a2ee6a653f4cbc097", "b1111111111111111"} {
		if err := os.WriteFile(filepath.Join(dir, "agent-"+id+".jsonl"), []byte(row), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("sh", "-c", wentCommand(dir, 60, wentFragment("ok"))).Output()
	if err != nil {
		t.Fatal(err)
	}
	if w := wentOf(out, "ok", time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)); w != nil {
		t.Errorf("two subagents' rows: one said for sure: %+v", w)
	}
	for i := 0; i < wentMaxFiles+1; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("agent-c%016d.jsonl", i)), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err = exec.Command("sh", "-c", wentCommand(dir, 60, wentFragment("ok"))).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\x01more\n") {
		t.Errorf("more logs than are read: not said")
	}
}

// More of the person's rows with its words in one log than are read: not known.
func TestWentCapped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1", "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a2ee6a653f4cbc097.jsonl"), []byte(strings.Repeat(wentRow(t, wentMidTurn+"ok"+wentTail, "human")+"\n", 51)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", wentCommand(dir, 60, wentFragment("ok"))).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\x01more\n") {
		t.Errorf("more rows than are read: not said")
	}
}

// A message too short to look for by its words: the person's rows all read.
func TestWentShort(t *testing.T) {
	home := filepath.Join(t.TempDir(), "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1")
	dir := filepath.Join(home, "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a2ee6a653f4cbc097.jsonl"), []byte(wentRow(t, "go on", "human")+"\n"+wentRow(t, wentMidTurn+"ok"+wentTail, "human")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", wentCommand(dir, 60, wentFragment("ok"))).Output()
	if err != nil {
		t.Fatal(err)
	}
	if w := wentOf(out, "ok", time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)); w == nil || w.Agent != "a2ee6a653f4cbc097" {
		t.Fatalf("went %+v, out %s", w, out)
	}
}

func TestPersonSaid(t *testing.T) {
	text := "Verifying picker backdrop 是在做什么？\n第二行 \"引号\""
	for _, c := range []struct {
		name string
		row  string
		said string
		ok   bool
	}{
		{"mid-turn", wentRow(t, wentMidTurn+text+wentTail, "human"), text, true},
		{"as is", wentRow(t, text, "human"), text, true},
		{"text parts", wentRow(t, []map[string]string{{"type": "text", "text": text}}, "human"), text, true},
		{"not the person's", wentRow(t, wentMidTurn+text+wentTail, "coordinator"), "", false},
		{"the wrapping's start alone: their own words", wentRow(t, wentMidTurn+text, "human"), wentMidTurn + text, true},
		{"wrapped, but not by Claude Code (not isMeta): their own words", strings.Replace(wentRow(t, wentMidTurn+text+wentTail, "human"), `"isMeta":true`, `"isMeta":false`, 1), wentMidTurn + text + wentTail, true},
		{"not a row", "{", "", false},
	} {
		said, _, ok := personSaid([]byte(c.row))
		if ok != c.ok || said != c.said {
			t.Errorf("%s: %q %v, want %q %v", c.name, said, ok, c.said, c.ok)
		}
	}
}

// The host's search run here, over a folder of logs as a session's is.
func TestWentSearch(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	text := "又出现了这种问题，你和 GPT 一起找出根本原因：\n/home/x/shot.png"
	home := filepath.Join(t.TempDir(), "8251e81c-b1f1-4d4a-8f3f-bb630ba7bcf1")
	dir := filepath.Join(home, "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The subagent that took it; another that only quotes it (a tool's
	// output, the coordinator's word); a third, a near miss.
	write("agent-a2ee6a653f4cbc097.jsonl", wentRow(t, "first", "human")+"\n"+wentRow(t, wentMidTurn+text+wentTail, "human")+"\n")
	write("agent-a2ee6a653f4cbc097.meta.json", `{"agentType":"general-purpose","description":"Implement UX fixes: agents page","finished":true}`)
	write("agent-b1111111111111111.jsonl", wentRow(t, "The coordinator said: "+text, "coordinator")+"\n"+`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"`+strings.ReplaceAll(text, "\n", `\n`)+`"}]}}`+"\n")
	write("agent-c2222222222222222.jsonl", wentRow(t, wentMidTurn+text+" 还有一句"+wentTail, "human")+"\n")
	if err := os.Symlink(filepath.Join(dir, "agent-a2ee6a653f4cbc097.jsonl"), filepath.Join(dir, "agent-d3333333333333333.jsonl")); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, minutes int, text string) []byte {
		t.Helper()
		out, err := exec.Command("sh", "-c", wentCommand(dir, minutes, wentFragment(text))).Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run(dir, 60, text)
	if strings.Contains(string(out), "agent-d333") {
		t.Errorf("a link was read: %s", out)
	}
	before := time.Date(2026, 11, 2, 12, 40, 0, 0, time.UTC) // (sent then: its row at 12:44:39)
	got := wentOf(out, text, before)
	if got == nil || got.Agent != "a2ee6a653f4cbc097" || got.Type != "general-purpose" || got.Description != "Implement UX fixes: agents page" {
		t.Fatalf("went %+v, out:\n%s", got, out)
	}
	// Words of it alone, or more than it: not that message.
	if w := wentOf(out, "又出现了这种问题", before); w != nil {
		t.Errorf("a part of it taken for it: %+v", w)
	}
	// (Blanks at its ends, as the page has it: Claude Code keeps it without.)
	if w := wentOf(run(dir, 60, "  "+text+"\n"), "  "+text+"\n", before); w == nil {
		t.Errorf("not found with blanks at its ends")
	}
	// The same words said to it before this one was sent: not this one.
	if w := wentOf(out, text, before.Add(time.Hour)); w != nil {
		t.Errorf("an earlier row of the same words taken for it: %+v", w)
	}
	// None of its logs changed since: not looked in.
	old := filepath.Join(dir, "agent-a2ee6a653f4cbc097.jsonl")
	if err := exec.Command("touch", "-d", "3 hours ago", old).Run(); err != nil {
		t.Fatal(err)
	}
	if w := wentOf(run(dir, 60, text), text, before); w != nil {
		t.Errorf("an older log looked in: %+v", w)
	}
	// The subagents' folder a link: nothing read.
	link := filepath.Join(t.TempDir(), "subagents")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if out := run(link, 600, text); len(out) != 0 {
		t.Errorf("read through a link: %s", out)
	}
}
