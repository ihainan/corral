package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The person's turns in a whole log: theirs only (not a tool's result),
// where each line begins; read on as the log grows; a line not ended yet
// counted once it is; another log in its place gone through anew.
func TestTurnIndex(t *testing.T) {
	c := sftpOf(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "s.jsonl")
	user := func(id, text string) string {
		return `{"type":"user","uuid":"` + id + `","timestamp":"2026-10-31T10:00:0` + id[1:] + `.000Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	tool := `{"type":"user","uuid":"t1","timestamp":"2026-10-31T10:00:05.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}` + "\n"
	said := `{"type":"assistant","uuid":"a1","timestamp":"2026-10-31T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"sure"}]}}` + "\n"
	first := user("u1", "first question") + said + tool + user("u2", `second   one\n with lines`)
	os.WriteFile(log, []byte(first), 0o644)
	idx := &turnIndex{}
	idx.reset(log, "claude", 1)
	rd := func() turnReader {
		return turnReader{
			size: func() (int64, error) {
				fi, err := c.Lstat(idx.log)
				if err != nil {
					return 0, err
				}
				return fi.Size(), nil
			},
			read: func(off int64, n int) ([]byte, error) { return c.ReadRange(idx.log, off, n) },
		}
	}
	scan := func() bool {
		more, _, err := idx.scan(rd(), time.Now().Add(5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return more
	}
	scan()
	if len(idx.turns) != 2 || idx.turns[0].Preview != "first question" || idx.turns[1].Preview != "second one with lines" || idx.turns[0].At != 0 {
		t.Fatalf("theirs, not the tool's: %+v", idx.turns)
	}
	if want := int64(len(user("u1", "first question") + said + tool)); idx.turns[1].At != want {
		t.Errorf("where its line begins: %d, want %d", idx.turns[1].At, want)
	}
	// (Grown: read on; a line not ended yet, counted once it is.)
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	half := user("u3", "third")
	f.WriteString(half[:20])
	f.Close()
	scan()
	if len(idx.turns) != 2 {
		t.Errorf("a line not ended: %+v", idx.turns)
	}
	f, _ = os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(half[20:])
	f.Close()
	scan()
	if len(idx.turns) != 3 || idx.turns[2].Preview != "third" {
		t.Errorf("ended: %+v", idx.turns)
	}
	// (Another log in its place: its head another.)
	os.WriteFile(log, []byte(user("u9", "anew")+strings.Repeat(said, 3)), 0o644)
	scan()
	if len(idx.turns) != 1 || idx.turns[0].Preview != "anew" {
		t.Errorf("another log: %+v", idx.turns)
	}
	// (Written with a space after its colons, as another JSON writer may.)
	os.WriteFile(log, []byte(`{"type": "user", "uuid": "u1", "timestamp": "2026-10-31T10:00:00.000Z", "message": {"role": "user", "content": "spaced"}}`+"\n"), 0o644)
	idx.reset(log, "claude", 1)
	scan()
	if len(idx.turns) != 1 || idx.turns[0].Preview != "spaced" {
		t.Errorf("spaced: %+v", idx.turns)
	}
	// (Codex's: its UserMessage items.)
	cx := filepath.Join(dir, "c.jsonl")
	os.WriteFile(cx, []byte(`{"timestamp":"2026-10-31T10:00:00.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"codex question"}]}}}`+"\n"), 0o644)
	idx.reset(cx, "codex", 1)
	scan()
	if len(idx.turns) != 1 || idx.turns[0].Preview != "codex question" {
		t.Errorf("codex: %+v", idx.turns)
	}
	// (review517: Codex's older user_message events too.)
	os.WriteFile(cx, []byte(`{"timestamp":"2026-10-31T10:00:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"older form"}}`+"\n"), 0o644)
	idx.reset(cx, "codex", 1)
	scan()
	if len(idx.turns) != 1 || idx.turns[0].Preview != "older form" {
		t.Errorf("codex user_message: %+v", idx.turns)
	}
	// (review517: a line too long to be the person's — even one that is
	// theirs after a megabyte of spaces — passed over to its end; the next
	// where it begins.)
	long := strings.Repeat(" ", turnChunk+10) + user("u1", "hidden")
	next := user("u2", "after the long one")
	os.WriteFile(log, []byte(long+next), 0o644)
	idx.reset(log, "claude", 1)
	scan()
	if len(idx.turns) != 1 || idx.turns[0].Preview != "after the long one" || idx.turns[0].At != int64(len(long)) {
		t.Errorf("past a long line: %+v (want at %d)", idx.turns, len(long))
	}
	// (review517: another log of the same head and length: what is just
	// before where it was read to tells it.)
	a := user("u1", "same head") + user("u2", "AAAA")
	os.WriteFile(log, []byte(a), 0o644)
	idx.reset(log, "claude", 1)
	scan()
	os.WriteFile(log, []byte(strings.Replace(a, "AAAA", "BBBB", 1)), 0o644)
	scan()
	if len(idx.turns) != 2 || idx.turns[1].Preview != "BBBB" {
		t.Errorf("replaced, same head and length: %+v", idx.turns)
	}
}

// review517: past the turns kept, the first let go of in batches.
func TestTurnsKept(t *testing.T) {
	idx := &turnIndex{}
	for i := 0; i < turnsKept+turnsSlack+1; i++ {
		idx.add(convoTurn{At: int64(i)})
	}
	if len(idx.turns) != turnsKept || !idx.cut || idx.turns[len(idx.turns)-1].At != int64(turnsKept+turnsSlack) {
		t.Errorf("kept %d, cut %v", len(idx.turns), idx.cut)
	}
}

// A long message: its first characters.
func TestTurnPreview(t *testing.T) {
	if p := preview(strings.Repeat("字", 200)); len([]rune(p)) != turnPreview+1 || !strings.HasSuffix(p, "…") {
		t.Errorf("cut: %q", p)
	}
}
