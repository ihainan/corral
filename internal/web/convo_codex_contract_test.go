package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
)

// Invoked by scripts/codex_contract.py on logs from that exact real-CLI
// run, not on personal sessions. Missing or empty evidence fails the run.
func TestCodexContractArtifacts(t *testing.T) {
	dir := os.Getenv("CORRAL_CODEX_CONTRACT_DIR")
	if dir == "" {
		t.Skip("run make codex-contract to produce isolated real-CLI evidence")
	}
	files, err := filepath.Glob(filepath.Join(dir, "rollout-*.jsonl"))
	if err != nil || len(files) < 3 {
		t.Fatalf("expected initial, new and rewind rollouts, got %d: %v", len(files), err)
	}
	screen, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "model-picker-open.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if picker := parseCodexPicker(string(screen)); !picker.ok || picker.effort || len(picker.options) == 0 {
		t.Fatal("Corral did not recognize the installed CLI's model picker")
	}
	sawImage, sawSegment, sawMultiline, sawSettings := false, false, false, false
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(bytes.SplitN(data, []byte{'\n'}, 2)[0], &head) != nil || head.Type != "session_meta" || !convoUUID.MatchString(head.Payload.ID) {
			t.Fatalf("missing session metadata in %s", filepath.Base(file))
		}
		log := "/home/fixture/.codex/sessions/2026/11/12/" + filepath.Base(file)
		if convoSessionOf(log) != head.Payload.ID || !convoLogOf("codex", log, head.Payload.ID) {
			t.Errorf("real rollout not bound to its session: %s", filepath.Base(file))
		}
		parser := convo.NewCodex()
		parser.Feed(data)
		if m := parser.Meta(); m.Model == "gpt-6-sol" && m.Effort == "high" {
			sawSettings = true
		}
		for _, entry := range parser.Since(0) {
			if entry.Role != convo.RoleUser {
				continue
			}
			for _, part := range entry.Parts {
				if part.Kind == convo.KindImage && strings.HasSuffix(part.Path, "/image.png") {
					sawImage = true
				}
				if part.Text == "#multiline 中文  连续空格\n第二行" {
					sawMultiline = true
				}
			}
		}
		if strings.Contains(filepath.Base(file), "_") {
			st := &convoState{agent: "codex", log: log, parser: parser}
			sawSegment = st.meta().HistoryPartial
		}
	}
	if !sawImage || !sawSegment || !sawMultiline || !sawSettings {
		t.Fatalf("real log coverage: image=%t rewind=%t multiline=%t model-and-effort=%t", sawImage, sawSegment, sawMultiline, sawSettings)
	}
}

// This operates only on the dedicated tmux server created by the contract
// runner. The production picker flow reads and drives the real CLI here;
// transport-level SSH gates are covered separately.
func TestCodexContractModelFlow(t *testing.T) {
	socket := os.Getenv("CORRAL_CODEX_CONTRACT_TMUX")
	if socket == "" {
		t.Skip("run make codex-contract for a dedicated real CLI terminal")
	}
	if !strings.HasPrefix(socket, "corral-contract-") || strings.ContainsAny(socket, "/\n\x00") {
		t.Fatal("not a dedicated contract terminal")
	}
	run := func(input string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", socket, "-f", "/dev/null"}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	look := func() (string, string, error) {
		screen, err := run("", "capture-pane", "-p", "-t", "contract", "-S", "-200")
		return screen, fmt.Sprintf("%x", sha256.Sum256([]byte(screen))), err
	}
	io := codexIO{look: look}
	io.press = func(keys []string, text, digest string) error {
		_, now, err := look()
		if err != nil {
			return err
		}
		if now != digest {
			return &sshx.ExitError{Status: 10}
		}
		if text != "" {
			if _, err := run(text, "load-buffer", "-b", "contract", "-"); err != nil {
				return err
			}
			if _, err := run("", "paste-buffer", "-p", "-d", "-b", "contract", "-t", "contract"); err != nil {
				return err
			}
			time.Sleep(300 * time.Millisecond)
			_, err := run("", "send-keys", "-t", "contract", "Enter")
			return err
		}
		args := []string{"send-keys", "-t", "contract"}
		for _, key := range keys {
			value, ok := modelKeys[key]
			if !ok {
				t.Fatalf("unexpected key %q", key)
			}
			args = append(args, value[1])
		}
		_, err = run("", args...)
		return err
	}
	list, err := codexModelFlow(io, io, "", "")
	if err != nil || len(list) < 9 {
		t.Fatalf("real full model list: %d models, %v", len(list), err)
	}
	if _, err := codexModelFlow(io, io, "GPT-6-Sol", "High"); err != nil {
		t.Fatalf("real session-only model switch: %v", err)
	}
}
