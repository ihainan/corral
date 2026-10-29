package web

import "testing"

// cc-statusline's record, its last line read off the file's end.
func TestQuotaOf(t *testing.T) {
	rec := `{"updated_at":1793259449,"session_id":"x","five_hour":{"used_percentage":3,"remaining_percentage":97,"resets_at_epoch":1793272200},"weekly":{"used_percentage":67.5,"resets_at_epoch":1793545200}}`
	q := quotaOf([]byte("garbage cut lin" + "e\n" + `{"updated_at":1}` + "\n" + rec + "\n"))
	if !q.Known || q.UpdatedAt != 1793259449 || q.FiveHour == nil || q.FiveHour.Used != 3 || q.FiveHour.ResetsAt != 1793272200 || q.Weekly == nil || q.Weekly.Used != 67.5 {
		t.Fatalf("%+v", q)
	}
	for name, tail := range map[string]string{
		"empty":         "",
		"bad JSON":      "{nope",
		"no windows":    `{"updated_at":1}`,
		"over 100":      `{"five_hour":{"used_percentage":101,"resets_at_epoch":5}}`,
		"negative":      `{"five_hour":{"used_percentage":-1,"resets_at_epoch":5}}`,
		"no reset time": `{"five_hour":{"used_percentage":5}}`,
		"a string":      `{"five_hour":{"used_percentage":"5","resets_at_epoch":5}}`,
	} {
		if q := quotaOf([]byte(tail)); q.Known {
			t.Fatalf("%s: %+v", name, q)
		}
	}
	// (One window of the two: that one.)
	if q := quotaOf([]byte(`{"weekly":{"used_percentage":0,"resets_at_epoch":9}}`)); !q.Known || q.FiveHour != nil || q.Weekly.Used != 0 {
		t.Fatalf("one: %+v", q)
	}
}
