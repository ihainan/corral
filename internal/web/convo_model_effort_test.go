package web

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The picker's effort line as Claude Code 2.1.323 draws it (testdata, read
// off a real one), wide and narrow; and the banner's "with high effort",
// which is no line to adjust.
func TestPickerEffort(t *testing.T) {
	for _, f := range []string{"testdata/model-picker-effort-120.txt", "testdata/model-picker-effort-47.txt"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := pickerEffort(string(b)); got != "medium" {
			t.Errorf("%s: %q", f, got)
		}
	}
	for in, want := range map[string]string{
		"  ◉ xHigh effort ←/→ to adjust":                       "xhigh",
		"  ◈ Max effort ←/→ to adjust\n  May use excessive":    "max",
		"  ○ Low effort ←/→\n  to adjust":                      "low", // (wrapped)
		"  ● High effort (default) ←/→ to adjust":              "high",
		"▝▜██████▀  Fable 5.1 with high effort · Claude Ultra": "",
		"": "",
	} {
		if got := pickerEffort(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// CORRAL_REAL_CLAUDE=<tmux socket>:<target> go test -run TestModelFlowReal -v:
// modelFlow on a real Claude Code in a tmux pane of its own (one with a
// CLAUDE_CONFIG_DIR of its own: see memory probe-real-cc-isolated-config),
// its keys and screen through tmux. Lists the models, switches to the
// second listed with xhigh effort, then back to the first with medium.
func TestModelFlowReal(t *testing.T) {
	spec := os.Getenv("CORRAL_REAL_CLAUDE")
	if spec == "" {
		t.Skip("CORRAL_REAL_CLAUDE not set")
	}
	sock, target, _ := strings.Cut(spec, ":")
	tmux := func(args ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", sock}, args...)...).Output()
		if err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
		return string(out)
	}
	step := func(keys []string, text string, read int) (string, error) {
		if text != "" {
			tmux("send-keys", "-t", target, "-l", text)
			time.Sleep(400 * time.Millisecond)
			tmux("send-keys", "-t", target, "Enter")
		}
		for _, k := range keys {
			tmux("send-keys", "-t", target, modelKeys[k][1])
			time.Sleep(120 * time.Millisecond)
		}
		time.Sleep(1200 * time.Millisecond)
		lines := strings.Split(strings.TrimRight(tmux("capture-pane", "-p", "-t", target), "\n"), "\n")
		if read > 0 && len(lines) > read {
			lines = lines[len(lines)-read:]
		}
		return strings.Join(lines, "\n"), nil
	}
	opts, err := modelFlow(step, step, "", "")
	if err != nil || len(opts) < 2 {
		t.Fatalf("list: %v %+v", err, opts)
	}
	t.Logf("offered: %+v", opts)
	if _, err := modelFlow(step, step, opts[1].Label, "xhigh"); err != nil {
		t.Fatalf("to %s with xhigh: %v", opts[1].Label, err)
	}
	scr, _ := step([]string{}, "", 0)
	if !strings.Contains(scr, "xhigh") {
		t.Fatalf("xhigh not said:\n%s", scr)
	}
	if _, err := modelFlow(step, step, opts[0].Label, "medium"); err != nil {
		t.Fatalf("back to %s with medium: %v", opts[0].Label, err)
	}
	scr, _ = step([]string{}, "", 0)
	t.Logf("last:\n%s", scr[max(0, len(scr)-400):])
}
