package term

import "testing"

func TestAlertScanner(t *testing.T) {
	var got []Alert
	var a alertScanner
	feed := func(s string) { a.feed([]byte(s), func(x Alert) { got = append(got, x) }) }
	feed("plain \x07 bell")
	feed("\x1b]0;title\x07")     // window title: not an alert
	feed("\x1b]9;Claude needs ") // split across chunks
	feed("your permission\x07")
	feed("\x1b]777;notify;Codex;turn complete\x1b\\")
	feed("\x1b]9;4;1;50\x07") // progress, ignored
	if len(got) != 3 || got[0].Message != "" || got[1].Message != "Claude needs your permission" || got[2].Message != "Codex: turn complete" {
		t.Fatalf("alerts: %+v", got)
	}
}
