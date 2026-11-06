package web

import (
	"testing"
	"time"
)

func TestQuotaOfConfig(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	const signedIn = `"oauthAccount":{"accountUuid":"acc-1"},`
	rec := func(account, five, week string) string {
		return `{` + signedIn + `"other":{"a":1},"cachedUsageUtilization":{"fetchedAtMs":1793937000000,"accountUuid":"` + account + `","utilization":{"five_hour":` + five + `,"seven_day":` + week + `,"seven_day_opus":null}}}`
	}
	const later = `{"utilization":62,"resets_at":"2026-11-08T14:59:59.676874+00:00","limit_dollars":null}`
	const passed = `{"utilization":99,"resets_at":"2026-11-06T03:00:00+00:00"}`

	q, in := quotaOfConfig([]byte(rec("acc-1", later, later)), now)
	if !in || !q.Known || q.UpdatedAt != 1793937000 || q.Weekly == nil || q.Weekly.Used != 62 || q.Weekly.ResetsAt != 1794149999 || q.FiveHour == nil {
		t.Fatalf("%+v %v", q, in)
	}
	// (A window not begun is null: left out, the other said.)
	if q, _ := quotaOfConfig([]byte(rec("acc-1", "null", later)), now); !q.Known || q.FiveHour != nil || q.Weekly == nil {
		t.Errorf("no five hours: %+v", q)
	}
	// (Its reset passed: nothing of now.)
	if q, _ := quotaOfConfig([]byte(rec("acc-1", passed, later)), now); q.FiveHour != nil || q.Weekly == nil {
		t.Errorf("reset passed: %+v", q)
	}
	if q, in := quotaOfConfig([]byte(rec("acc-1", passed, passed)), now); q.Known || !in {
		t.Errorf("both passed: %+v %v", q, in)
	}
	// (Another account's record: nothing of this one; still signed in.)
	if q, in := quotaOfConfig([]byte(rec("acc-2", later, later)), now); q.Known || !in {
		t.Errorf("another's: %+v %v", q, in)
	}
	// (Not signed in with an account, an API key's say: no record, nothing to ask.)
	if q, in := quotaOfConfig([]byte(`{"cachedUsageUtilization":{"accountUuid":"acc-1","utilization":{"seven_day":`+later+`}}}`), now); q.Known || in {
		t.Errorf("signed out: %+v %v", q, in)
	}
	if q, in := quotaOfConfig([]byte(`{`+signedIn+`"x":1}`), now); q.Known || !in {
		t.Errorf("signed in, nothing kept: %+v %v", q, in)
	}
	// (Of a kind not looked for, or not read: nothing, and no error.)
	for _, bad := range []string{`not json`, ``, `[]`, rec("acc-1", `{"utilization":"62","resets_at":"x"}`, `{"utilization":140,"resets_at":"2026-11-08T14:59:59Z"}`)} {
		if q, _ := quotaOfConfig([]byte(bad), now); q.Known {
			t.Errorf("%q: %+v", bad, q)
		}
	}
}

// However many look, an account on a host has it asked at most once in quotaEvery.
func TestQuotaRunsAtMostSoOften(t *testing.T) {
	var r quotaRuns
	at := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	n := 0
	for i := 0; i < 24*60; i++ { // (a look a minute for a day)
		if r.due("a", at.Add(time.Duration(i)*time.Minute)) {
			n++
		}
	}
	if n != 96 {
		t.Errorf("%d in a day", n)
	}
	if !r.due("b", at) {
		t.Error("another account is its own")
	}
}
