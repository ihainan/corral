package web

import (
	"encoding/json"
	"testing"
	"time"
)

// What was read of a Codex's title is used again only if read since keys
// last went to its pane (by when the read began), within its time, a
// failed read's backoff counted from its failing (review466: keys sent
// while a read that then failed was under way count).
func TestCodexTitleKept(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, c := range []struct {
		name                    string
		readAt, failAt, pressed time.Time
		fail, kept              bool
	}{
		{"read just now", ago(time.Second), time.Time{}, time.Time{}, false, true},
		{"read too long ago", ago(3 * time.Second), time.Time{}, time.Time{}, false, false},
		{"keys since", ago(time.Second), time.Time{}, ago(500 * time.Millisecond), false, false},
		{"failed, in its backoff", ago(8 * time.Second), ago(5 * time.Second), time.Time{}, true, true},
		{"failed, past its backoff", ago(15 * time.Second), ago(11 * time.Second), time.Time{}, true, false},
		{"failed after keys sent while it was read", ago(4 * time.Second), ago(time.Second), ago(2 * time.Second), true, false},
		{"failed, keys before the read", ago(4 * time.Second), ago(time.Second), ago(6 * time.Second), true, true},
	} {
		st := &convoState{cxTitleAt: c.readAt, cxTitleFailAt: c.failAt, cxTitleFor: "p", cxTitleFail: c.fail, cxTitleWait: true}
		if _, ok := st.codexTitleKept("p", c.pressed, now); ok != c.kept {
			t.Errorf("%s: kept %v", c.name, ok)
		}
		if _, ok := st.codexTitleKept("another", c.pressed, now); ok {
			t.Errorf("%s: kept for another process", c.name)
		}
	}
}

// A Codex log begun by the page's first message: its first message of the
// person's is that one (review468: not one resumed or begun anew in its
// terminal, whose first message is another).
func TestCodexFirstIs(t *testing.T) {
	row := func(typ string, p map[string]any) string {
		b, _ := json.Marshal(map[string]any{"timestamp": "2026-10-31T01:00:00.000Z", "type": typ, "payload": p})
		return string(b) + "\n"
	}
	user := func(text string) string {
		return row("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "u-" + text, "content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	head := row("session_meta", map[string]any{"id": "s"}) +
		row("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<environment_context>x</environment_context>"}}}) +
		user("修好登录\n然后跑测试") + user("插一句")
	for _, c := range []struct {
		name, head, first string
		want              bool
	}{
		{"its first is ours", head, "修好登录\n然后跑测试", true},
		{"ours, spaces about it", head, "  修好登录\n然后跑测试\n", true},
		{"another first (resumed, begun anew)", head, "插一句", false},
		{"none of the person's yet", row("session_meta", map[string]any{"id": "s"}), "修好登录", false},
		{"a line cut short only", `{"timestamp":"x","type":"event_msg","payload":{"type":"item_completed"`, "修好登录", false},
	} {
		if got := codexFirstIs([]byte(c.head), c.first); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
