package web

import (
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/sshx"
)

// What a waiting agent asks, as alerts show it, and when its buttons may
// answer: only when all its box shows fits in the alert.
func TestWaitQuestion(t *testing.T) {
	box := func(inner ...string) string {
		out := []string{"⏺ Bash(rm -rf build)", "╭──────────────────────────────────────╮"}
		for _, l := range inner {
			out = append(out, "│ "+l+" │")
		}
		return strings.Join(append(out, "╰──────────────────────────────────────╯", "  Esc to cancel"), "\n")
	}
	w := waitQuestion(box("Bash command", "  rm -rf build", "", "Do you want to proceed?",
		"❯ 1. Yes", "  2. Yes, and don't ask again for rm", "  3. No, and tell Claude what to do"))
	if w.question != "Bash command rm -rf build Do you want to proceed?" || !w.complete {
		t.Fatalf("question %q complete %v", w.question, w.complete)
	}
	if len(w.menu) != 3 || w.menu[0] != (waitChoice{"1", "Yes"}) || w.menu[2] != (waitChoice{"3", "No, and tell Claude what to do"}) {
		t.Fatalf("menu %+v", w.menu)
	}

	// A command too long to show whole: shown cut, and no buttons.
	w = waitQuestion(box("Bash command", "  "+strings.Repeat("x", 400), "Do you want to proceed?", "❯ 1. Yes", "  2. No"))
	if w.complete || len(w.menu) != 2 || !strings.HasSuffix(w.question, "…") {
		t.Fatalf("long: complete %v menu %d %q", w.complete, len(w.menu), w.question)
	}
	// No box or rule above the menu (its context cannot be told whole): no buttons.
	w = waitQuestion("rm -rf /important\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No")
	if w.complete || len(w.menu) != 2 {
		t.Fatalf("no box: %+v", w)
	}
	// A rule as the top (Claude Code without a box).
	w = waitQuestion("──────────\n Edit file\n main.go\n Do you want to make this edit?\n ❯ 1. Yes\n   2. No")
	if !w.complete || w.question != "Edit file main.go Do you want to make this edit?" {
		t.Fatalf("rule: %+v", w)
	}

	// No menu (a numbered list without the mark is not one): the last lines.
	w = waitQuestion("Some output\n\nWhich file should I edit?\n1. a.go\n2. b.go")
	if w.menu != nil || w.complete || w.question != "Which file should I edit? 1. a.go 2. b.go" {
		t.Fatalf("plain: %+v", w)
	}
	if w := waitQuestion(""); w.question != "" || w.menu != nil {
		t.Fatalf("empty: %+v", w)
	}
}

// sent is a send that went (true) or was held back.
func sent(ok bool) func() bool { return func() bool { return ok } }

// An alert is only kept for the wait under way; its token answers once, for
// its user, its keys, and while that wait lasts.
func TestWaitAlertsTokens(t *testing.T) {
	var a waitAlerts
	w1 := a.begin("p")
	a.end("p")
	if a.send(&waitAlert{tag: "p", wait: w1}, sent(true)) {
		t.Fatal("an alert was sent for a wait that ended")
	}
	w2 := a.begin("p")
	if a.send(&waitAlert{tag: "p", wait: w2}, sent(false)) {
		t.Fatal("an alert held back counted as sent")
	}
	if a.end("p") != nil {
		t.Fatal("an alert held back is to be replaced")
	}
	w3 := a.begin("p")
	al := &waitAlert{tag: "p", wait: w3, user: 7, token: "tok", keys: map[string]bool{"1": true, "3": true}}
	if !a.send(al, sent(true)) {
		t.Fatal("not sent")
	}
	if a.use("tok", 8, "1") != nil || a.use("tok", 7, "2") != nil {
		t.Fatal("answered for another user, or with another key")
	}
	if a.use("tok", 7, "1") != al || a.use("tok", 7, "1") != nil {
		t.Fatal("the token does not answer exactly once")
	}
	// A new wait of the same pane: the old alert's token no longer answers.
	w4 := a.begin("p")
	al4 := &waitAlert{tag: "p", wait: w4, user: 7, token: "t4", keys: map[string]bool{"1": true}}
	a.send(al4, sent(true))
	a.begin("p") // (another wait began meanwhile)
	if a.use("t4", 7, "1") != nil {
		t.Fatal("an alert of an earlier wait answered")
	}
	if !a.current("p", a.waiting["p"]) || a.current("p", w4) {
		t.Fatal("current")
	}
	// A session that ends takes its alerts with it.
	a2 := waitAlerts{}
	sid := "0123456789abcdef"
	tag := waitTagPrefix(sid) + "w1-p1"
	wa := a2.begin(tag)
	a2.send(&waitAlert{tag: tag, wait: wa, session: sid, token: "x", keys: map[string]bool{"1": true}}, sent(true))
	if got := a2.endSession(sid); len(got) != 1 || a2.use("x", 0, "1") != nil || a2.current(tag, wa) {
		t.Fatalf("endSession: %d alerts", len(got))
	}
}

