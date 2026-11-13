package convo

import "testing"

func TestCodexEffort(t *testing.T) {
	older := NewCodex()
	older.Feed([]byte(codexLine(t, "turn_context", map[string]any{"model": "gpt-6-sol", "effort": "high"})))
	if older.Meta().Effort != "high" {
		t.Fatal("turn_context effort missing")
	}
	newer := NewCodex()
	newer.Feed([]byte(codexLine(t, "event_msg", map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"model": "gpt-6-sol", "reasoning_effort": "xhigh"}})))
	newer.Prepend(older)
	if newer.Meta().Effort != "xhigh" {
		t.Fatal("older effort replaced the latest setting")
	}
	newer.Feed([]byte(codexLine(t, "turn_context", map[string]any{"model": "gpt-6-sol", "effort": nil})))
	newer.Prepend(older)
	if newer.Meta().Effort != "" {
		t.Fatal("default effort inherited an old explicit level")
	}
	newer.Feed([]byte(codexLine(t, "turn_context", map[string]any{"model": "gpt-6-sol", "effort": "high"})))
	newer.Feed([]byte(codexLine(t, "turn_context", map[string]any{"model": "other-model"})))
	if newer.Meta().Effort != "" {
		t.Fatal("another model inherited the old effort")
	}
	tail := NewCodex()
	tail.Prepend(older)
	if tail.Meta().Effort != "high" {
		t.Fatal("effort missing after reading the older settings")
	}
}
