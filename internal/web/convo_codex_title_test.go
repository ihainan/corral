package web

import (
	"strings"
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

// A Codex log's head says it is a subagent of the session asked about, or
// it is not given (item 1).
func TestCodexSubagentOf(t *testing.T) {
	const main = "01a18ff7-4138-7890-9ecd-1aee1502e132"
	head := func(source, parent string) []byte {
		return []byte(`{"timestamp":"x","type":"session_meta","payload":{"id":"01a18ff7-432f-7a80-bb03-7c6c92a8fd35","thread_source":"` + source + `","parent_thread_id":"` + parent + `"}}` + "\n{}\n")
	}
	for _, c := range []struct {
		name string
		head []byte
		want bool
	}{
		{"its subagent", head("subagent", main), true},
		{"another session's subagent", head("subagent", "01a18ff7-0000-7890-9ecd-1aee1502e132"), false},
		{"not a subagent", head("user", main), false},
		{"no parent said", head("subagent", ""), false},
		{"not a log's head", []byte("garbage\n"), false},
		// (review495: a line cut off says nothing; its keys in any order; a
		// key given twice, its last; what follows the first line not read.)
		{"its line cut off", []byte(`{"timestamp":"x","type":"session_meta","payload":{"thread_source":"subagent","parent_thread_id":"` + main + `","base_instructions":{"text":"` + strings.Repeat("x", 9000))[:8192], false},
		{"its payload before its type", []byte(`{"payload":{"parent_thread_id":"` + main + `","thread_source":"subagent"},"type":"session_meta"}` + "\n"), true},
		{"its long instructions first", []byte(`{"type":"session_meta","payload":{"base_instructions":"` + strings.Repeat("x", 20000) + `","thread_source":"subagent","parent_thread_id":"` + main + `"}}` + "\n"), true},
		{"its parent given twice, another's last", []byte(`{"type":"session_meta","payload":{"thread_source":"subagent","parent_thread_id":"` + main + `","parent_thread_id":"01a18ff7-0000-7890-9ecd-1aee1502e132"}}` + "\n"), false},
		{"not JSON after what it says", []byte(`{"type":"session_meta","payload":{"thread_source":"subagent","parent_thread_id":"` + main + `"},` + "\n"), false},
	} {
		if got := codexSubagentOf(c.head, main); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if codexSubagentOf(head("subagent", ""), "") {
		t.Error("no session: none")
	}
}
