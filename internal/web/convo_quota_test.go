package web

import (
	"strings"
	"testing"
)

// cc-statusline's record, its last line read off the file's end.
func TestQuotaOf(t *testing.T) {
	rec := `{"updated_at":1793259449,"session_id":"x","five_hour":{"used_percentage":3,"remaining_percentage":97,"resets_at_epoch":1793272200},"weekly":{"used_percentage":67.5,"resets_at_epoch":1793545200}}`
	q := quotaOf([]byte("{\"updated_at\":1}\n"+rec+"\n"), true)
	if !q.Known || q.UpdatedAt != 1793259449 || q.FiveHour == nil || q.FiveHour.Used != 3 || q.FiveHour.ResetsAt != 1793272200 || q.Weekly == nil || q.Weekly.Used != 67.5 {
		t.Fatalf("%+v", q)
	}
	// (Per review420.) A line still being written: the one before.
	if q := quotaOf([]byte(rec+"\n{\"updated_at\":"), true); !q.Known || q.FiveHour.Used != 3 {
		t.Fatalf("half a line at the end: %+v", q)
	}
	// Read from the middle: its first line a piece, not taken for a record.
	if q := quotaOf([]byte(rec[40:]+"\n"), false); q.Known {
		t.Fatalf("a piece taken whole: %+v", q)
	}
	// (Whole JSON though it is, the first line read from the middle is not taken.)
	if q := quotaOf([]byte(rec+"\n"), false); q.Known {
		t.Fatalf("a first line from the middle taken: %+v", q)
	}
	// (null is no number, per review421.)
	if q := quotaOf([]byte(`{"five_hour":{"used_percentage":null,"resets_at_epoch":1999999999}}`+"\n"), true); q.Known {
		t.Fatalf("null taken for 0: %+v", q)
	}
	if q := quotaOf([]byte("piece\n"+rec+"\n"), false); !q.Known {
		t.Fatalf("after a piece: %+v", q)
	}
	// One part of another kind: the other kept.
	if q := quotaOf([]byte(`{"updated_at":"x","five_hour":{"used_percentage":"5","resets_at_epoch":5},"weekly":{"used_percentage":7,"resets_at_epoch":9}}`+"\n"), true); !q.Known || q.FiveHour != nil || q.Weekly == nil || q.Weekly.Used != 7 || q.UpdatedAt != 0 {
		t.Fatalf("mixed: %+v", q)
	}
	for name, tail := range map[string]string{
		"empty":         "",
		"no newline":    rec,
		"bad JSON":      "{nope\n",
		"no windows":    `{"updated_at":1}` + "\n",
		"over 100":      `{"five_hour":{"used_percentage":101,"resets_at_epoch":5}}` + "\n",
		"negative":      `{"five_hour":{"used_percentage":-1,"resets_at_epoch":5}}` + "\n",
		"no reset time": `{"five_hour":{"used_percentage":5}}` + "\n",
		"a string":      `{"five_hour":{"used_percentage":"5","resets_at_epoch":5}}` + "\n",
		"an array":      `[1,2]` + "\n",
	} {
		if q := quotaOf([]byte(tail), true); q.Known {
			t.Fatalf("%s: %+v", name, q)
		}
	}
	if q := quotaOf([]byte(`{"weekly":{"used_percentage":0,"resets_at_epoch":9}}`+"\n"), true); !q.Known || q.FiveHour != nil || q.Weekly.Used != 0 {
		t.Fatalf("one: %+v", q)
	}
	// A record longer than what is read: none, not a piece.
	long := `{"pad":"` + strings.Repeat("x", quotaTail) + `",` + rec[1:] + "\n"
	if q := quotaOf([]byte(long[len(long)-quotaTail:]), false); q.Known {
		t.Fatalf("longer than read: %+v", q)
	}
}
