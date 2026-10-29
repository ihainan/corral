package convo

import (
	"strings"
	"testing"
)

// cost-state rows: the latest read stands (running totals, never added
// up); its models by cost; an older part read before gives one only where
// the later part said none.
func TestClaudeCost(t *testing.T) {
	row := func(usd string, models string) string {
		return `{"type":"cost-state","totalCostUSD":` + usd + `,"totalAPIDuration":5000,"totalLinesAdded":40,"totalLinesRemoved":16,"totalDuration":1000,"startTime":1792890915545,"modelUsage":` + models + `,"hasUnknownModelCost":true}` + "\n"
	}
	m1 := `{"claude-haiku-5-5":{"inputTokens":10,"outputTokens":2,"costUSD":0.01},"claude-opus-5-5":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokens":3000,"cacheCreationInputTokens":400,"costUSD":2.5}}`
	c := NewClaude()
	c.Feed([]byte(row("1.5", m1) + row("3.25", m1)))
	cost := c.Meta().Cost
	if cost == nil || cost.USD != 3.25 || !cost.Unknown || cost.Added != 40 || cost.Removed != 16 || cost.APIMs != 5000 || cost.At != 1792890916545 {
		t.Fatalf("cost: %+v", cost)
	}
	if len(cost.Models) != 2 || cost.Models[0].Model != "claude-opus-5-5" || cost.Models[0].CacheRead != 3000 || cost.Models[0].CacheWrite != 400 || cost.Models[1].In != 10 {
		t.Fatalf("models: %+v", cost.Models)
	}
	// As a string (some versions).
	q := strings.ReplaceAll(m1, `"`, `\"`)
	c = NewClaude()
	c.Feed([]byte(row("1", `"`+q+`"`)))
	if cost := c.Meta().Cost; cost == nil || len(cost.Models) != 2 {
		t.Fatalf("as a string: %+v", cost)
	}
	// None read: nil, not nought.
	c = NewClaude()
	c.Feed([]byte(`{"type":"user","uuid":"u1","timestamp":"2026-10-29T00:00:00Z","message":{"content":"hi"}}` + "\n"))
	if c.Meta().Cost != nil {
		t.Fatal("a cost from nothing")
	}
	// Prepend: the later part's own stands; where it said none, the older's.
	older := NewClaude()
	older.Feed([]byte(row("1", m1)))
	later := NewClaude()
	later.Feed([]byte(row("2", m1)))
	later.Prepend(older)
	if later.Meta().Cost.USD != 2 {
		t.Fatalf("the later's: %v", later.Meta().Cost.USD)
	}
	none := NewClaude()
	none.Feed([]byte(`{"type":"user","uuid":"u2","timestamp":"2026-10-29T00:00:00Z","message":{"content":"hi"}}` + "\n"))
	none.Prepend(older)
	if none.Meta().Cost == nil || none.Meta().Cost.USD != 1 {
		t.Fatalf("the older's, where the later said none: %+v", none.Meta().Cost)
	}
}
