package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihainan/corral/internal/convo"
)

// ocV2DB makes an OpenCode 2 database (its tables as 2.2.6's, the rows
// shaped as its own were, 11-08): one session, the rows given (type, id,
// seq, created, updated, data), its inbox, whether it is at work, what
// /undo went back to. Its path.
func ocV2DB(t *testing.T, rows [][]any, inbox [][]any, busy bool, revert string) string {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	db := filepath.Join(t.TempDir(), "opencode.db")
	script := `import sqlite3, sys, json
c = sqlite3.connect(sys.argv[1])
c.execute('create table session (id text, directory text, parent_id text, title text, revert text, time_updated int)')
c.execute('create table message (id text, session_id text, time_created int, time_updated int, data text)')
c.execute('create table part (id text, message_id text, session_id text, time_created int, time_updated int, data text)')
c.execute('create table session_v2 (id text, parent_id text, directory text, title text, revert text, time_created int, time_updated int, time_idle int, time_busy int)')
c.execute('create table session_message (id text, session_id text, type text, seq int, time_created int, time_updated int, data text)')
c.execute('create table session_inbox (id text, session_id text, type text, payload text, delivery text, enqueued_seq int, time_created int)')
a = json.loads(sys.argv[2])
c.execute('insert into session_v2 values (?,?,?,?,?,?,?,?,?)', ('ses_bbbbbbbbbb', None, '/w', 'Greeting', a['revert'] or None, 100, 900, 50 if a['busy'] else 800, 700 if a['busy'] else 40))
c.execute('insert into session_v2 values (?,?,?,?,?,?,?,?,?)', ('ses_bbbbbbbbsub', 'ses_bbbbbbbbbb', '/w', 'Child', None, 100, 950, 0, 0))
c.execute('insert into session values (?,?,?,?,?,?)', ('ses_oldoldoldold', '/w', None, 'an old one', None, 500))
for r in a['rows']:
    c.execute('insert into session_message values (?,?,?,?,?,?,?)', (r[1], 'ses_bbbbbbbbbb', r[0], r[2], r[3], r[4], json.dumps(r[5])))
for q in a['inbox'] or []:
    c.execute('insert into session_inbox values (?,?,?,?,?,?,?)', (q[0], 'ses_bbbbbbbbbb', 'user', json.dumps({'text': q[1], 'files': [], 'agents': []}), 'steer', 1, q[2]))
c.commit()
`
	arg, _ := json.Marshal(map[string]any{"rows": rows, "inbox": inbox, "busy": busy, "revert": revert})
	if out, err := exec.Command("python3", "-c", script, db, string(arg)).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return db
}

