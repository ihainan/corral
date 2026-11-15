package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCodexTurnIndexRewind(t *testing.T) {
	started := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}\n"
	message := func(text string) string {
		return fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n", text)
	}
	data := started + message("A") + started + message("B") + message("steer B") + started + message("C")
	idx := &turnIndex{}
	idx.reset("fixture", "codex", 1)
	read := turnReader{
		size: func() (int64, error) { return int64(len(data)), nil },
		read: func(off int64, n int) ([]byte, error) { return []byte(data[off:min(int(off)+n, len(data))]), nil },
	}
	check := func(want string) {
		t.Helper()
		if _, _, err := idx.scan(read, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		var previews []string
		for _, turn := range idx.turns {
			previews = append(previews, turn.Preview)
		}
		if got := strings.Join(previews, ","); got != want {
			t.Fatalf("index %q, want %q", got, want)
		}
	}
	check("A,B,steer B,C")
	data += "{\"type\":\"event_msg\",\"payload\":{\"type\":\"thread_rolled_back\",\"num_turns\":2}}\n"
	check("A")
	data += started + message("D")
	check("A,D")
	data += "{\"type\":\"event_msg\",\"payload\":{\"type\":\"thread_rolled_back\",\"num_turns\":0}}\n"
	check("A,D")
	data += "{\"type\":\"event_msg\",\"payload\":{\"type\":\"thread_rolled_back\",\"num_turns\":999999}}\n"
	check("")
	data = started + message("replacement")
	check("replacement")
}

func TestCodexTurnIndexRewindAfterTrimming(t *testing.T) {
	idx := &turnIndex{agent: "codex"}
	for i := 0; i < turnsKept+turnsSlack+2; i++ {
		idx.codexOpen = true
		idx.add(convoTurn{At: int64(i), Preview: fmt.Sprint(i)})
	}
	if !idx.cut || len(idx.codexStarts) != len(idx.turns) || idx.codexStarts[0] != 0 {
		t.Fatal("trimmed task boundaries do not match retained messages")
	}
	last := idx.turns[len(idx.turns)-2].Preview
	idx.codexEvent([]byte(`{"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}`))
	if got := idx.turns[len(idx.turns)-1].Preview; got != last {
		t.Fatalf("wrong task removed after trimming: %q, want %q", got, last)
	}
}
