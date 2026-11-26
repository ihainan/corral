package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A command a Claude Code set going in the background is named by what the
// agent said it does (its call's description), read from the session's log
// by the call whose result names the task: the script itself, on a home of
// its own, with a "claude" whose child writes to the task's file (11-26).
func TestCmdScanDescription(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-proj")
	tasks := filepath.Join(home, "tmp", "claude-1000", "-home-u-proj", cmdSess, "tasks")
	for _, d := range []string{proj, tasks, filepath.Join(home, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01A","name":"Bash","input":{"command":"cd /w && codex-review -o r.txt \"审查\"","description":"Run Codex review of \"fix 4\"","run_in_background":true}},{"type":"tool_use","id":"toolu_01B","name":"Bash","input":{"command":"ls","description":"List files"}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"toolu_01A","type":"tool_result","content":"Command running in background with ID: bdesc0001."}]},"toolUseResult":{"backgroundTaskId":"bdesc0001"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_02N","name":"Bash","input":{"command":"sleep 31","run_in_background":true}},{"id":"toolu_02D","name":"Bash","input":{"command":"ls","description":"Not this one"},"type":"tool_use"}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"toolu_02N","type":"tool_result","content":"Command running in background with ID: bnone0002."}]},"toolUseResult":{"backgroundTaskId":"bnone0002"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(proj, cmdSess+".jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	claude := filepath.Join(home, "bin", "claude")
	if err := os.Symlink(sh, claude); err != nil {
		t.Fatal(err)
	}
	fake := exec.Command(claude, "-c", `sleep 30 > "$1" & sleep 31 > "$2" & wait`, "claude", filepath.Join(tasks, "bdesc0001.output"), filepath.Join(tasks, "bnone0002.output"))
	if err := fake.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fake.Process.Kill(); _ = fake.Wait() }()
	var got []bgCmd
	for i := 0; i < 40 && len(got) < 2; i++ {
		time.Sleep(50 * time.Millisecond)
		cmd := exec.Command("sh", "-c", cmdScanCommand("s:"+cmdSess))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		all, _ := parseCmdScan(string(out))
		got = all[cmdSess]
		for _, c := range got {
			if (c.Task == "bdesc0001" && c.Desc != `Run Codex review of "fix 4"`) || (c.Task == "bnone0002" && c.Desc != "") {
				t.Fatalf("got %+v\n%s", got, out)
			}
		}
	}
	if len(got) != 2 {
		t.Fatalf("not found: %+v", got)
	}
	// (Of a line that says none, or one cut short: none.)
	if c, _ := parseCmdScan("clock\t1\t100\ncmd\t9\t5\t/t/" + cmdSess + "/tasks/b9.output\techo\ndesc\t" + cmdSess + "\tb9\t{\"description\":\"cut sho\n"); c[cmdSess][0].Desc != "" {
		t.Errorf("cut short: %+v", c)
	}
}
