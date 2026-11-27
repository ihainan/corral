package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/term"
)

// A Devin CLI database (its tables as 3000.15's, the rows shaped as its own
// were, 11-26), made up: nothing of anyone's conversation in it.
const dvSID = "brave-otter"

// dvMakeScript makes (or adds to) the database at argv[1]: a JSON list of
// nodes, each [node_id, parent, role, extra] (extra: the message's fields
// beyond role and message_id, its metadata merged into its metadata; with
// "_node": the node's own metadata), then the session's tip.
const dvMakeScript = `import sqlite3, sys, json
c = sqlite3.connect(sys.argv[1])
c.execute('create table if not exists sessions (id text primary key, working_directory text not null, backend_type text not null, model text not null, agent_mode text not null, created_at int not null, last_activity_at int not null, title text, main_chain_id int, shell_last_seen_index int default 0, cogs_json text, workspace_dirs text, hidden int not null default 0, metadata text)')
c.execute('create table if not exists message_nodes (row_id integer primary key autoincrement, session_id text not null, node_id int not null, parent_node_id int, chat_message text not null, created_at int not null, metadata text, unique(session_id, node_id))')
c.execute('create table if not exists prompt_history (id integer primary key autoincrement, content text not null, timestamp int not null, session_id text not null, is_shell int not null default 0)')
a = json.load(open(sys.argv[2]))
for n, p, role, extra in a['nodes']:
    node_md = extra.pop('_node', None)
    m = {'message_id': 'm-%d' % n, 'role': role}
    md = {'num_tokens': None, 'is_user_input': None, 'created_at': '2026-11-26T07:%02d:00Z' % (n % 60)}
    md.update(extra.pop('metadata', {}))
    m.update(extra)
    m['metadata'] = md
    c.execute('insert into message_nodes (session_id, node_id, parent_node_id, chat_message, created_at, metadata) values (?,?,?,?,?,?)', (a['sid'], n, p, json.dumps(m), 1795676000 + n, json.dumps(node_md) if node_md else None))
for t in a.get('prompts') or []:
    c.execute('insert into prompt_history (content, timestamp, session_id) values (?,?,?)', (t, 1795676000, a['sid']))
c.execute('insert or replace into sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, title, main_chain_id) values (?,?,?,?,?,?,?,?,?)', (a['sid'], a.get('cwd', '/w'), 'windsurf', 'swe-test-low', 'bypass', 1795676000, 1795676100 + a['tip'], a.get('title'), a['tip']))
c.commit()
`

type dvNode = []any

