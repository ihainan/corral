package convo

import (
	"reflect"
	"testing"
)

func TestClaudeEvents(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"s1","content":"Conversation compacted","compactMetadata":{"trigger":"auto","preTokens":213270,"postTokens":6621,"durationMs":72583,"preservedMessages":{"uuids":["x"]}}}`,
		`{"type":"user","uuid":"u2","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued…"}}`,
		`{"type":"system","subtype":"informational","uuid":"s2","content":"Compacted while idle, before the prompt cache expired","level":"notice"}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"s3","compactMetadata":{"trigger":"manual","preTokens":-5,"postTokens":"many","durationMs":900}}`,
		`{"type":"system","subtype":"informational","uuid":"s4","content":"Unknown command: /restart","level":"warning"}`,
		`{"type":"system","subtype":"scheduled_task_fire","uuid":"s5","content":"Running scheduled task (Oct 27 9:07pm)","taskId":"e9dd925d","cron":"7 21 27 10 *","prompt":"check the deploy","taskKind":"loop"}`,
		`{"type":"user","uuid":"u3","isMeta":true,"promptSource":"system","turnOrigin":"scheduled","scheduledTaskId":"e9dd925d","message":{"role":"user","content":"check the deploy"}}`,
		`{"type":"user","uuid":"u4","turnOrigin":"scheduled","message":{"role":"user","content":"check the deploy"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"user","uuid":"u5","turnOrigin":"scheduled","message":{"role":"user","content":"check again"}}`,
		`{"type":"user","uuid":"u6","turnOrigin":"scheduled","message":{"role":"user","content":[{"type":"text","text":"as blocks"}]}}`,
		`{"type":"user","uuid":"u7","turnOrigin":"scheduled","promptSource":"system","message":{"role":"user","content":[{"type":"text","text":"sent by it"}]}}`,
		`{"type":"system","subtype":"api_error","uuid":"s6","level":"error","retryInMs":570,"retryAttempt":1,"maxRetries":2,"error":{"formatted":"Internal server error","status":500}}`,
		`{"type":"system","subtype":"api_error","uuid":"s7","level":"error","retryAttempt":3,"maxRetries":2,"error":{"formatted":"Internal server error","status":500}}`,
	))
	type got struct {
		Role string
		Ev   Event
		Tr   Trouble
		Text string
	}
	var all []got
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			g := got{Role: e.Role, Text: p.Text}
			if p.Event != nil {
				g.Ev = *p.Event
			}
			if p.Trouble != nil {
				g.Tr = *p.Trouble
			}
			all = append(all, g)
		}
	}
	want := []got{
		{Role: RoleUser, Text: "hello"},
		{Role: RoleNote, Ev: Event{Kind: EventCompact, Trigger: "auto", Pre: 213270, Post: 6621, Ms: 72583}},
		{Role: RoleSummary, Text: "This session is being continued…"},
		{Role: RoleNote, Ev: Event{Kind: EventIdleCompact}},
		{Role: RoleNote, Ev: Event{Kind: EventCompact, Trigger: "manual", Ms: 900}}, // (what cannot be read of it is left out)
		{Role: RoleNote, Ev: Event{Kind: EventNotice, Text: "Unknown command: /restart", Level: "warning"}},
		{Role: RoleNote, Ev: Event{Kind: EventScheduled, Text: "check the deploy", Loop: true}},
		{Role: RoleNote, Ev: Event{Kind: EventScheduled, Text: "check the deploy", Echo: true}}, // (u4: the view shows its firing's)
		{Role: RoleAssistant, Text: "done"},
		{Role: RoleNote, Ev: Event{Kind: EventScheduled, Text: "check again", Echo: true}}, // (no firing: said as one)
		{Role: RoleNote, Ev: Event{Kind: EventScheduled, Text: "as blocks", Echo: true}},   // (its text in blocks)
		{Role: RoleNote, Ev: Event{Kind: EventScheduled, Text: "sent by it", Echo: true}},
		{Role: RoleNote, Tr: Trouble{Kind: TroubleAPI, Text: "Internal server error", Status: 500, Attempt: 1, Max: 2, RetryMs: 570}},
		{Role: RoleNote, Tr: Trouble{Kind: TroubleAPI, Text: "Internal server error", Status: 500}}, // (an attempt past the most: not told)
	}
	if !reflect.DeepEqual(all, want) {
		for i := range max(len(all), len(want)) {
			var a, w got
			if i < len(all) {
				a = all[i]
			}
			if i < len(want) {
				w = want[i]
			}
			if !reflect.DeepEqual(a, w) {
				t.Errorf("%d: got %+v\nwant %+v", i, a, w)
			}
		}
	}
}

// The plan's usage limit, as Claude Code 2.1.312 logs it: reached (its
// error row, with when it resets), the wait to go on, reset, called off.
// A rate limit of the interface's own stays an answer as it was.
func TestClaudeLimitEvents(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,"quotaLimits":{"status":"rejected","resetsAt":1788601200,"rateLimitType":"five_hour"},"message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 5:40pm (Asia/Shanghai)"}]}}`,
		`{"type":"system","subtype":"informational","uuid":"s1","content":"Usage limit reached · continuing automatically at 5:40pm · esc or type to cancel","level":"notice"}`,
		`{"type":"system","subtype":"informational","uuid":"s2","content":"Usage limit reset · continuing automatically","level":"notice"}`,
		`{"type":"system","subtype":"informational","uuid":"s3","content":"Automatic continue cancelled · Claude Code exited during the wait","level":"warning"}`,
		`{"type":"assistant","uuid":"a2","isApiErrorMessage":true,"error":"rate_limit","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"Rate limited, retry later"}]}}`,
	))
	var kinds []string
	var first Event
	for _, e := range c.Since(0) {
		p := e.Parts[0]
		if p.Event != nil {
			kinds = append(kinds, p.Event.Kind)
			if p.Event.Kind == EventLimit {
				first = *p.Event
			}
		} else {
			kinds = append(kinds, e.Role+":"+p.Text)
		}
	}
	want := []string{EventLimit, EventLimitWait, EventLimitReset, EventLimitCancelled, "assistant:Rate limited, retry later"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("got %q", kinds)
	}
	if first.ResetsAt != 1788601200 || first.LimitKind != "five_hour" || first.Text == "" {
		t.Fatalf("limit %+v", first)
	}
}

