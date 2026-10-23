package web

import (
	"testing"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

func TestAgentTimes(t *testing.T) {
	a := &agentTimes{at: map[string]agentTime{}}
	first := a.of("w1:p1", "idle") // (first seen: now, and kept)
	time.Sleep(5 * time.Millisecond)
	if got := a.of("w1:p1", "idle"); got != first {
		t.Fatalf("first-seen time moved: %d then %d", first, got)
	}
	// The same change told twice (two watchers) moves it once.
	a.changed("w1:p1", "working")
	once := a.of("w1:p1", "working")
	if once <= first {
		t.Fatalf("a change did not move it: %d then %d", first, once)
	}
	time.Sleep(5 * time.Millisecond)
	a.changed("w1:p1", "working")
	if got := a.of("w1:p1", "working"); got != once {
		t.Fatalf("the same change moved it again: %d then %d", once, got)
	}
	// Found in another status than noted (changed while no one watched): now.
	time.Sleep(5 * time.Millisecond)
	if got := a.of("w1:p1", "blocked"); got <= once {
		t.Fatalf("a status not noted kept the old time: %d", got)
	}
	a.changed("w1:p1", herdr.StatusGone)
	if _, ok := a.at["w1:p1"]; ok {
		t.Fatal("a pane gone is still kept")
	}
}

func TestHerdrServersByWhereTheHostIs(t *testing.T) {
	var h herdrServers
	a := h.of(&store.Host{ID: 1, Address: "a.example", Port: 22, Username: "u"}, "/s")
	if h.of(&store.Host{ID: 1, Address: "a.example", Port: 22, Username: "u"}, "/s") != a {
		t.Fatal("the same server got two")
	}
	if h.of(&store.Host{ID: 1, Address: "b.example", Port: 22, Username: "u"}, "/s") == a {
		t.Fatal("a record moved to another machine shares the first one's")
	}
}