// A "needs you" alert's button presses its choice once, for its user, with
// one of its keys, while its wait lasts and the screen is the alert's.
func TestAlertAnswer(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	hd.setScreen("Do you want to proceed?\n❯ 1. Yes\n  2. No")
	base, id := herdrReplyEnv(t, h, admin, hd)
	digest, _ := admin.do("GET", base+"screen", nil).json()["digest"].(string)
	me := int64(admin.do("GET", "/api/me", nil).json()["id"].(float64))
	tag := waitTagPrefix(id) + "w1-p1"
	alert := func(token string) {
		w := h.srv.waitAlerts.begin(tag)
		if !h.srv.waitAlerts.send(&waitAlert{tag: tag, wait: w, user: me, session: id, pane: "w1:p1", digest: digest, token: token, keys: map[string]bool{"1": true, "2": true}}, sent(true)) {
			t.Fatal("alert not kept")
		}
	}
	answer := func(c *client, token, key string) resp {
		return c.do("POST", "/api/alerts/answer", map[string]any{"token": token, "key": key})
	}

	alert("t1")
	bob := h.member(admin, "bob")
	mustStatus(t, answer(bob, "t1", "1"), 410, "another user's token")
	mustStatus(t, answer(admin, "t1", "3"), 410, "a key not on the alert")
	mustStatus(t, answer(admin, "nope", "1"), 410, "unknown token")
	mustStatus(t, answer(admin, "t1", "1"), 200, "answer")
	if s := hd.typed(); len(s) != 1 || s[0]["keys"].([]any)[0] != "1" {
		t.Fatalf("typed: %v", s)
	}
	mustStatus(t, answer(admin, "t1", "1"), 410, "the token a second time")

	// Another wait of the pane began: the earlier alert no longer answers.
	alert("t2")
	h.srv.waitAlerts.begin(tag)
	mustStatus(t, answer(admin, "t2", "2"), 410, "an earlier wait's alert")

	// The token taken, the wait ends and an identical one begins before the
	// key is typed: nothing typed (checked right before typing).
	hd.setScreen("Proceed again?\n❯ 1. Yes\n  2. No") // (a screen not answered moments ago)
	digest, _ = admin.do("GET", base+"screen", nil).json()["digest"].(string)
	alert("t5")
	alertAnswerHook = func() { h.srv.waitAlerts.begin(tag) }
	r := answer(admin, "t5", "1")
	alertAnswerHook = nil
	mustStatus(t, r, 410, "the wait changed while answering")
	if len(hd.typed()) != 1 {
		t.Fatal("typed into a later wait")
	}

	// The screen changed since the alert: nothing typed.
	alert("t3")
	hd.setScreen("Working…")
	mustStatus(t, answer(admin, "t3", "2"), 409, "changed screen")
	if len(hd.typed()) != 1 {
		t.Fatal("typed into a changed screen")
	}
}

// The composer's listing: the folder, files that can be typed after "@", and
// the agents' commands and skills with names that are names.
func TestParseComplete(t *testing.T) {
	rec := func(r ...string) string { return strings.Join(r, "\x00") + "\x00" }
	out := rec("/home/a/proj", "claude /deploy", "codex /prompts:fix", "codex $lint", "opencode /ship", "claude /bad name", "other /x", "claude /$(rm)",
		"claude /evil\n-- corral files --\nphantom", completeMarker,
		"src/main.go", "./my notes.md", `a"b\c.md`, "/etc/passwd", "../up", "bad\x1bname", completeMarker, strings.Repeat("d/", 40)+"x") + "cut-sh"
	l := parseComplete(out)
	if l.Cwd != "/home/a/proj" || strings.Join(l.Files, "|") != `src/main.go|my notes.md|a"b\c.md|`+completeMarker || !l.More {
		t.Fatalf("files %q more %v", l.Files, l.More)
	}
	if len(l.Commands) != 4 || l.Commands[0] != (completeCmd{"claude", "/deploy"}) || l.Commands[2] != (completeCmd{"codex", "$lint"}) || l.Commands[3] != (completeCmd{"opencode", "/ship"}) {
		t.Fatalf("commands %+v", l.Commands)
	}
	if l := parseComplete(rec("/x", completeMarker, "a")); l.More || len(l.Files) != 1 {
		t.Fatalf("whole listing: %+v", l)
	}
	if l := parseComplete(""); l.Cwd != "" || len(l.Files) != 0 {
		t.Fatalf("empty: %+v", l)
	}
	if dir := "/x'; rm -rf ~; '"; !strings.HasSuffix(completeCommand(dir), " corral "+sshx.QuoteShell(dir)) {
		t.Fatal("the folder is not quoted as one argument")
	}
}

// A replacement is not sent once the agent waits again (that wait's alert
// replaces the old one).
func TestWaitReplacementSkippedWhenWaitingAgain(t *testing.T) {
	var a waitAlerts
	a.send(&waitAlert{tag: "p", wait: a.begin("p")}, sent(true))
	a.end("p")
	n := 0
	a.sendReplacement("p", func() bool { n++; return true })
	if n != 1 {
		t.Fatal("no replacement after the wait ended")
	}
	a.send(&waitAlert{tag: "p", wait: a.begin("p")}, sent(true))
	a.sendReplacement("p", func() bool { n++; return true })
	if n != 1 {
		t.Fatal("a replacement hid an agent that waits again")
	}
}