// What else Claude Code says of the session: a recap of what went on while
// the person was away, a goal set and judged, any other notice.
func TestClaudeRecapGoalNotice(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"system","subtype":"away_summary","uuid":"s1","content":"Goal is a slimmer subscription; the DNS is done."}`,
		`{"type":"attachment","uuid":"g1","attachment":{"type":"goal_status","met":false,"sentinel":true,"condition":"make the report"}}`,
		`{"type":"attachment","uuid":"g2","attachment":{"type":"goal_status","met":true,"condition":"make the report","reason":"The report is written."}}`,
		`{"type":"system","subtype":"informational","uuid":"s2","content":"Unknown command: /restart","level":"warning"}`,
		`{"type":"system","subtype":"informational","uuid":"s3","content":"Fable 5's safeguards stopped the response above · continuing once with that noted","level":"notice"}`,
	))
	var got []Event
	for _, e := range c.Since(0) {
		got = append(got, *e.Parts[0].Event)
	}
	want := []Event{
		{Kind: EventRecap, Text: "Goal is a slimmer subscription; the DNS is done."},
		{Kind: EventGoal, Goal: "make the report", Set: true},
		{Kind: EventGoal, Goal: "make the report", Met: true, Why: "The report is written."},
		{Kind: EventNotice, Text: "Unknown command: /restart", Level: "warning"},
		{Kind: EventNotice, Text: "Fable 5's safeguards stopped the response above · continuing once with that noted", Level: "notice"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}