func ocV2Run(t *testing.T, db, mode string, args ...string) []byte {
	out, err := exec.Command("sh", "-c", ocQueryCommand(db, mode, append([]string{"ses_bbbbbbbbbb"}, args...)...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ocFeedRows: of what the query printed, the rows the parser is fed.
func ocFeedRows(out []byte) []byte {
	var b strings.Builder
	for _, l := range strings.Split(string(out), "\n") {
		var r struct{ K string }
		if json.Unmarshal([]byte(l), &r) == nil && strings.Contains(" s m p ids q ", " "+r.K+" ") {
			b.WriteString(l + "\n")
		}
	}
	return []byte(b.String())
}

func ocSaid(p *convo.OpenCode) []string {
	var s []string
	for _, e := range p.Since(0) {
		var w []string
		for _, x := range e.Parts {
			switch {
			case x.Tool != nil:
				w = append(w, x.Tool.Name+"("+x.Tool.Detail+")")
			case x.Text != "":
				w = append(w, string(x.Kind)+":"+x.Text)
			case x.Trouble != nil:
				w = append(w, "trouble:"+x.Trouble.Text)
			}
		}
		g := ""
		if e.Gone {
			g = " (gone)"
		}
		s = append(s, string(e.Role)+": "+strings.Join(w, ", ")+g)
	}
	return s
}

var ocV2Talk = [][]any{
	{"user", "msg_b0000000001", 4, 110, 111, map[string]any{"time": map[string]any{"created": 110}, "text": "run it", "files": []any{}, "agents": []any{}}},
	{"assistant", "msg_b0000000002", 5, 120, 160, map[string]any{"time": map[string]any{"created": 120, "streamed": 150, "completed": 159}, "agent": "build",
		"model": map[string]any{"id": "deepseek-v4.1-flash", "providerID": "opencode-go", "variant": "high"}, "finish": "tool-calls",
		"tokens": map[string]any{"input": 2000, "output": 40, "reasoning": 0, "cache": map[string]any{"read": 1000, "write": 0}},
		"content": []any{
			map[string]any{"type": "reasoning", "text": "Run the echo."},
			map[string]any{"type": "tool", "id": "call_00_x", "name": "shell", "executed": false,
				"state": map[string]any{"status": "completed", "input": map[string]any{"command": "echo probe-ran", "description": "Echo a word"},
					"content":  []any{map[string]any{"type": "text", "text": "probe-ran\n"}, map[string]any{"type": "text", "text": "There is no \"bash\" tool, so this ran as \"shell\"."}},
					"metadata": map[string]any{"output": "probe-ran\n", "exit": 0, "description": "Echo a word", "truncated": false}}},
			map[string]any{"type": "tool", "id": "call_01_y", "name": "write", "state": map[string]any{"status": "completed", "input": map[string]any{"path": "/w/a.txt", "content": "hi\n"}, "metadata": map[string]any{"truncated": false}}},
			map[string]any{"type": "tool", "id": "call_02_z", "name": "edit", "state": map[string]any{"status": "error", "input": map[string]any{"filePath": "/w/b.txt", "oldString": "a", "newString": "b"},
				"error": map[string]any{"type": "tool.execution", "message": "Found multiple matches for oldString."}}},
		}}},
	{"idle", "msg_b0000000003", 10, 161, 161, map[string]any{"time": map[string]any{"created": 161}, "outcome": "succeeded"}},
	{"system", "msg_b0000000004", 11, 170, 170, map[string]any{"text": "Instructions from: /w", "metadata": map[string]any{"notice": "instructions"}}},
	{"assistant", "msg_b0000000005", 12, 180, 190, map[string]any{"time": map[string]any{"created": 180, "completed": 190}, "model": map[string]any{"id": "deepseek-v4.1-flash"},
		"content": []any{map[string]any{"type": "text", "text": "Long answer "}}, "finish": "error", "error": map[string]any{"type": "aborted", "message": "Step interrupted"}}},
	{"assistant", "msg_b0000000006", 13, 200, 210, map[string]any{"time": map[string]any{"created": 200, "completed": 210}, "model": map[string]any{"id": "deepseek-v4.1-flash"}, "content": []any{},
		"error": map[string]any{"type": "provider.error", "message": "Upstream request failed: Model is unavailable."}}},
}

// OpenCode 2's session read as 1's was: its steps, its tools by Claude
// Code's names (shell, a file's path), what went wrong; its notices and
// rests left out; whether it is at work by its session.
func TestOpenCode2Read(t *testing.T) {
	db := ocV2DB(t, ocV2Talk, nil, false, "")
	p := convo.NewOpenCode()
	p.Feed(ocFeedRows(ocV2Run(t, db, "tail", "100")))
	got := strings.Join(ocSaid(p), "\n")
	want := strings.Join([]string{
		"user: text:run it",
		"assistant: thinking:Run the echo., Bash(echo probe-ran), Write(/w/a.txt), Edit(/w/b.txt)",
		"assistant: text:Long answer ",
		"assistant: trouble:Upstream request failed: Model is unavailable.",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// (Its status line's "Build · … · high": the agent it ran as and its variant, kept from the step that said them.)
	if m := p.Meta(); m.Mode != "build" || m.Effort != "high" {
		t.Errorf("agent and variant: %+v", m)
	}
	if p.Skipped != 0 || p.Title() != "Greeting" || p.Meta().Model != "deepseek-v4.1-flash" || p.Status() != "idle" {
		t.Fatalf("skipped %d, title %q, model %q, status %q", p.Skipped, p.Title(), p.Meta().Model, p.Status())
	}
	for _, e := range p.Since(0) {
		for _, x := range e.Parts {
			if x.Tool != nil && x.Tool.Name == "Bash" && (strings.TrimSpace(x.Tool.Result) != "probe-ran" || x.Tool.Error) {
				t.Fatalf("shell's result: %q (error %v)", x.Tool.Result, x.Tool.Error)
			}
			if x.Tool != nil && x.Tool.Name == "Edit" && (!x.Tool.Error || !strings.Contains(x.Tool.Result, "multiple matches")) {
				t.Fatalf("edit's error: %q (error %v)", x.Tool.Result, x.Tool.Error)
			}
		}
	}
}

// At work by its session — between its steps too, when the last step has
// ended — and what was sent meanwhile in its queue, by its own id.
func TestOpenCode2BusyAndQueue(t *testing.T) {
	db := ocV2DB(t, ocV2Talk[:2], [][]any{{"msg_b0000000009", "while it works", 175}}, true, "")
	p := convo.NewOpenCode()
	p.Feed(ocFeedRows(ocV2Run(t, db, "tail", "100")))
	if p.Status() != "busy" {
		t.Fatalf("status %q", p.Status())
	}
	q := p.Queued()
	if len(q) != 1 || q[0].Text != "while it works" || q[0].ID != "msg_b0000000009" || q[0].Time == "" {
		t.Fatalf("queued %+v", q)
	}
	// (Taken in: the queue read empty again, and at rest.)
	db = ocV2DB(t, ocV2Talk[:2], nil, false, "")
	p.Feed(ocFeedRows(ocV2Run(t, db, "since", "0", "0", "")))
	if len(p.Queued()) != 0 || p.Status() != "idle" {
		t.Fatalf("queued %+v, status %q", p.Queued(), p.Status())
	}
}

// /undo by where a message is in the conversation, not by its id: one sent
// while it worked keeps the id it was given then, and is taken in after an
// answer made since.
func TestOpenCode2UndoByOrder(t *testing.T) {
	rows := [][]any{
		{"user", "msg_c0000000001", 4, 100, 100, map[string]any{"text": "first"}},
		{"assistant", "msg_c0000000005", 5, 110, 120, map[string]any{"time": map[string]any{"created": 110, "completed": 120}, "content": []any{map[string]any{"type": "text", "text": "answer made before it was taken in"}}}},
		{"user", "msg_c0000000003", 6, 130, 130, map[string]any{"text": "sent while it worked"}},
		{"assistant", "msg_c0000000007", 7, 140, 150, map[string]any{"time": map[string]any{"created": 140, "completed": 150}, "content": []any{map[string]any{"type": "text", "text": "its answer"}}}},
	}
	db := ocV2DB(t, rows, nil, false, `{"messageID":"msg_c0000000003","files":[]}`)
	p := convo.NewOpenCode()
	p.Feed(ocFeedRows(ocV2Run(t, db, "tail", "100")))
	if g := ocGone(p); g != "msg_c0000000003 msg_c0000000007" {
		t.Fatalf("gone %q", g)
	}
}

// A message written over with fewer pieces than it had: those past its count
// are no longer shown.
func TestOpenCode2Rewritten(t *testing.T) {
	two := [][]any{{"assistant", "msg_d0000000001", 5, 100, 110, map[string]any{"time": map[string]any{"created": 100}, "content": []any{
		map[string]any{"type": "text", "text": "draft one"}, map[string]any{"type": "text", "text": "draft two"}}}}}
	one := [][]any{{"assistant", "msg_d0000000001", 5, 100, 120, map[string]any{"time": map[string]any{"created": 100, "completed": 120}, "content": []any{
		map[string]any{"type": "text", "text": "final"}}}}}
	p := convo.NewOpenCode()
	p.Feed(ocFeedRows(ocV2Run(t, ocV2DB(t, two, nil, true, ""), "tail", "100")))
	p.Feed(ocFeedRows(ocV2Run(t, ocV2DB(t, one, nil, false, ""), "since", "110", "0", "")))
	if got := strings.Join(ocSaid(p), "\n"); got != "assistant: text:final" {
		t.Fatalf("got %q", got)
	}
	// (In the same millisecond, the message's own words the same: still told.)
	same := [][]any{{"assistant", "msg_d0000000001", 5, 100, 110, map[string]any{"time": map[string]any{"created": 100}, "content": []any{
		map[string]any{"type": "text", "text": "draft one"}}}}}
	p = convo.NewOpenCode()
	p.Feed(ocFeedRows(ocV2Run(t, ocV2DB(t, two, nil, true, ""), "tail", "100")))
	p.Feed(ocFeedRows(ocV2Run(t, ocV2DB(t, same, nil, true, ""), "since", "110", "0", "")))
	if got := strings.Join(ocSaid(p), "\n"); got != "assistant: text:draft one" {
		t.Fatalf("same millisecond: got %q", got)
	}
	// (Holding nothing now: nothing of what it held is shown.)
	none := [][]any{{"assistant", "msg_d0000000001", 5, 100, 130, map[string]any{"time": map[string]any{"created": 100}, "content": []any{}}}}
	p.Feed(ocFeedRows(ocV2Run(t, ocV2DB(t, none, nil, true, ""), "since", "110", "0", "")))
	if got := strings.Join(ocSaid(p), "\n"); got != "assistant: " {
		t.Fatalf("emptied: got %q", got)
	}
}

// Where it runs: the latest top-level session of 2's (not a subagent's),
// before an older one of 1's in the same database.
func TestOpenCode2Find(t *testing.T) {
	db := ocV2DB(t, ocV2Talk, nil, false, "")
	out, err := exec.Command("sh", "-c", ocQueryCommand(db, "find", "/w", "0")).Output()
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ ID string }
	if json.Unmarshal(out, &r) != nil || r.ID != "ses_bbbbbbbbbb" {
		t.Fatalf("found %s", out)
	}
}

// OpenCode 2's input on its screen (as 2.2.6 draws it, 11-08): there and
// empty; words in it; a permission or a question in its place; neither.
func TestOcIdleInput(t *testing.T) {
	idle := "     Build · Mock One · 8.5s · 9.9 tok/s\n\n\n  ┃\n  ┃\n  ┃\n  ┃  Build · Mock One Mock\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n  ~/proj                                1.0K (1%)  ctrl+p commands\n\n"
	first := "  ┃\n  ┃  Ask anything… \"What is the tech stack of this project?\"\n  ┃\n  ┃  Build · Mock One Mock\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n  ~/proj       shift+tab agents  ctrl+p commands\n                                                                  2.2.6\n"
	busy := "  ┃  second while busy\n  ┃\n\n\n  ┃\n  ┃\n  ┃\n  ┃  Build · Mock One Mock\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n   ⬝⬝■■■■■■ esc interrupt          1.0K (1%)  ctrl+p commands\n"
	// (A sidebar beside it, wider than its box: not its.)
	wide := "  ┃" + strings.Repeat(" ", 62) + "Context\n  ┃" + strings.Repeat(" ", 62) + "1,020 tokens\n  ┃\n  ┃  Build · Mock One Mock\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	// (Its placeholder cut off in a narrow pane.)
	narrow := "  ┃\n  ┃  Ask anything… \"What is the te\n  ┃\n  ┃  Build · Mock\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n"
	draft := "  ┃  1 message reverted\n  ┃  ctrl+x r or /redo to restore\n\n  ┃\n  ┃  do a command\n  ┃\n  ┃  Build · Mock One Mock\n  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n  ~/proj\n"
	perm := "  ┃\n  ┃  △ Permission required\n  ┃\n  ┃  $ echo needs-ok\n  ┃\n  ┃   Allow once   Always allow   Reject        ctrl+f fullscreen  ⇆ select  enter confirm\n  ┃                                                  ~/proj\n"
	ask := "  ┃  Which database?\n  ┃  1. SQLite\n  ┃  2. Postgres\n  ┃  3. Type your own answer\n  ┃\n  ┃  ↑↓ select  enter submit  esc dismiss\n  ┃                                   ~/proj\n"
	for _, c := range []struct {
		name, screen string
		want         error
	}{{"idle", idle, nil}, {"first", first, nil}, {"busy", busy, nil}, {"wide", wide, nil}, {"narrow", narrow, nil}, {"narrower", strings.Replace(narrow, `Ask anything… "What is the te`, "Ask anything…", 1), nil},
		{"draft", draft, errDraft}, {"asks-anything draft", strings.Replace(idle, "  ┃\n  ┃\n  ┃\n", "  ┃\n  ┃  Ask anything about this repo\n  ┃\n", 1), errDraft},
		{"asks-anything… draft", strings.Replace(idle, "  ┃\n  ┃\n  ┃\n", "  ┃\n  ┃  Ask anything… about this repo\n  ┃\n", 1), errDraft},
		{"indented draft", strings.Replace(idle, "  ┃\n  ┃\n  ┃\n", "  ┃\n  ┃          secret\n  ┃\n", 1), errDraft},
		{"sidebar by an empty input", strings.Replace(idle, "  ┃\n  ┃\n  ┃\n", "  ┃\n  ┃"+strings.Repeat(" ", 62)+"Modified files\n  ┃\n", 1), nil}, {"permission", perm, errOcAsks}, {"question", ask, errOcAsks}, {"other", "$ \n", errScreen}} {
		if got := ocIdleInput(c.screen); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		// (The host's look before typing: its input's digest as the gateway's,
		// by every awk there is here; none where it is not there.)
		gw, boxed := ocBoxDigest([]byte(c.screen))
		for _, awk := range []string{"awk", "mawk", "gawk", "busybox awk"} {
			if _, err := exec.LookPath(strings.Fields(awk)[0]); err != nil {
				continue
			}
			pipe := strings.ReplaceAll(ocBoxPipe, "LC_ALL=C awk", "LC_ALL=C "+awk)
			cmd := exec.Command("sh", "-c", `b=$(`+pipe+`); [ -n "$b" ] || exit 0; printf '%s\n' "$b" | sha256sum`)
			cmd.Stdin = strings.NewReader(c.screen)
			out, err := cmd.Output()
			host, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
			if err != nil || host != gw || (host != "") != boxed {
				t.Errorf("%s, %s: host %q (%v), gateway %q", c.name, awk, host, err, gw)
			}
		}
	}
}
