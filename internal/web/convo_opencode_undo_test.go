package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/convo"
)

// ocUndoDB makes an OpenCode database of one session: three messages, the
// last two taken back by /undo (revert at the second). Its path.
func ocUndoDB(t *testing.T, revert string) string {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	db := filepath.Join(t.TempDir(), "opencode.db")
	script := `import sqlite3, sys, json
c = sqlite3.connect(sys.argv[1])
c.execute('create table session (id text, directory text, parent_id text, title text, revert text, time_updated int)')
c.execute('create table message (id text, session_id text, time_created int, time_updated int, data text)')
c.execute('create table part (id text, message_id text, session_id text, time_created int, time_updated int, data text)')
c.execute('insert into session values (?,?,?,?,?,?)', ('ses_aaaaaaaaaa', '/w', None, 'undo test', sys.argv[2] or None, 300))
for i, (mid, role) in enumerate([('msg_aaaaaaaaa1', 'user'), ('msg_aaaaaaaaa2', 'user'), ('msg_aaaaaaaaa3', 'assistant')]):
    t = 100 + i * 10
    c.execute('insert into message values (?,?,?,?,?)', (mid, 'ses_aaaaaaaaaa', t, t, json.dumps({'role': role, 'time': {'created': t, 'completed': t}})))
    c.execute('insert into part values (?,?,?,?,?,?)', ('prt_aaaaaaaa%d' % i, mid, 'ses_aaaaaaaaaa', t, t, json.dumps({'type': 'text', 'text': 'said %d' % i})))
c.commit()
`
	if out, err := exec.Command("python3", "-c", script, db, revert).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return db
}

