package web

import (
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
)

// lookServer: a server with one herdr session watched, its snapshot having
// two workspaces, the first focused, its tab t1 showing p1 and p2 (p3 in
// its other tab t2, p4 in the second workspace).
func lookServer(t *testing.T, focusedTab string) (*Server, string) {
	t.Helper()
	s := &Server{herdrSocks: herdrSockets{path: map[string]string{}}, vis: visibility{seen: map[string]map[*liveSocket]bool{}}}
	const id = "0123456789abcdef"
	s.herdrSocks.set(id, "/run/herdr.sock")
	s.herdrSocks.setView(id, &herdr.Snapshot{
		FocusedWorkspace: "w1", FocusedTab: focusedTab,
		Workspaces: []herdr.Workspace{{ID: "w1", ActiveTab: "t1"}, {ID: "w2", ActiveTab: "t3"}},
		Panes: []herdr.Pane{
			{ID: "p1", TabID: "t1", WorkspaceID: "w1"}, {ID: "p2", TabID: "t1", WorkspaceID: "w1"},
			{ID: "p3", TabID: "t2", WorkspaceID: "w1"}, {ID: "p4", TabID: "t3", WorkspaceID: "w2"},
		},
	})
	return s, id
}

func TestPaneWatchedGoesByWhatHerdrShows(t *testing.T) {
	for _, tab := range []string{"t1", ""} { // ("": herdr 0.8, no focused tab told)
		s, id := lookServer(t, tab)
		for _, p := range []string{"p1", "p2", "p3", "p4"} {
			if s.paneWatched(id, p) {
				t.Fatalf("tab %q: %s watched with no one looking", tab, p)
			}
		}
		s.vis.set(id, &liveSocket{}, true)
		want := map[string]bool{"p1": true, "p2": true, "p3": false, "p4": false}
		for p, w := range want {
			if got := s.paneWatched(id, p); got != w {
				t.Fatalf("tab %q: %s watched = %v, want %v", tab, p, got, w)
			}
		}
	}
}

func TestPaneWatchedWithoutAViewIsTheSession(t *testing.T) {
	s := &Server{herdrSocks: herdrSockets{path: map[string]string{}}, vis: visibility{seen: map[string]map[*liveSocket]bool{}}}
	const id = "0123456789abcdef"
	s.herdrSocks.set(id, "/run/herdr.sock")
	// (No focused workspace told: which panes show is not known.)
	s.herdrSocks.setView(id, &herdr.Snapshot{Panes: []herdr.Pane{{ID: "p1", TabID: "t1"}}})
	s.vis.set(id, &liveSocket{}, true)
	if !s.paneWatched(id, "p9") {
		t.Fatal("not known what herdr shows: the whole session should count as looked at, as before")
	}
	// (The watch over: what was kept goes with it.)
	s2, id2 := lookServer(t, "t1")
	s2.herdrSocks.set(id2, "")
	s2.vis.set(id2, &liveSocket{}, true)
	if !s2.paneWatched(id2, "p4") {
		t.Fatal("a view kept after its watch ended")
	}
}

func TestPaneWatchedByAnAgentsPage(t *testing.T) {
	s, id := lookServer(t, "t1")
	s.looks.note(id, "p4", "look-0001")
	if !s.paneWatched(id, "p4") {
		t.Fatal("an agent shown on an agents page should count as looked at")
	}
	if s.paneWatched(id, "p3") {
		t.Fatal("another agent of the session counted as looked at")
	}
	if s.paneWatched("fedcba9876543210", "p4") {
		t.Fatal("the same pane of another session counted as looked at")
	}
	s.looks.at[id+"\x00p4"] = paneLook{time.Now().Add(-lookTTL - time.Second), "look-0001"}
	if s.paneWatched(id, "p4") {
		t.Fatal("still looked at long after the page last read it")
	}
}

// A look's end said: not looked at at once; a reading of that look come
// late (sent before its end) does not bring it back; a later look of the
// same pane is not ended by the earlier one's end come late.
func TestLookEndsByItsName(t *testing.T) {
	s, id := lookServer(t, "t1")
	s.looks.note(id, "p4", "look-0001")
	s.looks.end(id, "p4", "look-0001")
	if s.paneWatched(id, "p4") {
		t.Fatal("still looked at after its end was said")
	}
	s.looks.note(id, "p4", "look-0001") // (a reading of it, late)
	if s.paneWatched(id, "p4") {
		t.Fatal("a reading come after the look's end brought it back")
	}
	s.looks.note(id, "p4", "look-0002")
	s.looks.end(id, "p4", "look-0001") // (said again, late)
	if !s.paneWatched(id, "p4") {
		t.Fatal("a later look ended by an earlier one's end")
	}
}

// The app icon's number: the panes waiting in the last snapshot of each
// herdr server a person's sessions watch, each server once (two sessions of
// one server, one count); another person's not; a session whose watch ended
// not at all.
func TestAgentsWaitingCountsEachServerOnce(t *testing.T) {
	s := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	snapA := &herdr.Snapshot{Panes: []herdr.Pane{{ID: "p1", AgentStatus: herdr.StatusBlocked}, {ID: "p2", AgentStatus: herdr.StatusBlocked}, {ID: "p3", AgentStatus: herdr.StatusWorking}}}
	snapB := &herdr.Snapshot{Panes: []herdr.Pane{{ID: "p1", AgentStatus: herdr.StatusBlocked}}}
	of := func(snap **herdr.Snapshot) func() *herdr.Snapshot { return func() *herdr.Snapshot { return *snap } }
	var none *herdr.Snapshot
	for id, w := range map[string]watchedServer{"s1": {"ka", 7, of(&snapA)}, "s2": {"ka", 7, of(&snapA)}, "s0": {"ka", 7, of(&none)}, "s3": {"kb", 8, of(&snapB)}} {
		s.herdrSocks.set(id, "/s")
		s.herdrSocks.setServer(id, w.key, w.owner, w.latest)
	}
	if n := s.agentsWaiting(7); n != 2 {
		t.Fatalf("waiting for 7: %d, want 2", n)
	}
	if n := s.agentsWaiting(8); n != 1 {
		t.Fatalf("waiting for 8: %d, want 1", n)
	}
	snapA = &herdr.Snapshot{Panes: []herdr.Pane{{ID: "p1", AgentStatus: herdr.StatusBlocked}}} // (p2 closed meanwhile)
	s.herdrSocks.set("s3", "")
	if s.agentsWaiting(7) != 1 || s.agentsWaiting(8) != 0 {
		t.Fatalf("after: %d, %d; want 1, 0", s.agentsWaiting(7), s.agentsWaiting(8))
	}
}
