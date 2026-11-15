package convo

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Synthetic, credential-free logs. Measure both supported tool encodings;
// these numbers describe parsing, not SSH or browser responsiveness.
func codexBenchData(b *testing.B, code bool, size int) []byte {
	b.Helper()
	var data []byte
	row := func(kind string, payload any) {
		v, err := json.Marshal(map[string]any{"timestamp": "2026-11-12T00:00:00Z", "type": kind, "payload": payload})
		if err != nil {
			b.Fatal(err)
		}
		data = append(append(data, v...), '\n')
	}
	for i := 0; len(data) < size; i++ {
		id := fmt.Sprint(i)
		row("event_msg", map[string]any{"type": "task_started"})
		row("event_msg", map[string]any{"type": "user_message", "message": "检查这个函数并运行测试 " + id})
		if code {
			row("response_item", map[string]any{"type": "custom_tool_call", "call_id": id, "name": "exec", "input": `const r = await tools.exec_command({cmd:"go test ./..."}); text(r.output);`})
			row("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": id, "output": "ok example/project 0.1s"})
		} else {
			row("response_item", map[string]any{"type": "function_call", "call_id": id, "name": "exec_command", "arguments": `{"cmd":"go test ./..."}`})
			row("response_item", map[string]any{"type": "function_call_output", "call_id": id, "output": "Process exited with code 0\nOutput:\nok example/project 0.1s"})
		}
		row("response_item", map[string]any{"type": "message", "id": "m" + id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "测试通过。"}}})
		row("event_msg", map[string]any{"type": "task_complete"})
	}
	return data
}

func BenchmarkCodexFeed(b *testing.B) {
	for _, code := range []bool{false, true} {
		for _, size := range []int{64 << 10, 4 << 20} {
			b.Run(fmt.Sprintf("code=%t/bytes=%d", code, size), func(b *testing.B) {
				data := codexBenchData(b, code, size)
				b.SetBytes(int64(len(data)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					p := NewCodex()
					p.Feed(data)
					if p.Skipped != 0 || p.Title() == "" || len(p.Since(0)) == 0 {
						b.Fatal("benchmark data did not produce a conversation")
					}
				}
			})
		}
	}
}
