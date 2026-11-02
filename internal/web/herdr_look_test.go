package web

import (
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
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
	s.looks.note(id, "p4")
	if !s.paneWatched(id, "p4") {
		t.Fatal("an agent shown on an agents page should count as looked at")
	}
	if s.paneWatched(id, "p3") {
		t.Fatal("another agent of the session counted as looked at")
	}
	if s.paneWatched("fedcba9876543210", "p4") {
		t.Fatal("the same pane of another session counted as looked at")
	}
	s.looks.at[id+"\x00p4"] = time.Now().Add(-lookTTL - time.Second)
	if s.paneWatched(id, "p4") {
		t.Fatal("still looked at long after the page last read it")
	}
}

// The app icon's number: the panes waiting on the herdr servers a person's
// sessions watch, each server once (two sessions of one server, one count),
// those waiting before the watch began too; another person's not.
func TestAgentsWaitingCountsEachServerOnce(t *testing.T) {
	s := &Server{herdrSocks: herdrSockets{path: map[string]string{}}}
	a := s.herdrTimes.of(&store.Host{ID: 1, Address: "a"}, "/s")
	b := s.herdrTimes.of(&store.Host{ID: 2, Address: "b"}, "/s")
	ka, kb := herdrServerKey(&store.Host{ID: 1, Address: "a"}, "/s"), herdrServerKey(&store.Host{ID: 2, Address: "b"}, "/s")
	a.of("p1", herdr.StatusBlocked, false) // (waiting when first seen: no change to tell of it)
	a.changed("p2", herdr.StatusBlocked)
	a.changed("p3", herdr.StatusWorking)
	b.changed("p1", herdr.StatusBlocked)
	for id, w := range map[string]watchedServer{"s1": {ka, 7}, "s2": {ka, 7}, "s3": {kb, 8}} {
		s.herdrSocks.set(id, "/s")
		s.herdrSocks.setServer(id, w.key, w.owner)
	}
	if n := s.agentsWaiting(7); n != 2 {
		t.Fatalf("waiting for 7: %d, want 2", n)
	}
	if n := s.agentsWaiting(8); n != 1 {
		t.Fatalf("waiting for 8: %d, want 1", n)
	}
	a.changed("p2", herdr.StatusGone)
	s.herdrSocks.set("s3", "") // (its watch over: its server no longer counted)
	if s.agentsWaiting(7) != 1 || s.agentsWaiting(8) != 0 {
		t.Fatalf("after: %d, %d; want 1, 0", s.agentsWaiting(7), s.agentsWaiting(8))
	}
}