func dvMake(t *testing.T, db string, nodes []dvNode, tip int, extra map[string]any) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	arg := map[string]any{"sid": dvSID, "nodes": nodes, "tip": tip}
	for k, v := range extra {
		arg[k] = v
	}
	b, _ := json.Marshal(arg)
	f := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("python3", "-c", dvMakeScript, db, f).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func dvRun(t *testing.T, db, mode string, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("sh", "-c", dvQueryCommand(db, mode, append([]string{dvSID}, args...)...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sys0(n, p int) dvNode {
	return dvNode{n, nilIf(p), "system", map[string]any{"content": "<system_info>made up</system_info>"}}
}

func nilIf(p int) any {
	if p < 0 {
		return nil
	}
	return p
}

func user(n, p int, text string) dvNode {
	return dvNode{n, p, "user", map[string]any{"content": text, "metadata": map[string]any{"is_user_input": true}}}
}

func asst(n, p int, text, fin string, calls ...map[string]any) dvNode {
	if calls == nil {
		calls = []map[string]any{}
	}
	return dvNode{n, p, "assistant", map[string]any{"content": text, "tool_calls": calls,
		"thinking": map[string]any{"thinking": "thinking " + strconv.Itoa(n), "signature": "x"},
		"metadata": map[string]any{"finish_reason": fin, "generation_model": "swe-test-low", "metrics": map[string]any{"context_input_tokens": 1000 + n, "output_tokens": 10, "total_time_ms": 500}}}}
}

func call(id, name string, args map[string]any) map[string]any {
	return map[string]any{"id": id, "name": name, "arguments": args, "index": 0, "kind": "function"}
}

func result(n, p int, callID, text string, ext map[string]any) dvNode {
	if ext == nil {
		ext = map[string]any{}
	}
	if _, ok := ext["chisel/tool_result_meta"]; !ok {
		ext["chisel/tool_result_meta"] = map[string]any{"success": true}
	}
	return dvNode{n, p, "tool", map[string]any{"content": text, "tool_call_id": callID, "metadata": map[string]any{"extensions": ext}}}
}

// dvFeed: of what the query printed, a parser fed its rows; its controls.
func dvFeed(t *testing.T, p *convo.Devin, out []byte) (r dvRead, tip int64, reset bool) {
	t.Helper()
	r, tip, reset, ok := dvRows(out)
	if !ok {
		t.Fatalf("no reader: %s", out)
	}
	p.SetFloor(r.floor)
	p.Feed(r.lines)
	return r, tip, reset
}

func dvSaid(p *convo.Devin) []string {
	var s []string
	for _, e := range p.Since(0) {
		var w []string
		for _, x := range e.Parts {
			switch {
			case x.Tool != nil:
				d := x.Tool.Name + "(" + x.Tool.Detail + ")"
				if x.Tool.Done {
					d += "=" + strings.TrimSpace(x.Tool.Result)
				}
				if x.Tool.Error {
					d += "!"
				}
				if x.Tool.Add > 0 || x.Tool.Del > 0 {
					d += "+" + strconv.Itoa(x.Tool.Add) + "-" + strconv.Itoa(x.Tool.Del)
				}
				w = append(w, d)
			case x.Event != nil:
				w = append(w, "event:"+x.Event.Kind)
			case x.Trouble != nil:
				w = append(w, "trouble:"+x.Trouble.Kind)
			default:
				w = append(w, x.Kind+":"+x.Text)
			}
		}
		s = append(s, e.Role+" "+strings.Join(w, " | "))
	}
	return s
}

// A turn: what was asked, a command run, an edit made, a write, and the
// answer, read from the session's line only — not a node of another
// branch, nor the copy of a message an earlier line held.
func dvTurn() []dvNode {
	return []dvNode{
		sys0(0, -1), user(1, 0, "an earlier copy"), // (a line made anew since: not the session's)
		sys0(2, -1), sys0(3, 2), user(4, 3, "fix the greeting"),
		asst(5, 4, "", "tool_calls", call("exec:0#a", "exec", map[string]any{"command": "echo hi"})),
		asst(6, 4, "", "tool_calls"), // (a sibling: not on the line)
		result(7, 5, "exec:0#a", "Output from command in shell 1:\nhi\n\nExit code: 0", map[string]any{"chisel/terminal_output": map[string]any{"text": "hi\n", "exit": map[string]any{"exit_code": 0}}}),
		asst(8, 7, "", "tool_calls", call("c#b", "edit", map[string]any{"file_path": "/w/a.txt", "old_string": "x", "new_string": "y\nz"})),
		result(9, 8, "c#b", "The file /w/a.txt has been updated.", map[string]any{"chisel/file_operations": []any{map[string]any{"operation": "edit", "file_path": "/w/a.txt", "lines_added": 2, "lines_removed": 1}}}),
		sys0(10, 9), // (a reminder of its own between: not shown)
		asst(11, 10, "", "tool_calls", call("c#c", "exec", map[string]any{"command": "false"})),
		result(12, 11, "c#c", "Exit code: 1", map[string]any{"chisel/terminal_output": map[string]any{"text": "", "exit": map[string]any{"exit_code": 1}}}),
		dvNode{13, 12, "user", map[string]any{"content": "continue", "metadata": map[string]any{"telemetry": map[string]any{"operation": "continue_prompt"}}}}, // (not the person's)
		asst(14, 13, "done", "stop"),
	}
}

func TestDevinQueryTail(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	dvMake(t, db, dvTurn(), 14, map[string]any{"title": "Fix the greeting"})
	p := convo.NewDevin()
	r, tip, reset := dvFeed(t, p, dvRun(t, db, "tail", "200"))
	if tip != 14 || reset || r.older != 0 || !r.hasOlder || r.floor != 4 {
		t.Fatalf("tip %d reset %v older %d floor %d", tip, reset, r.older, r.floor)
	}
	want := []string{
		"user text:fix the greeting",
		"assistant thinking:thinking 5 | Bash(echo hi)=hi",
		"assistant thinking:thinking 8 | Edit(/w/a.txt)=The file /w/a.txt has been updated.+2-1",
		"assistant thinking:thinking 11 | Bash(false)=!",
		"assistant thinking:thinking 14 | text:done",
	}
	if got := dvSaid(p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("read:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	m := p.Meta()
	if p.Title() != "Fix the greeting" || m.Model != "swe-test-low" || m.Mode != "bypass" || m.Context != 1014 || p.Status() != "idle" {
		t.Errorf("title %q meta %+v status %q", p.Title(), m, p.Status())
	}
	// (Nothing since: nothing printed but where it is.)
	r2, tip2, reset2, _ := dvRows(dvRun(t, db, "since", "14", "200"))
	if tip2 != 14 || reset2 || strings.Count(string(r2.lines), "\n") != 1 {
		t.Errorf("since, unchanged: %d %v %q", tip2, reset2, r2.lines)
	}
}

// Rows written since: only those after the tip read, a result filling in a
// call read before; its line gone (rewound): read anew.
func TestDevinQuerySince(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	nodes := []dvNode{sys0(0, -1), user(1, 0, "list files"),
		asst(2, 1, "", "tool_calls", call("c#1", "exec", map[string]any{"command": "ls"}))}
	dvMake(t, db, nodes, 2, nil)
	p := convo.NewDevin()
	dvFeed(t, p, dvRun(t, db, "tail", "200"))
	if p.Status() != "busy" {
		t.Errorf("a call out: %q", p.Status())
	}
	rev := p.Rev()
	dvMake(t, db, []dvNode{result(3, 2, "c#1", "a\nb\n", nil), asst(4, 3, "two files", "stop")}, 4, nil)
	_, tip, reset := dvFeed(t, p, dvRun(t, db, "since", "2", "200"))
	if tip != 4 || reset {
		t.Fatalf("since: tip %d reset %v", tip, reset)
	}
	changed := p.Since(rev)
	if len(changed) != 2 || changed[0].Parts[1].Tool == nil || !changed[0].Parts[1].Tool.Done || changed[1].Parts[1].Text != "two files" {
		t.Fatalf("since: %+v", dvSaid(p))
	}
	if p.Status() != "idle" {
		t.Errorf("answered: %q", p.Status())
	}
	// Rewound: its line goes another way, from before the tip read.
	dvMake(t, db, []dvNode{user(5, 0, "something else"), asst(6, 5, "ok", "stop")}, 6, nil)
	_, tip, reset = dvFeed(t, convo.NewDevin(), dvRun(t, db, "since", "4", "200"))
	if tip != 6 || !reset {
		t.Errorf("rewound: tip %d reset %v", tip, reset)
	}
}

func TestDevinQuerySinceOverBudget(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	dvMake(t, db, []dvNode{sys0(0, -1), user(1, 0, "start")}, 1, nil)
	_, tip, _ := dvFeed(t, convo.NewDevin(), dvRun(t, db, "tail", "200"))
	if tip != 1 {
		t.Fatalf("tail tip %d", tip)
	}
	const added = 100
	nodes := make([]dvNode, 0, added)
	for i := 2; i < 2+added; i++ {
		text := "message " + strconv.Itoa(i) + " " + strings.Repeat("x", 65536)
		nodes = append(nodes, user(i, i-1, text))
	}
	end := 1 + added
	dvMake(t, db, nodes, end, nil)
	var got []int64
	cursor := tip
	for poll := 0; poll < 8 && cursor < int64(end); poll++ {
		out := dvRun(t, db, "since", strconv.FormatInt(cursor, 10), "200")
		r, next, reset, ok := dvRows(out)
		if !ok || reset {
			t.Fatalf("poll %d: ok %v reset %v", poll, ok, reset)
		}
		before := len(got)
		for _, l := range strings.Split(strings.TrimSpace(string(r.lines)), "\n") {
			var x struct {
				K string `json:"k"`
				N int64  `json:"n"`
			}
			if json.Unmarshal([]byte(l), &x) == nil && x.K == "u" {
				got = append(got, x.N)
			}
		}
		if len(got) == before && next <= cursor {
			t.Fatalf("poll %d made no progress: cursor %d next %d", poll, cursor, next)
		}
		cursor = next
	}
	if cursor != int64(end) {
		t.Fatalf("cursor %d after the polls, want %d", cursor, end)
	}
	if len(got) != added {
		t.Fatalf("%d rows read, want %d", len(got), added)
	}
	for i, n := range got {
		if want := int64(i + 2); n != want {
			t.Fatalf("row %d is node %d, want %d (out of order or repeated)", i, n, want)
		}
	}
}

// Read from its end, then back: what is before the first read, with a
// result whose call is in it paired; a compacted one's summary, and what it
// summed up before it.
func TestDevinQueryOlderAndCompacted(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	var nodes []dvNode
	nodes = append(nodes, sys0(0, -1))
	prev := 0
	for i := 1; i <= 30; i += 3 {
		nodes = append(nodes, user(i, prev, "ask "+strconv.Itoa(i)),
			asst(i+1, i, "", "tool_calls", call("c#"+strconv.Itoa(i), "read", map[string]any{"file_path": "/w/f" + strconv.Itoa(i)})),
			result(i+2, i+1, "c#"+strconv.Itoa(i), "body", nil))
		prev = i + 2
	}
	// Compacted after node 30: a new line, its summary naming it.
	nodes = append(nodes, sys0(40, -1),
		dvNode{41, 40, "system", map[string]any{"content": "Summary: read ten files.", "_node": map[string]any{"summarized_from": 30}}},
		user(42, 41, "after the summary"), asst(43, 42, "fine", "stop"))
	dvMake(t, db, nodes, 43, nil)
	p := convo.NewDevin()
	r, _, _ := dvFeed(t, p, dvRun(t, db, "tail", "8"))
	if !r.hasOlder || r.older == 0 || p.Floor() == 0 {
		t.Fatalf("tail: older %d floor %d", r.older, p.Floor())
	}
	// (Begun at an answer, not at a result whose call is before it.)
	if got := dvSaid(p); !strings.Contains(got[0], "=body") || got[len(got)-1] != "assistant thinking:thinking 43 | text:fine" {
		t.Fatalf("tail: %q", got)
	}
	joined := strings.Join(dvSaid(p), "\n")
	if !strings.Contains(joined, "note event:compact\nsummary text:Summary: read ten files.\nuser text:after the summary") {
		t.Errorf("compaction: %s", joined)
	}
	for p.Floor() > 0 {
		before := len(p.Since(0))
		o := convo.NewDevin()
		r, _, _ := dvFeed(t, o, dvRun(t, db, "older", strconv.FormatInt(p.Floor(), 10), "8"))
		p.Prepend(o)
		if len(p.Since(0)) == before || r.older == 0 {
			break
		}
	}
	got := dvSaid(p)
	if got[0] != "user text:ask 1" || got[1] != "assistant thinking:thinking 2 | Read(/w/f1)=body" {
		t.Fatalf("read back to its start: %q", got[:2])
	}
	users := 0
	for _, g := range got {
		if strings.HasPrefix(g, "user ") {
			users++
		}
		if strings.Contains(g, "Read(") && !strings.Contains(g, "=body") {
			t.Errorf("a call not paired with its result: %s", g)
		}
	}
	if users != 11 {
		t.Errorf("%d of the person's messages, want 11: %q", users, got)
	}
	for i, e := range p.Since(0)[1:] {
		if e.Seq <= p.Since(0)[i].Seq {
			t.Fatalf("out of order at %d", i)
		}
	}
}

func TestDevinQueryHugeMessage(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	dvMake(t, db, []dvNode{
		sys0(0, -1),
		user(1, 0, "before"),
		user(2, 1, "huge "+strings.Repeat("x", 1100000)),
		user(3, 2, "after"),
	}, 3, nil)
	p := convo.NewDevin()
	dvFeed(t, p, dvRun(t, db, "tail", "200"))
	want := []string{"user text:before", "note trouble:big", "user text:after"}
	if got := dvSaid(p); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("read:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	es := p.Since(0)
	tr := es[1].Parts[0].Trouble
	if tr == nil || tr.Bytes < 1<<20 {
		t.Fatalf("the large message's note: %+v", tr)
	}
}

func TestDevinQueryNoSession(t *testing.T) {
	db := filepath.Join(t.TempDir(), "sessions.db")
	dvMake(t, db, []dvNode{sys0(0, -1)}, 0, nil)
	out, err := exec.Command("sh", "-c", dvQueryCommand(db, "tail", "no-such-one", "200")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if r, tip, _, ok := dvRows(out); !ok || tip != 0 || len(r.lines) != 0 {
		t.Errorf("none: %s", out)
	}
	// (No database there: none, not an error.)
	out, err = exec.Command("sh", "-c", dvQueryCommand(filepath.Join(t.TempDir(), "x", "sessions.db"), "tail", dvSID, "200")).Output()
	if err != nil || !strings.Contains(string(out), `"k":"none"`) {
		t.Errorf("no database: %v %s", err, out)
	}
}

func TestDevinLivePanelKeyRefused(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	hd := newFakeHerdr(t)
	pane := map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "t1", "agent": "devin", "agent_status": "working", "cwd": "/w"}
	hd.setSnapshot(nil, []any{pane})
	_, id := herdrReplyEnv(t, h, admin, hd)
	eventually(t, "herdr did not report the Devin pane", func() bool {
		p, ok := h.srv.herdrSocks.paneOf(id, "w1:p1")
		return ok && p.Agent == "devin"
	})
	r := admin.do("POST", "/api/sessions/"+id+"/conversation/panel", map[string]any{
		"pane": "w1:p1", "pid": 100, "start": 200, "session": sidA,
		"keys": []string{"a"}, "live": true,
	})
	mustStatus(t, r, 409, "a key to Devin's live panel")
	if r.code() != "unsupported" {
		t.Fatalf("answer: %s", r.Body)
	}
	if typed := hd.typed(); len(typed) != 0 {
		t.Fatalf("typed into Devin's pane: %v", typed)
	}
}

func TestDevinSessionIDs(t *testing.T) {
	for _, ok := range []string{"cuddly-dime", "voracious-floor", "a-b", "abiding-chameleon"} {
		if !devinSessionID.MatchString(ok) {
			t.Errorf("%q not taken", ok)
		}
	}
	for _, bad := range []string{"", "nohyphen", "../x-y", "a-b;rm", "A-b", "-a-b", "a--b", strings.Repeat("a", 40) + "-b", "00000000-0000-0000-0000-000000000000"} {
		if devinSessionID.MatchString(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}

func TestParseConvoLocateDevin(t *testing.T) {
	db := "/home/u/.local/share/devin/cli/sessions.db"
	out := convoRec("pid=300", "start=50", "fg=1", "procstart=50", "sid=cuddly-dime", "log="+db, "agent=devin", "tty=/dev/pts/2", "anc=90:zsh,80:herdr,", "HERDR_PANE_ID=w1:p2", "HERDR_SOCKET_PATH=/s") +
		// (Not its session yet: the zero id, its database the default one.)
		convoRec("pid=301", "start=60", "procstart=60", "sid=00000000-0000-0000-0000-000000000000", "log="+db, "agent=devin") +
		// (A database elsewhere is not read; a session named otherwise is no Devin's.)
		convoRec("pid=302", "start=70", "procstart=70", "sid=brave-otter", "log=/etc/passwd", "agent=devin") +
		convoRec("pid=303", "start=80", "procstart=80", "sid=../x", "log="+db, "agent=devin") +
		// (A Claude Code's session is never named in words.)
		convoRec("pid=304", "start=90", "procstart=90", "sid=brave-otter", "log=/home/u/.claude/projects/-w/brave-otter.jsonl")
	l := parseConvoLocate(out)
	if len(l.procs) != 3 {
		t.Fatalf("procs: %+v", l.procs)
	}
	if p := l.procs[0]; p.agent != "devin" || p.sid != "cuddly-dime" || p.log != db || p.herdrPane != "w1:p2" || !p.fg {
		t.Errorf("devin: %+v", p)
	}
	if p := l.procs[1]; p.agent != "devin" || p.sid != convoNoSession || p.log != db {
		t.Errorf("devin, no session yet: %+v", p)
	}
	if p := l.procs[2]; p.log != "" {
		t.Errorf("devin, another database: %+v", p)
	}
}

func TestDevinSessionOf(t *testing.T) {
	s := &Server{}
	p := &convoProc{agent: "devin", sid: convoNoSession, log: "/home/u/.local/share/devin/cli/sessions.db"}
	s.devinSessionOf(&term.Session{Persistence: "tmux"}, p, "")
	if p.sid != convoNoSession || p.log != "" {
		t.Errorf("none told: %+v", p)
	}
	p = &convoProc{agent: "devin", sid: "cuddly-dime", log: "/d/devin/cli/sessions.db"}
	s.devinSessionOf(&term.Session{Persistence: "tmux"}, p, "")
	if p.sid != "cuddly-dime" || p.log == "" {
		t.Errorf("its own: %+v", p)
	}
}

// The scan finds an interactive Devin by the lock its agent's child holds,
// not a "devin -p" nor its child; one whose child holds none yet is at no
// session (the zero id).
func TestConvoLocateDevinProcesses(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "devin", "cli")
	if err := os.MkdirAll(filepath.Join(data, "session_locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "sessions.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// (A program named devin, built here — a script's command line would
	// begin with its interpreter: with "acp", its agent, holding the lock
	// of the session in DV_SESSION (none: no lock) and the database; else
	// the TUI, which starts one and waits.)
	src := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(src, []byte(dvFakeMain), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "devin")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Skipf("no fake devin built: %v %s", err, out)
	}
	start := func(session, pane string, args ...string) int {
		c := exec.Command(bin, args...)
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "DV_SESSION=" + session, "HERDR_PANE_ID=" + pane, "HERDR_SOCKET_PATH=/s"}
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		pid := c.Process.Pid
		t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL); c.Wait() })
		return pid
	}
	tui := start("cuddly-dime", "w1:p1", "--model", "swe-1", "-r", "cuddly-dime")
	fresh := start("", "w1:p2")
	printed := start("other-one", "w1:p3", "-p", "hi")
	// (Its child started, the lock taken.)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(data, "session_locks", "cuddly-dime.lock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no lock taken")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	c := exec.Command("sh", "-c", convoLocateCommand("herdr", "", ""))
	c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	by := map[int64]convoProc{}
	self := int64(os.Getpid())
	for _, p := range parseConvoLocate(string(out)).procs {
		// (Only this test's: a real Devin on the host is found too, and so is
		// another test binary's fake one when two runs share the machine; the
		// log path alone does not tell them apart, this process among its
		// ancestors does.)
		if p.agent != "devin" || !strings.HasPrefix(p.log, home+"/") {
			continue
		}
		for _, a := range p.anc {
			if a.pid == self {
				by[p.pid] = p
				break
			}
		}
	}
	if p, ok := by[int64(tui)]; !ok || p.sid != "cuddly-dime" || !strings.HasSuffix(p.log, "/devin/cli/sessions.db") || p.herdrPane != "w1:p1" {
		t.Errorf("the TUI: %v %+v", ok, p)
	}
	if p, ok := by[int64(fresh)]; !ok || p.sid != convoNoSession || p.log != filepath.Join(data, "sessions.db") {
		t.Errorf("no session yet: %v %+v", ok, p)
	}
	if _, ok := by[int64(printed)]; ok {
		t.Errorf("devin -p taken for an agent")
	}
	if len(by) != 2 {
		t.Errorf("found %d Devins, want 2 (no child taken for one): %+v", len(by), by)
	}
}

const dvFakeMain = `package main

import (
	"os"
	"os/exec"
	"time"
)

func main() {
	d := os.Getenv("HOME") + "/.local/share/devin/cli/"
	if len(os.Args) > 1 && os.Args[1] == "acp" {
		if s := os.Getenv("DV_SESSION"); s != "" {
			f, _ := os.Create(d + "session_locks/" + s + ".lock")
			defer f.Close()
		}
		db, _ := os.Open(d + "sessions.db")
		defer db.Close()
		time.Sleep(60 * time.Second)
		return
	}
	c := exec.Command(os.Args[0], "acp")
	c.Start()
	c.Wait()
}
`

func TestAgentSessionsSearchDevin(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "devin", "cli")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	dvMake(t, filepath.Join(data, "sessions.db"), []dvNode{sys0(0, -1), user(1, 0, "tidy the build")}, 1, map[string]any{"cwd": "/w/proj", "title": "Tidy the build", "prompts": []string{"tidy the build"}})
	c := exec.Command("sh", "-c", agentSessionsSearchCommand("tidy", "devin", ""))
	c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := parseAgentSessions(out)
	if len(got) != 1 || got[0].ID != dvSID || got[0].Kind != "devin" || got[0].Dir != "/w/proj" || got[0].Title != "Tidy the build" || got[0].First != "tidy the build" || got[0].Modified <= 0 {
		t.Fatalf("%s → %+v", out, got)
	}
}

func TestAgentArgsDevin(t *testing.T) {
	if a := agentArgs("devin", agentStart{resume: "cuddly-dime"}); strings.Join(a, " ") != "-r cuddly-dime" {
		t.Errorf("resume: %q", a)
	}
	if a := agentArgs("devin", agentStart{}); len(a) != 0 {
		t.Errorf("new: %q", a)
	}
}

// A Devin run inside a Claude Code pane has its session reported (by
// Claude Code's hook for herdr, which Devin runs) as that pane's: not
// taken as Claude Code's.
func TestHerdrSessionNotClaudeShaped(t *testing.T) {
	pane := func(agent, source, by, v string) herdr.Pane {
		return herdr.Pane{ID: "w1:p1", Agent: agent, AgentSession: &herdr.AgentSessionInfo{Source: source, Agent: by, Kind: "id", Value: v}}
	}
	uuid := "0b6a3c1e-2f4d-4e5a-9b7c-1d2e3f4a5b6c"
	for _, c := range []struct {
		p    herdr.Pane
		want string
	}{
		{pane("claude", "herdr:claude", "claude", "cuddly-dime"), ""},
		{pane("claude", "herdr:claude", "claude", uuid), uuid},
		{pane("", "herdr:claude", "claude", "cuddly-dime"), ""},
		{pane("devin", "herdr:devin", "devin", "brave-otter"), "brave-otter"},
		{pane("opencode", "herdr:opencode", "opencode", "ses_abc"), "ses_abc"},
	} {
		if got := agentSession(c.p); got != c.want {
			t.Errorf("%+v: %q, want %q", *c.p.AgentSession, got, c.want)
		}
	}
}
