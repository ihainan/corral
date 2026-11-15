package web

import "testing"

// herdr's answer for a phone's pane (0.9.5, off the test host).
const layoutPhone = `{"id":"cli:pane:layout","result":{"layout":{"area":{"height":32,"width":47,"x":0,"y":0},"focused_pane_id":"w1:p1","panes":[{"focused":true,"pane_id":"w1:p1","rect":{"height":32,"width":47,"x":0,"y":0}}],"splits":[],"tab_id":"w1:t1","workspace_id":"w1","zoomed":false},"type":"pane_layout"}}`

func TestPaneLayout(t *testing.T) {
	own, area, ok := paneLayout([]byte(layoutPhone), "w1:p1")
	if !ok || own != (rect{47, 32}) || area != (rect{47, 32}) {
		t.Fatalf("got %v %v %v", own, area, ok)
	}
	if _, _, ok := paneLayout([]byte(layoutPhone), "w1:p9"); ok {
		t.Error("a pane that is not in the layout was read")
	}
	if _, _, ok := paneLayout([]byte(`{"error":{"code":"server_not_running"}}`), "w1:p1"); ok {
		t.Error("an error was read as a layout")
	}
}

func TestRoomyWanted(t *testing.T) {
	for _, c := range []struct {
		name       string
		own, area  rect
		cols, rows int
		want       bool
	}{
		{"a phone's pane, this session's client sizing it", rect{47, 32}, rect{47, 32}, 47, 34, true},
		{"wide but short", rect{120, 22}, rect{120, 22}, 146, 23, true},
		{"roomy already", rect{124, 44}, rect{124, 44}, 150, 45, false},
		// A desktop's herdr on the same session sizes the pane: this
		// session's terminal is a phone's, the area larger than it.
		{"another client's size, a narrow split of it", rect{60, 58}, rect{174, 59}, 47, 34, false},
		{"a narrow split of this session's own wide terminal", rect{45, 44}, rect{90, 44}, 116, 45, true},
	} {
		if got := roomyWanted(c.own, c.area, c.cols, c.rows); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
