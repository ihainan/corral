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

// A Claude Code session imported into Codex (0.164's import): its tools'
// calls and results as written there, its command envelopes in the
// person's rows, the import's own mark.
func TestCodexClaudeImport(t *testing.T) {
	assistant := func(text string) []byte {
		row, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text}}}})
		return append(row, '\n')
	}
	user := func(turn, text string) []byte {
		row, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "turn_id": turn, "item": map[string]any{"type": "UserMessage", "id": "u" + turn, "content": []map[string]string{{"type": "text", "text": text}}}}})
		return append(row, '\n')
	}
	var log []byte
	log = append(log, user("external-import-turn-1", "<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args>sonnet</command-args>")...)
	log = append(log, user("external-import-turn-1", "<local-command-stdout>Set model to `Sonnet 5.5` and saved as your default for new sessions</local-command-stdout>")...)
	log = append(log, user("external-import-turn-1", "<system-reminder>internal</system-reminder>")...)
	log = append(log, user("external-import-turn-2", "This session is being continued from a previous conversation that ran out of context. Summary: …")...)
	log = append(log, user("external-import-turn-2", "看一下 dashboard")...)
	log = append(log, assistant("[external_agent_tool_call: Bash]\ndescription: Download dashboard homepage\ncommand: curl -s https://example.org/\n[/external_agent_tool_call]")...)
	log = append(log, assistant("[external_agent_tool_result]\n200 163187\n<!DOCTYPE html>")...)
	log = append(log, assistant("[external_agent_tool_result]\nan orphan result")...)
	log = append(log, assistant("好的，页面已下载。")...)
	log = append(log, assistant("<EXTERNAL SESSION IMPORTED>")...)
	// (Not imported: the person's own words, whatever they look like.)
	log = append(log, user("turn-9", "<command-name>/model</command-name>")...)
	p := NewCodex()
	p.Feed(log)
	var got []string
	for _, e := range p.Since(0) {
		for _, pt := range e.Parts {
			switch {
			case pt.Tool != nil:
				got = append(got, e.Role+" tool "+pt.Tool.Name+" | "+pt.Tool.Title+" | "+pt.Tool.Detail+" | "+pt.Tool.Result)
			default:
				got = append(got, e.Role+" "+pt.Text)
			}
		}
	}
	want := []string{
		"user /model sonnet",
		"note Set model to `Sonnet 5.5` and saved as your default for new sessions",
		"summary This session is being continued from a previous conversation that ran out of context. Summary: …",
		"user 看一下 dashboard",
		"assistant tool Bash | Download dashboard homepage | curl -s https://example.org/ | 200 163187\n<!DOCTYPE html>",
		"assistant tool result |  |  | an orphan result",
		"assistant 好的，页面已下载。",
		"user <command-name>/model</command-name>",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
