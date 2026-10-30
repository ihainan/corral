package web

import (
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
