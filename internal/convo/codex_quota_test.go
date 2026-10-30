package convo

import (
	"strings"
	"testing"
)

func codexLimitsLine(t *testing.T, at string, limits any) string {
	l := codexLine(t, "event_msg", map[string]any{"type": "token_count", "info": nil, "rate_limits": limits})
	return strings.Replace(l, "2026-10-26T01:00:00.000Z", at, 1)
}

func window(used float64, minutes, resets int64) map[string]any {
	return map[string]any{"used_percent": used, "window_minutes": minutes, "resets_at": resets}
}

// The account's use of its windows, as the latest row says it: by the
// window's minutes; a relay's nulls, another limit's and a bad number keep it.
func TestCodexQuota(t *testing.T) {
	c := NewCodex()
	if c.Meta().Quota != nil {
		t.Fatal("quota before any row")
	}
	c.Feed([]byte(codexLimitsLine(t, "2026-10-30T07:00:00.000Z", map[string]any{"limit_id": "codex", "primary": window(43, 300, 1791302615), "secondary": window(18, 10080, 1791581449)})))
	q := c.Meta().Quota
	if q == nil || q.FiveHour == nil || q.Weekly == nil || q.FiveHour.Used != 43 || q.FiveHour.ResetsAt != 1791302615 || q.Weekly.Used != 18 || q.UpdatedAt != 1793343600 {
		t.Fatalf("quota %+v", q)
	}
	c.Feed([]byte(codexLimitsLine(t, "2026-10-30T07:01:00.000Z", map[string]any{"limit_id": "codex", "primary": nil, "secondary": nil}) +
		codexLimitsLine(t, "2026-10-30T07:02:00.000Z", map[string]any{"limit_id": "codex_bengalfox", "limit_name": "GPT-5.3-Codex-Spark", "primary": window(0, 300, 1791309999)}) +
		codexLimitsLine(t, "2026-10-30T07:03:00.000Z", map[string]any{"limit_id": "premium", "primary": window(0, 10080, 1791309999)}) +
		codexLimitsLine(t, "2026-10-30T07:04:00.000Z", map[string]any{"limit_id": "codex", "primary": window(150, 300, 1791309999)}) +
		codexLimitsLine(t, "2026-10-30T07:05:00.000Z", nil)))
	if q2 := c.Meta().Quota; q2.FiveHour.Used != 43 || q2.Weekly.Used != 18 || q2.UpdatedAt != q.UpdatedAt {
		t.Errorf("kept %+v / %+v %+v", q2, q2.FiveHour, q2.Weekly)
	}
	// An older Codex's (no limit_id); one window said: the other kept, of
	// its own time, the quota's time the older one's.
	c.Feed([]byte(codexLimitsLine(t, "2026-10-30T07:10:00.000Z", map[string]any{"primary": window(50, 300, 1791302615)})))
	if q3 := c.Meta().Quota; q3.FiveHour.Used != 50 || q3.Weekly.Used != 18 || q3.FiveHour.At != 1793344200 || q3.Weekly.At != 1793343600 || q3.UpdatedAt != 1793343600 {
		t.Errorf("updated %+v / %+v %+v", q3, q3.FiveHour, q3.Weekly)
	}
	if q.FiveHour.Used != 43 {
		t.Error("an earlier Meta's quota changed")
	}
}

// Read in two parts (the later first): each window the later part's, else
// the earlier's.
func TestCodexQuotaPrepend(t *testing.T) {
	early := codexLimitsLine(t, "2026-10-30T06:00:00.000Z", map[string]any{"limit_id": "codex", "primary": window(10, 300, 1791302615), "secondary": window(5, 10080, 1791581449)})
	late := codexLimitsLine(t, "2026-10-30T07:00:00.000Z", map[string]any{"limit_id": "codex", "primary": window(40, 300, 1791302615), "secondary": window(20, 10080, 1791581449)})
	relay := codexLimitsLine(t, "2026-10-30T07:00:00.000Z", map[string]any{"limit_id": "codex", "primary": nil, "secondary": nil})
	half := codexLimitsLine(t, "2026-10-30T07:00:00.000Z", map[string]any{"limit_id": "codex", "primary": window(40, 300, 1791302615), "secondary": nil})
	for _, tc := range []struct {
		later        string
		five, weekly float64
		at           int64
	}{{late, 40, 20, 1793343600}, {relay, 10, 5, 1793340000}, {half, 40, 5, 1793340000}} {
		a, b := NewCodex(), NewCodex()
		a.Feed([]byte(early))
		b.Feed([]byte(tc.later))
		b.Prepend(a)
		if q := b.Meta().Quota; q == nil || q.FiveHour.Used != tc.five || q.Weekly == nil || q.Weekly.Used != tc.weekly || q.UpdatedAt != tc.at {
			t.Errorf("want %v %v %v: %+v", tc.five, tc.weekly, tc.at, q)
		}
	}
}
