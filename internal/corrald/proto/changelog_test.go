package proto

import "testing"

func TestDefaultChangelogHasCurrentEntry(t *testing.T) {
	entries := Changelog()
	if len(entries) == 0 {
		t.Fatalf("expected at least one default changelog entry")
	}
	if len(entries[0].Zh) == 0 || len(entries[0].En) == 0 {
		t.Fatalf("expected bilingual notes on newest changelog entry: %+v", entries[0])
	}
}

func TestFilterChangelog(t *testing.T) {
	sample := []ChangelogEntry{
		{Version: "0.3.3", Zh: nil, En: nil}, // empty version must be omitted
		{Version: "0.3.2", Zh: []string{"修复连接断开后的恢复速度。"}, En: []string{"Faster recovery after reconnect."}},
		{Version: "0.3.1", Zh: []string{"发送消息和回答卡片改由辅助进程执行。"}, En: []string{"Sending and card answers use the helper daemon."}},
		{Version: "0.3.0", Zh: []string{"初始版本。"}, En: []string{"Initial release."}},
	}

	// Never viewed: returns all non-empty entries up to current (omitting empty 0.3.3).
	got := FilterChangelog(sample, "0.3.3", "")
	if len(got) != 3 || got[0].Version != "0.3.2" || got[1].Version != "0.3.1" || got[2].Version != "0.3.0" {
		t.Fatalf("first view at 0.3.3: got %+v", got)
	}

	// Viewed 0.3.0 earlier, upgraded to 0.3.2 -> shows 0.3.2 and 0.3.1 only.
	got = FilterChangelog(sample, "v0.3.2-4-g1f9a7f0", "0.3.0")
	if len(got) != 2 || got[0].Version != "0.3.2" || got[1].Version != "0.3.1" {
		t.Fatalf("upgrade from 0.3.0 to 0.3.2: got %+v", got)
	}

	// Already viewed 0.3.2, reopened at 0.3.2 -> shows current version 0.3.2.
	got = FilterChangelog(sample, "0.3.2", "0.3.2")
	if len(got) != 1 || got[0].Version != "0.3.2" {
		t.Fatalf("reopen at 0.3.2: got %+v", got)
	}

	// Current version 0.3.3 has no notes, already viewed 0.3.2 -> returns empty (no empty item for 0.3.3).
	got = FilterChangelog(sample, "0.3.3", "0.3.2")
	if len(got) != 0 {
		t.Fatalf("empty current 0.3.3 after seeing 0.3.2: expected 0 entries, got %+v", got)
	}

	// Undocumented dev version reopened after already viewing dev -> returns only latest documented version.
	got = FilterChangelog(sample, "dev", "dev")
	if len(got) != 1 || got[0].Version != "0.3.2" {
		t.Fatalf("reopen undocumented dev: got %+v", got)
	}
}