func ocRun(t *testing.T, db, mode string, args ...string) []byte {
	out, err := exec.Command("sh", "-c", ocQueryCommand(db, mode, append([]string{"ses_aaaaaaaaaa"}, args...)...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ocGone(p *convo.OpenCode) string {
	var g []string
	for _, e := range p.Since(0) {
		if e.Gone {
			g = append(g, e.ID)
		}
	}
	return strings.Join(g, " ")
}

// /undo back to the second message: it and what came after are taken back.
func TestOpenCodeUndoRead(t *testing.T) {
	db := ocUndoDB(t, `{"messageID":"msg_aaaaaaaaa2","snapshot":"x"}`)
	out := ocRun(t, db, "tail", "100")
	if !strings.Contains(string(out), `"rv":"msg_aaaaaaaaa2"`) {
		t.Fatalf("the session's line does not say what /undo went back to: %s", out)
	}
	p := convo.NewOpenCode()
	feed(p, out)
	if g := ocGone(p); g != "msg_aaaaaaaaa2 msg_aaaaaaaaa3" {
		t.Fatalf("gone %q", g)
	}
}

// Undone, then the next message sent: OpenCode deleted what was taken back
// and cleared revert. A look after (since) finds fewer messages than were
// read: those not there are taken back, and stay so.
func TestOpenCodeUndoThenDeleted(t *testing.T) {
	db := ocUndoDB(t, "")
	p := convo.NewOpenCode()
	feed(p, ocRun(t, db, "tail", "100"))
	if g := ocGone(p); g != "" {
		t.Fatalf("gone before any undo: %q", g)
	}
	// (/undo to the second, then a new message: OpenCode's cleanup.)
	if out, err := exec.Command("python3", "-c", `import sqlite3, sys, json
c = sqlite3.connect(sys.argv[1])
c.execute("delete from message where id >= 'msg_aaaaaaaaa2'")
c.execute("insert into message values ('msg_aaaaaaaaa4','ses_aaaaaaaaaa',200,200,?)", (json.dumps({'role': 'user'}),))
c.execute("insert into part values ('prt_aaaaaaaa9','msg_aaaaaaaaa4','ses_aaaaaaaaaa',200,200,?)", (json.dumps({'type': 'text', 'text': 'again'}),))
c.commit()`, db).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	fc, fid, _ := p.Floor()
	cursor := int64(121)
	out := ocRun(t, db, "since", "121", "0", "", strconv.FormatInt(fc, 10), fid, strconv.Itoa(p.KnownBefore(cursor)))
	if !strings.Contains(string(out), `"k":"ids"`) {
		t.Fatalf("the deletion is not told: %s", out)
	}
	feed(p, out)
	if g := ocGone(p); g != "msg_aaaaaaaaa2 msg_aaaaaaaaa3" {
		t.Fatalf("gone %q", g)
	}
	// (Told once: the next look counts as many as are known.)
	again := ocRun(t, db, "since", "200", "0", "", strconv.FormatInt(fc, 10), fid, strconv.Itoa(p.KnownBefore(200)))
	if strings.Contains(string(again), `"k":"ids"`) {
		t.Fatalf("told again: %s", again)
	}
}

// /redo: revert cleared with the messages still there: they come back.
func TestOpenCodeRedo(t *testing.T) {
	db := ocUndoDB(t, `{"messageID":"msg_aaaaaaaaa2"}`)
	p := convo.NewOpenCode()
	feed(p, ocRun(t, db, "tail", "100"))
	if out, err := exec.Command("python3", "-c", `import sqlite3, sys
c = sqlite3.connect(sys.argv[1]); c.execute("update session set revert = null"); c.commit()`, db).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	rev := p.Rev()
	fc, fid, _ := p.Floor()
	feed(p, ocRun(t, db, "since", "121", "0", "", strconv.FormatInt(fc, 10), fid, strconv.Itoa(p.KnownBefore(121))))
	if g := ocGone(p); g != "" {
		t.Fatalf("still gone after /redo: %q", g)
	}
	if ch := p.Since(rev); len(ch) != 2 {
		t.Fatalf("what came back is not told: %+v", ch)
	}
}

// feed gives the parser what a read printed, as ocRead does: the rows,
// and the first message read as its floor.
func feed(p *convo.OpenCode, out []byte) {
	var rows []byte
	for _, l := range strings.Split(string(out), "\n") {
		var r struct {
			K  string `json:"k"`
			C  int64  `json:"c"`
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(l), &r) != nil {
			continue
		}
		switch r.K {
		case "floor":
			p.SetFloor(r.C, r.ID)
		case "s", "m", "p", "ids":
			rows = append(rows, l+"\n"...)
		}
	}
	p.Feed(rows)
}

// Undo seen; then the next message sent: OpenCode deletes what was taken
// back (one made in the very millisecond the last look ended on) and
// clears revert. The same look tells both: nothing taken back shows again.
func TestOpenCodeUndoClearedSameLook(t *testing.T) {
	db := ocUndoDB(t, `{"messageID":"msg_aaaaaaaaa2"}`)
	p := convo.NewOpenCode()
	feed(p, ocRun(t, db, "tail", "100"))
	if g := ocGone(p); g != "msg_aaaaaaaaa2 msg_aaaaaaaaa3" {
		t.Fatalf("gone %q", g)
	}
	if out, err := exec.Command("python3", "-c", `import sqlite3, sys, json
c = sqlite3.connect(sys.argv[1])
c.execute("delete from message where id >= 'msg_aaaaaaaaa2'")
c.execute("update session set revert = null")
c.execute("insert into message values ('msg_aaaaaaaaa4','ses_aaaaaaaaaa',200,200,?)", (json.dumps({'role': 'user'}),))
c.execute("insert into part values ('prt_aaaaaaaa9','msg_aaaaaaaaa4','ses_aaaaaaaaaa',200,200,?)", (json.dumps({'type': 'text', 'text': 'again'}),))
c.commit()`, db).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	fc, fid, _ := p.Floor()
	cursor := int64(120) // (the last message read was made at 120: the look ended there)
	feed(p, ocRun(t, db, "since", "120", "0", "", strconv.FormatInt(fc, 10), fid, strconv.Itoa(p.KnownBefore(cursor))))
	if g := ocGone(p); g != "msg_aaaaaaaaa2 msg_aaaaaaaaa3" {
		t.Fatalf("gone after the same look %q", g)
	}
}
