package convo

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// What went wrong with a turn, as Codex 0.164 writes it on the turn's
// task_complete (the shapes of every kind found in two months of real logs):
// each a note of its own after the person's message, not a turn ending with
// nothing said of why.
func TestCodexTurnError(t *testing.T) {
	turn := func(text string, err map[string]any) []string {
		done := map[string]any{"type": "task_complete", "last_agent_message": nil}
		if err != nil {
			done["error"] = err
		}
		return []string{
			codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
			codexLine(t, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage", "id": "u-" + text, "content": []any{map[string]any{"type": "text", "text": text}}}}),
			codexLine(t, "event_msg", done),
		}
	}
	var rows []string
	rows = append(rows, turn("a", map[string]any{"message": "unexpected status 503 Service Unavailable: Service temporarily unavailable, url: https://relay.example/responses, request id: 66cb", "codex_error_info": map[string]any{"http_connection_failed": map[string]any{"http_status_code": 503}}})...)
	rows = append(rows, turn("b", map[string]any{"message": "exceeded retry limit, last status: 429 Too Many Requests, request id: f30e", "codex_error_info": map[string]any{"response_too_many_failed_attempts": map[string]any{"http_status_code": 429}}})...)
	rows = append(rows, turn("s", map[string]any{"message": "Selected model is at capacity. Please try a different model.", "codex_error_info": "server_overloaded"})...)
	rows = append(rows, turn("c", map[string]any{"message": "stream disconnected before completion: response protection is unavailable", "codex_error_info": "other"})...)
	rows = append(rows, turn("d", map[string]any{"message": "This content was flagged for possible cybersecurity risk.", "codex_error_info": "cyber_policy"})...)
	rows = append(rows, turn("e", map[string]any{"message": "You’ve hit your usage limit. Try again at 10:00 PM.", "codex_error_info": "usage_limit_exceeded"})...)
	rows = append(rows, turn("f", nil)...) // (a turn that went well: nothing)
	c := NewCodex()
	c.Feed([]byte(strings.Join(rows, "")))
	var got []string
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			switch {
			case p.Trouble != nil:
				got = append(got, e.Role+":"+p.Trouble.Kind+":"+strconv.Itoa(p.Trouble.Status)+":"+string([]rune(p.Trouble.Text)[:12]))
			default:
				got = append(got, e.Role+":"+p.Text)
			}
		}
	}
	want := []string{
		"user:a", "note:api:503:unexpected s",
		"user:b", "note:api:429:exceeded ret",
		"user:s", "note:api:0:Selected mod",
		"user:c", "note:api:0:stream disco",
		"user:d", "note:refused:0:This content",
		"user:e", "note:api:0:You’ve hit y",
		"user:f",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
	if c.Status() != "idle" {
		t.Errorf("status %q", c.Status())
	}
}

// An older Codex's error row (seen to 0.128), and one that wrote both it and the
// error on task_complete: told once.
func TestCodexTurnErrorOnce(t *testing.T) {
	rows := []string{
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "user_message", "message": "hi"}),
		codexLine(t, "event_msg", map[string]any{"type": "error", "message": "unexpected status 401 Unauthorized"}),
		codexLine(t, "event_msg", map[string]any{"type": "task_complete", "error": map[string]any{"message": "unexpected status 401 Unauthorized", "codex_error_info": map[string]any{"http_connection_failed": map[string]any{"http_status_code": 401}}}}),
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "user_message", "message": "again"}),
		codexLine(t, "event_msg", map[string]any{"type": "task_complete", "error": map[string]any{"message": "unexpected status 401 Unauthorized", "codex_error_info": map[string]any{"http_connection_failed": map[string]any{"http_status_code": 401}}}}),
		codexLine(t, "event_msg", map[string]any{"type": "task_started"}),
		codexLine(t, "event_msg", map[string]any{"type": "user_message", "message": "null"}),
		codexLine(t, "event_msg", map[string]any{"type": "task_complete", "error": nil}),
	}
	c := NewCodex()
	c.Feed([]byte(strings.Join(rows, "")))
	n := 0
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Trouble != nil {
				n++
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d troubles, want 2 (once a turn)", n)
	}
}

// TestCodexTurnErrorReal: in every rollout under CORRAL_CODEX_LOGS, each
// turn that ended in an error (on its task_complete, or an older Codex's
// error row) shows as a note. Run it when a new Codex version is about: a
// change in where it writes errors shows here, not in the person's chat.
// Skipped without it.
func TestCodexTurnErrorReal(t *testing.T) {
	glob := os.Getenv("CORRAL_CODEX_LOGS")
	if glob == "" {
		t.Skip("CORRAL_CODEX_LOGS not set")
	}
	files, _ := filepath.Glob(glob)
	all, n := 0, 0
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		// (Turns that ended in an error: once a turn, as shown, whichever
		// way it was written.)
		want, failed := 0, false
		for _, l := range bytes.Split(b, []byte("\n")) {
			var r struct {
				Type    string `json:"type"`
				Payload struct {
					Type    string          `json:"type"`
					Message string          `json:"message"`
					Error   json.RawMessage `json:"error"`
				} `json:"payload"`
			}
			if json.Unmarshal(l, &r) != nil || r.Type != "event_msg" {
				continue
			}
			switch r.Payload.Type {
			case "task_started":
				failed = false
			case "error":
				if !failed && strings.TrimSpace(r.Payload.Message) != "" {
					want, failed = want+1, true
				}
			case "task_complete":
				if !failed && len(r.Payload.Error) > 0 && string(r.Payload.Error) != "null" {
					want++
				}
				failed = false
			}
		}
		if want == 0 {
			continue
		}
		c := NewCodex()
		c.Feed(b)
		if c.trimmed {
			continue // (its first entries let go: not all there to count)
		}
		got := 0
		for _, e := range c.entries {
			for _, p := range e.Parts {
				if p.Trouble != nil && p.Trouble.Kind != TroubleCut {
					got++
				}
			}
		}
		if got != want {
			t.Errorf("%s: %d turns ended in an error, %d shown", name, want, got)
		}
		all, n = all+want, n+1
	}
	t.Logf("%d rollouts, %d errors", n, all)
}
