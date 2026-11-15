package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A folder's Codex conversations, from a database as Codex 0.167 keeps it:
// the person's own, newest first, its name else its title; not a
// subagent's, nor an archived one, nor another folder's.
func TestCodexSessionsList(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	home := t.TempDir()
	db := filepath.Join(home, "state_5.sqlite")
	mk := `import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
c.execute("create table threads (id text, name text, title text, first_user_message text, cwd text, source text, thread_source text, archived int, updated_at int)")
rows=[("01a1df42-8f07-7d43-b8b7-c6bad45f5528","fix tests","t1","first one","/w/p","cli","user",0,200),
("01a1df42-0000-7d43-b8b7-c6bad45f5528",None,"the title","second","/w/p","vscode","user",0,300),
("01a1df42-1111-7d43-b8b7-c6bad45f5528",None,"sub","x","/w/p","cli","subagent",0,400),
("01a1df42-2222-7d43-b8b7-c6bad45f5528",None,"old","x","/w/p","cli","user",1,500),
("01a1df42-3333-7d43-b8b7-c6bad45f5528",None,"other","x","/w/q","cli","user",0,600)]
c.executemany("insert into threads values (?,?,?,?,?,?,?,?,?)",rows)
c.commit()`
	if out, err := exec.Command("python3", "-c", mk, db).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	cmd := exec.Command("sh", "-c", codexSessionsCommand("/w/p/"))
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := parseCodexSessions(out)
	if len(got) != 2 || got[0].ID != "01a1df42-0000-7d43-b8b7-c6bad45f5528" || got[0].Title != "the title" || got[0].Modified != 300000 || got[1].Title != "fix tests" || got[1].First != "first one" {
		t.Fatalf("%+v (%s)", got, out)
	}
}

// The command a Codex taken up is started with: codex resume <id>.
func TestAgentStartCodexResume(t *testing.T) {
	cmd := agentStartCmd("/s", "w1:p1", "fix", "codex", agentStart{resume: "01a1df42-8f07-7d43-b8b7-c6bad45f5528"})
	if !strings.Contains(cmd, " -- 'resume' '01a1df42-8f07-7d43-b8b7-c6bad45f5528'") {
		t.Fatalf("%s", cmd)
	}
}
