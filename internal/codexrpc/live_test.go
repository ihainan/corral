package codexrpc

// This opt-in test starts its own Codex with an empty HOME and a loopback
// model fixture. It never connects to a user's daemon or provider account.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsolatedCodexStreaming(t *testing.T) {
	binary := os.Getenv("CORRAL_CODEX_BINARY")
	if binary == "" {
		t.Skip("set CORRAL_CODEX_BINARY for an isolated real-engine test")
	}
	const answer = "Native stream: 逐段显示的本地测试文本。"
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			fmt.Fprint(w, `{"data":[{"id":"gpt-6.1-sol"}]}`)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, fields map[string]any) bool {
			fields["type"] = kind
			b, _ := json.Marshal(fields)
			_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
			w.(http.Flusher).Flush()
			return err == nil
		}
		item := map[string]any{"type": "message", "id": "local_answer", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer, "annotations": []any{}}}}
		event("response.created", map[string]any{"response": map[string]string{"id": "local_response"}})
		event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "local_answer", "role": "assistant", "status": "in_progress", "content": []any{}}})
		for _, part := range []string{"Native stream: ", "逐段", "显示的", "本地", "测试文本。"} {
			if !event("response.output_text.delta", map[string]any{"output_index": 0, "item_id": "local_answer", "content_index": 0, "delta": part}) {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(80 * time.Millisecond):
			}
		}
		event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		event("response.completed", map[string]any{"response": map[string]any{"id": "local_response", "usage": map[string]int{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}})
	}))
	defer model.Close()
	dir := t.TempDir()
	home := filepath.Join(dir, ".codex")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model = \"gpt-6.1-sol\"\nmodel_provider = \"fixture\"\nmodel_reasoning_effort = \"high\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\ncheck_for_update_on_startup = false\n[model_providers.fixture]\nname = \"Local fixture\"\nbase_url = %q\nenv_key = \"CORRAL_FIXTURE_KEY\"\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\n", model.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "control.sock")
	log, err := os.Create(filepath.Join(dir, "app-server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "app-server", "--listen", "unix://"+socket)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "CORRAL_FIXTURE_KEY=fixture-only", "LANG=C.UTF-8"}
	cmd.Stderr = log
	cmd.Stdout = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	var first *Client
	for ctx.Err() == nil {
		first, err = Dial(ctx, "ws://codex.local/", &http.Client{Transport: transport})
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Millisecond):
		}
	}
	if err != nil {
		b, _ := os.ReadFile(log.Name())
		t.Fatalf("start: %v\n%s", err, b)
	}
	defer first.Close()
	second, err := Dial(ctx, "ws://codex.local/", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	type thread struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Effort string `json:"reasoningEffort"`
	}
	var a, b struct {
		Thread thread `json:"thread"`
	}
	if err = first.Call(ctx, "thread/start", map[string]any{"cwd": dir}, &a); err != nil {
		t.Fatal(err)
	}
	if err = first.Call(ctx, "thread/start", map[string]any{"cwd": dir}, &b); err != nil {
		t.Fatal(err)
	}
	if err = first.Call(ctx, "thread/settings/update", map[string]any{"threadId": a.Thread.ID, "effort": "low"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = first.Call(ctx, "thread/read", map[string]any{"threadId": a.Thread.ID, "includeTurns": false}, &a); err != nil {
		t.Fatal(err)
	}
	if err = first.Call(ctx, "thread/read", map[string]any{"threadId": b.Thread.ID, "includeTurns": false}, &b); err != nil {
		t.Fatal(err)
	}
	if a.Thread.Effort != "low" || b.Thread.Effort != "high" {
		t.Fatalf("settings crossed threads: %+v %+v", a, b)
	}
	// A new thread has no rollout until its first turn. Its creator already
	// owns a subscription and must not resume it before sending that turn.
	start := time.Now()
	if err = first.Call(ctx, "turn/start", map[string]any{"threadId": a.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Return the local fixture response."}}, "clientUserMessageId": "fixture-message"}, nil); err != nil {
		t.Fatal(err)
	}
	if err = second.Call(ctx, "thread/resume", map[string]any{"threadId": a.Thread.ID}, nil); err != nil {
		t.Fatalf("resume: %#v", err)
	}
	var cursor uint64
	text := ""
	deltas := 0
	firstDelta := time.Duration(0)
	for {
		events, seq, gap, changed := second.Events(cursor, a.Thread.ID)
		if gap {
			t.Fatal("unexpected gap")
		}
		cursor = seq
		done := false
		for _, e := range events {
			if e.Method == "item/agentMessage/delta" {
				var p struct {
					Delta string `json:"delta"`
				}
				_ = json.Unmarshal(e.Params, &p)
				text += p.Delta
				deltas++
				if firstDelta == 0 {
					firstDelta = time.Since(start)
				}
			}
			if e.Method == "turn/completed" {
				done = true
			}
		}
		if done {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("turn did not finish")
		}
	}
	if text != answer || deltas < 2 || calls.Load() != 1 {
		t.Fatalf("stream: %q deltas=%d model calls=%d", text, deltas, calls.Load())
	}
	var history struct {
		Data []struct {
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		} `json:"data"`
	}
	if err = second.Call(ctx, "thread/items/list", map[string]any{"threadId": a.Thread.ID, "limit": 40, "sortDirection": "desc"}, &history); err != nil {
		var rpc *RPCError
		if !errors.As(err, &rpc) || rpc.Code != -32601 {
			t.Fatal(err)
		}
		// Standalone app-server currently lacks the daemon's pagination API.
		// Its stream is still tested against the real engine; daemon read-only
		// pagination is covered separately by the deployed protocol probe.
		var full struct {
			Thread struct {
				Turns []struct {
					Items []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"items"`
				} `json:"turns"`
			} `json:"thread"`
		}
		if err = second.Call(ctx, "thread/read", map[string]any{"threadId": a.Thread.ID, "includeTurns": true}, &full); err != nil {
			t.Fatal(err)
		}
		for _, turn := range full.Thread.Turns {
			for _, item := range turn.Items {
				history.Data = append(history.Data, struct {
					Item struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"item"`
				}{Item: item})
			}
		}
		t.Log("standalone pagination unavailable; checked completed snapshot via thread/read")
	}
	found := false
	for _, r := range history.Data {
		if r.Item.Type == "agentMessage" && r.Item.Text == answer {
			found = true
		}
	}
	if !found {
		t.Fatal("stream missing from paginated history")
	}
	if saved, _ := os.ReadFile(filepath.Join(home, "config.toml")); string(saved) != config {
		t.Fatal("global config changed")
	}
	if !strings.HasPrefix(a.Thread.ID, "01") {
		t.Fatalf("unexpected engine thread id: %q", a.Thread.ID)
	}
	t.Logf("real engine, two clients: first delta %s, completed %s, %d deltas, exactly one local model call", firstDelta, time.Since(start), deltas)
}
