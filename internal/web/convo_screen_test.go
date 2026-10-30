package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cxRow(t *testing.T, typ string, payload map[string]any) string {
	b, err := json.Marshal(map[string]any{"timestamp": "2026-10-29T06:50:00.000Z", "type": typ, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func cxSaid(t *testing.T, id, text string) string {
	return cxRow(t, "response_item", map[string]any{"type": "message", "id": id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
}

func cxAsked(t *testing.T, id, text string) string {
	return cxRow(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": id, "content": []any{map[string]any{"type": "text", "text": text}}}})
}

const (
	cxA = "/h/.codex/sessions/2026/10/29/rollout-2026-10-29T14-45-49-01a18668-0000-7000-8000-00000000000a.jsonl"
	cxB = "/h/.codex/sessions/2026/10/29/rollout-2026-10-29T14-45-49-01a18668-0000-7000-8000-00000000000b.jsonl"
)

// What a Codex draws of an answer: its Markdown drawn (no backticks, bold
// marks, link brackets or code fences), wrapped and indented, under a
// bullet. The answer is still told by it.
func TestScreenPickDrawnMarkdown(t *testing.T) {
	said := "可以用于**本地 Codex CLI 或 IDE 扩展**。官方文档给出的 CLI 登录方式是将 Key 通过标准输入传给 `codex login --with-api-key`。API Key 模式按 API 用量计费，不使用 ChatGPT 套餐额度；**Codex 云端**则需要 ChatGPT 登录。([learn.chatgpt.com](https://learn.chatgpt.com/docs/auth))\n\n```sh\nprintf %s \"$KEY\" | codex login --with-api-key\n```"
	screen := "• 可以用于本地 Codex CLI 或 IDE 扩展。官方文档给出的 CLI 登录方式是将 Key 通过标准输入传给 codex login --with-api-\n  key。API Key 模式按 API 用量计费，不使用 ChatGPT 套餐额度；Codex 云端则需要 ChatGPT 登录。(learn.chatgpt.com\n  (https://learn.chatgpt.com/docs/auth))\n\n    printf %s \"$KEY\" | codex login --with-api-key\n\n  Worked for 1m 19s\n\n› Ask Codex to do anything\n"
	tails := map[string]string{
		cxA: cxAsked(t, "u1", "这个 API Key 是否可以用在 Codex") + cxSaid(t, "m1", said),
		cxB: cxAsked(t, "u2", "你好") + cxSaid(t, "m2", "你好！有什么我可以帮你的吗？今天想做点什么呢？"),
	}
	log, sid, on := screenPick(letters(screen), tails)
	if log != cxA || sid != convoSessionOf(cxA) || on < screenEnough2 {
		t.Errorf("picked %q %q, evidence %d", log, sid, on)
	}
}

// Words two conversations both said tell neither; one's own words, that
// one. Of two whose own words are there, the one lower on the screen.
func TestScreenPickOnlyItsOwn(t *testing.T) {
	same := "好的，我已经把这个问题修好了，测试全部通过，可以合并了。"
	tails := map[string]string{
		cxA: cxSaid(t, "a1", "甲这边先把数据库迁移脚本写好并在测试库上跑过一遍。") + cxSaid(t, "a2", same),
		cxB: cxSaid(t, "b1", "乙这边负责前端页面的改版和三种浏览器内核的截图核对。") + cxSaid(t, "b2", same),
	}
	if log, _, _ := screenPick(letters("• "+same+"\n› "), tails); log != "" {
		t.Errorf("told by words both said: %q", log)
	}
	if log, _, _ := screenPick(letters("• 甲这边先把数据库迁移脚本写好并在测试库上跑过一遍。\n• "+same), tails); log != cxA {
		t.Errorf("not told by its own words: %q", log)
	}
	if log, _, _ := screenPick(letters("• 甲这边先把数据库迁移脚本写好并在测试库上跑过一遍。\n• 乙这边负责前端页面的改版和三种浏览器内核的截图核对。"), tails); log != cxB {
		t.Errorf("the one lower on the screen not taken: %q", log)
	}
}

// What was taken back (rolled back) is no sign of the conversation.
func TestScreenPickNotWhatWasTakenBack(t *testing.T) {
	gone := "这一段回答后来被撤回了，屏幕上即使还留着它也不能算数。"
	tails := map[string]string{
		cxA: cxRow(t, "event_msg", map[string]any{"type": "task_started"}) + cxAsked(t, "u1", "先回答一段之后再撤回") + cxSaid(t, "m1", gone) +
			cxRow(t, "event_msg", map[string]any{"type": "task_complete"}) + cxRow(t, "event_msg", map[string]any{"type": "thread_rolled_back", "num_turns": 1}),
		cxB: cxSaid(t, "b1", "另一段毫不相干的对话内容在这里。"),
	}
	if log, _, _ := screenPick(letters("• "+gone), tails); log != "" {
		t.Errorf("told by what was taken back: %q", log)
	}
}

// Two Codexes in one place, a conversation begun once both were there: of
// neither by where and when — till the screen of one shows it. Then it is
// that one's, and the other's reading does not make it of neither again.
func TestDaemonLogKeepsWhatTheScreenShowed(t *testing.T) {
	srv := &Server{}
	const sec = 1000
	now := int64(1_700_000_000_000)
	sid := convoSessionOf(cxA)
	first := convoProc{pid: 7, start: 70, agent: "codex", cwd: "/w", started: now - 600*sec}
	second := convoProc{pid: 8, start: 80, agent: "codex", cwd: "/w", started: now - 300*sec}
	c := convoCand{log: cxA, cwd: "/w", began: now - 299*sec, wrote: now - 10*sec}
	both := convoLocated{procs: []convoProc{first, second}, cands: []convoCand{c}, hostNow: now}
	p := both.procs[1]
	if took, refused := srv.daemonLog(&p, both); took || !refused {
		t.Fatalf("taken by where and when: %v %v", took, refused)
	}
	alive := aliveCodex(both)
	if srv.convoClaimed.take(sid, second.id(), second.started, alive, now, false) {
		t.Fatal("taken, not shown")
	}
	if !srv.convoClaimed.take(sid, second.id(), second.started, alive, now, true) {
		t.Fatal("not taken when its screen showed it")
	}
	// (The first's reading: it could be either's, by where and when.)
	q := both.procs[0]
	srv.daemonLog(&q, both)
	if q.sid != "" {
		t.Errorf("the other took it: %q", q.sid)
	}
	p = both.procs[1]
	if took, _ := srv.daemonLog(&p, both); !took || p.sid != sid {
		t.Errorf("not kept by the one whose screen showed it: %v %q", took, p.sid)
	}
	// The other cannot take it by its screen either, while this one runs.
	if srv.convoClaimed.take(sid, first.id(), first.started, alive, now, true) {
		t.Error("taken from the one whose screen showed it")
	}
}

// The end of each log as the script prints it, its last line whole (with
// its newline: a record is read only once its line ends), a log's first
// line left out where it was read from its middle.
func TestConvoTailsCommandKeepsTheLastLine(t *testing.T) {
	dir := t.TempDir()
	short, long := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(short, []byte(cxSaid(t, "m1", "最后一条回答，就在文件的末尾。")), 0o600); err != nil {
		t.Fatal(err)
	}
	pad := cxSaid(t, "m0", strings.Repeat("长", convoTailBytes))
	if err := os.WriteFile(long, []byte(pad+cxSaid(t, "m2", "长记录末尾的回答。")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", convoTailsCommand([]string{short, long, filepath.Join(dir, "none.jsonl")})).Output()
	if err != nil {
		t.Fatal(err)
	}
	tails := splitTails(string(out), []string{short, long})
	if len(tails) != 2 || !strings.HasSuffix(tails[short], "\n") || !strings.HasPrefix(tails[long], "{") || strings.Contains(tails[long], "长长") {
		t.Fatalf("tails: %q", tails)
	}
	if log, _, _ := screenPick(letters("• 最后一条回答，就在文件的末尾。"), tails); log != short {
		t.Errorf("its last record not read: %q", log)
	}
	// (One that cannot be read is left out, not taken for an empty one.)
	locked := filepath.Join(dir, "c.jsonl")
	if err := os.WriteFile(locked, []byte(cxSaid(t, "m3", "读不到的记录。")), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		out, err = exec.Command("sh", "-c", convoTailsCommand([]string{short, locked})).Output()
		if err != nil {
			t.Fatal(err)
		}
		if tails := splitTails(string(out), []string{short, locked}); len(tails) != 1 || tails[short] == "" {
			t.Errorf("one not read: %q", tails)
		}
	}
}

// When the Codex started is not known, nothing is its by time alone.
func TestOnlyItsByTimeNeedsWhenItStarted(t *testing.T) {
	p := convoProc{pid: 8, start: 80, agent: "codex", cwd: "/w"}
	c := convoCand{log: cxA, cwd: "/w", began: 2000}
	if onlyItsByTime(&p, &c, convoLocated{procs: []convoProc{p}}) {
		t.Error("its by time, though when it started is not known")
	}
}

// The host's clock, as the scan says it: to the end of its second.
func TestParseHostNow(t *testing.T) {
	if l := parseConvoLocate("hostnow=1700000000\x00"); l.hostNow != 1_700_000_000_999 || l.now() != l.hostNow {
		t.Errorf("hostNow %d", l.hostNow)
	}
	if l := parseConvoLocate("hostnow=\x00"); l.hostNow != 0 || l.now() < time.Now().UnixMilli()-1000 {
		t.Errorf("none said: %d", l.hostNow)
	}
}

// When a Codex was last seen running never goes back (a scan read before
// another may come in after it), and is seen whichever Codex is read: one
// that ran beside it does not take its conversation once it has ended.
func TestConvoClaimsSeenRunning(t *testing.T) {
	var c convoClaims
	a, b := procID{7, 70}, procID{8, 80}
	if !c.take("s", a, 0, map[procID]bool{a: true}, 200, false) || !c.take("s", a, 0, map[procID]bool{a: true}, 100, false) {
		t.Fatal("not taken by the one it is of")
	}
	if c.take("s", b, 150, map[procID]bool{}, 300, true) {
		t.Error("taken by one started before it was last seen running")
	}
	srv := &Server{}
	sid := convoSessionOf(cxA)
	cand := convoCand{log: cxA, cwd: "/w", began: 1000, wrote: 2000}
	if !srv.convoClaimed.take(sid, a, 0, map[procID]bool{a: true}, 1000, false) {
		t.Fatal("not taken")
	}
	// (The other is read while the first runs: the first is seen then.)
	other := convoProc{pid: 8, start: 80, agent: "codex", cwd: "/w", started: 5000}
	srv.daemonLog(&other, convoLocated{procs: []convoProc{{pid: 7, start: 70, agent: "codex", cwd: "/w", started: 500}, other}, cands: []convoCand{cand}, hostNow: 9000})
	if srv.convoClaimed.take(sid, other.id(), other.started, map[procID]bool{}, 10000, true) {
		t.Error("taken by one that ran beside the one it is of")
	}
}

// One that could be either of two needs more of the screen, though the
// other has ended since.
func TestScreenEnoughOnceContested(t *testing.T) {
	srv := &Server{}
	sid := convoSessionOf(cxA)
	srv.convoClaimed.contest(sid)
	p := convoProc{pid: 8, start: 80, agent: "codex", cwd: "/w", started: 1000}
	c := convoCand{log: cxA, cwd: "/w", began: 2000}
	if n := srv.screenEnoughFor(&p, &c, sid, convoLocated{procs: []convoProc{p}}); n != screenEnough2 {
		t.Errorf("enough: %d", n)
	}
	if n := (&Server{}).screenEnoughFor(&p, &c, sid, convoLocated{procs: []convoProc{p}}); n != screenEnough {
		t.Errorf("enough, never contested: %d", n)
	}
}

// Evidence is the longest piece found, not pieces that overlap added up.
func TestScreenPickEvidenceNotAddedUp(t *testing.T) {
	tails := map[string]string{cxA: cxSaid(t, "a1", "abcdefghij") + cxSaid(t, "a2", "12345abcdefghij"), cxB: cxSaid(t, "b1", "完全不同的另一段对话内容")}
	if log, _, on := screenPick(letters("• 12345abcdefghij"), tails); log != cxA || on != 15 {
		t.Errorf("picked %q, evidence %d", log, on)
	}
}

// One that could once be either of two still needs more of the screen
// after a screen showed whose it is.
func TestScreenEnoughOnceDisputed(t *testing.T) {
	srv := &Server{}
	sid := convoSessionOf(cxA)
	p := convoProc{pid: 8, start: 80, agent: "codex", cwd: "/w", started: 1000}
	srv.convoClaimed.contest(sid)
	if !srv.convoClaimed.take(sid, p.id(), p.started, map[procID]bool{p.id(): true}, 2000, true) {
		t.Fatal("not taken when shown")
	}
	c := convoCand{log: cxA, cwd: "/w", began: 2000}
	if n := srv.screenEnoughFor(&p, &c, sid, convoLocated{procs: []convoProc{p}}); n != screenEnough2 {
		t.Errorf("enough: %d", n)
	}
}
