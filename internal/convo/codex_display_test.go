package convo

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexImportedToolPresentation(t *testing.T) {
	for _, tc := range []struct {
		text string
		tool bool
	}{
		{"[external agent tool call: Bash]\ndescription: Read image list\ncommand: docker images\n" + strings.Repeat("echo inspect\n", 1000), true},
		{"[external agent tool result: Bash]\nProcess exited successfully", true},
		{"Explain this notation: [external agent tool call: Bash]\ncommand: docker images", false},
		{"```text\n[external agent tool call: Bash]\ncommand: docker images\n```", false},
	} {
		payload := map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": tc.text}}}
		row, _ := json.Marshal(map[string]any{"type": "response_item", "payload": payload})
		p := NewCodex()
		p.Feed(append(row, '\n'))
		entries := p.Since(0)
		if len(entries) != 1 || len(entries[0].Parts) != 1 {
			t.Fatalf("missing imported record: %+v", entries)
		}
		part := entries[0].Parts[0]
		if (part.Kind == KindTool) != tc.tool {
			t.Errorf("wrong presentation: %+v", part)
		}
		if tc.tool {
			if part.Tool.Name != "Bash" || !part.Tool.Done {
				t.Errorf("wrong tool: %+v", part.Tool)
			}
			if strings.Contains(part.Text, "external agent") {
				t.Error("transport marker left in message text")
			}
		}
	}
}

func TestCodexStructuredReplyPresentation(t *testing.T) {
	text := `<send_user_message_question_reply>[{"question":"Continue?","answer":"Yes"}]</send_user_message_question_reply>`
	row, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]string{"type": "user_message", "message": text}})
	p := NewCodex()
	p.Feed(append(row, '\n'))
	entries := p.Since(0)
	if len(entries) != 1 || entries[0].Role != RoleUser || entries[0].Parts[0].Text != "Continue?\nYes" {
		t.Fatalf("reply was not rendered as readable text: %+v", entries)
	}
	if p.Title() != "" {
		t.Fatalf("internal reply became the conversation title: %s", p.Title())
	}
	quoted := "Explain this XML: " + text
	if got, ok := codexReplyText(quoted); ok || got != quoted {
		t.Fatal("quoted markup was interpreted")
	}
	if got, ok := codexReplyText("<send_user_message_question_reply>invalid</send_user_message_question_reply>"); ok || !strings.Contains(got, "invalid") {
		t.Fatal("malformed user input was discarded")
	}
}

func TestCodexTaskNoticePresentation(t *testing.T) {
	text := "<task-notification>\n<task-id>test</task-id>\n<status>completed</status>\n<summary>Background check completed</summary>\n</task-notification>"
	row, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]string{"type": "user_message", "message": text}})
	p := NewCodex()
	p.Feed(append(row, '\n'))
	entries := p.Since(0)
	if len(entries) != 1 || entries[0].Role != RoleNote || entries[0].Parts[0].Kind != KindEvent {
		t.Fatalf("task envelope leaked into user messages: %+v", entries)
	}
	if p.Title() != "" {
		t.Fatal("task notice became a title")
	}
}
