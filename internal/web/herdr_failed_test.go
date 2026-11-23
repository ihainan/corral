package web

import (
	"context"
	"strings"
	"testing"
)

// A turn that ended on an API error Claude Code wrote (a rate limit, the
// server's error) is told failed, with the error's kind; one that went on
// past an error (retried, answered) is not, nor one the person spoke after,
// nor a subagent's error.
func TestParseBgTailsFailed(t *testing.T) {
	sids := []string{bgSid1, bgSid2, "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"}
	tails := [][]string{
		{
			`{"type":"user","uuid":"u","message":{"content":"go"}}`,
			`{"type":"assistant","uuid":"a1","isApiErrorMessage":true,"error":"rate_limit","message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your session limit"}]}}`,
			`{"type":"system","subtype":"informational","uuid":"i"}`,
			`{"type":"system","subtype":"turn_duration","uuid":"t1"}`,
		},
		{
			`{"type":"assistant","uuid":"a1","isApiErrorMessage":true,"error":"server_error","message":{"content":[{"type":"text","text":"API Error: 500"}]}}`,
			`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"Done after all."}]}}`,
			`{"type":"system","subtype":"turn_duration","uuid":"t2"}`,
		},
		{
			`{"type":"assistant","uuid":"a1","isApiErrorMessage":true,"error":"Weird Kind!","message":{"content":[{"type":"text","text":"API Error"}]}}`,
			`{"type":"system","subtype":"turn_duration","uuid":"t3"}`,
		},
		{
			`{"type":"assistant","uuid":"a1","isApiErrorMessage":true,"error":"server_error","message":{"content":[{"type":"text","text":"API Error"}]}}`,
			`{"type":"user","uuid":"u","message":{"content":"try again"}}`,
			`{"type":"system","subtype":"turn_duration","uuid":"t4"}`,
		},
		{
			`{"type":"assistant","uuid":"a0","message":{"content":[{"type":"text","text":"Fine."}]}}`,
			`{"type":"assistant","uuid":"s","isSidechain":true,"isApiErrorMessage":true,"error":"rate_limit","message":{"content":[{"type":"text","text":"sub"}]}}`,
			`{"type":"system","subtype":"turn_duration","uuid":"t5"}`,
		},
	}
	var b strings.Builder
	for i, s := range sids {
		b.WriteString("\n@@corral-bg " + s + "\n" + strings.Join(tails[i], "\n"))
	}
	got := parseBgTails([]byte(b.String()))
	for i, want := range []string{"rate_limit", "", "unknown", "", ""} {
		if r := got[sids[i]]; r.failed != want {
			t.Errorf("tail %d: failed %q, want %q (%+v)", i, r.failed, want, r)
		}
	}
}

// decorate tells a failed turn only once it is known, and not of one with
// subagents still out (its turn not over).
func TestBgProberDecoratesFailed(t *testing.T) {
	p := newBgProber(nil)
	p.panes["p1"] = &bgPane{session: bgSid1, state: bgNone, failed: "rate_limit"}
	p.panes["p2"] = &bgPane{session: bgSid2, state: bgRunning, n: 1, failed: "rate_limit"}
	p.panes["p3"] = &bgPane{session: bgSid1, state: bgChecking, failed: "rate_limit"}
	sum := &herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "p1", Session: bgSid1}, {PaneID: "p2", Session: bgSid2}, {PaneID: "p3"}}}}}
	p.decorate(sum)
	a := sum.Workspaces[0].Agents
	if a[0].Failed != "rate_limit" || a[1].Failed != "" || a[2].Failed != "" {
		t.Fatalf("failed told: %q %q %q", a[0].Failed, a[1].Failed, a[2].Failed)
	}
}

// The model a pane's agent answered with is the last of its own answers'
// (not a subagent's, nor one Claude Code wrote for an error), kept for the
// pane while it runs that session, gone with the pane.
func TestBgProberModel(t *testing.T) {
	tail := "\n@@corral-bg " + bgSid1 + "\n" + strings.Join([]string{
		`{"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"x"}]}}`,
		`{"type":"assistant","uuid":"s","isSidechain":true,"message":{"model":"claude-haiku-5-5","content":[]}}`,
		`{"type":"assistant","uuid":"e","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[]}}`,
		`{"type":"system","subtype":"turn_duration","uuid":"t1"}`,
	}, "\n")
	if r := parseBgTails([]byte(tail))[bgSid1]; r.model != "claude-opus-5-5" {
		t.Fatalf("model %q", r.model)
	}
	if m := modelIn([][]byte{[]byte(`{"type":"assistant","message":{"model":"bad model; rm -rf"}}`)}); m != "" {
		t.Fatalf("a name not a model's taken: %q", m)
	}
	p := newBgProber(func(context.Context, []string, int) (map[string]bgRecord, error) {
		return parseBgTails([]byte(tail)), nil
	})
	p.consider("p1", "claude", "done", bgSid1, 1)
	p.once(context.Background())
	sum := &herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "p1", Session: bgSid1, Status: "working"}}}}}
	p.consider("p1", "claude", "working", bgSid1, 2) // (at work again: still that model)
	p.decorate(sum)
	if got := sum.Workspaces[0].Agents[0].Model; got != "claude-opus-5-5" {
		t.Fatalf("model kept: %q", got)
	}
	other := &herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "p1", Session: bgSid2}}}}}
	p.decorate(other)
	if got := other.Workspaces[0].Agents[0].Model; got != "" {
		t.Fatalf("another session's model told: %q", got)
	}
	// (A summary of before the pane came, sent again, takes nothing away.)
	p.decorate(&herdrSummary{})
	again := &herdrSummary{Workspaces: []herdrWorkspace{{Agents: []herdrAgent{{PaneID: "p1", Session: bgSid1}}}}}
	p.decorate(again)
	if got := again.Workspaces[0].Agents[0].Model; got != "claude-opus-5-5" {
		t.Fatalf("model lost to an old summary: %q", got)
	}
	arn := "arn:aws:bedrock:us-east-1:123456789012:inference-profile/global.anthropic.claude-sonnet-4-5-20250929-v1:0"
	if m := modelIn([][]byte{[]byte(`{"type":"assistant","message":{"model":"` + arn + `"}}`)}); m != arn {
		t.Fatalf("a Bedrock ARN not taken: %q", m)
	}
}
