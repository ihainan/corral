package convo

import (
	"reflect"
	"strings"
	"testing"
)

// As Claude Code 2.1.312 logs them: the questions in the call, the answers
// in its account of the result; a plan in ExitPlanMode's input (written in
// by Claude Code from the plan's file, though the model sent none).
func TestClaudeAsksAndPlan(t *testing.T) {
	c := NewClaude()
	c.Feed(lines(
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"set up the service"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[`+
			`{"header":"Database","question":"Which database?","multiSelect":false,"options":[{"label":"PostgreSQL","description":"Relational"},{"label":"SQLite","description":"One file","preview":"db.Open(\"sqlite\")\n// no server"}]},`+
			`{"header":"Features","question":"Which features?","multiSelect":true,"options":[{"label":"Search"},{"label":"Export"}]}]}}]}}`,
		`{"type":"user","uuid":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"answered"}]},"toolUseResult":{"questions":[],"answers":{"Which database?":"SQLite","Which features?":"Search, Export"}}}`,
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"tool_use","id":"p1","name":"ExitPlanMode","input":{"plan":"# Plan\n1. Add a table\n","planFilePath":"/home/x/.claude/plans/plan.md"}}]}}`,
	))
	var ask, plan *Tool
	for _, e := range c.Since(0) {
		for _, p := range e.Parts {
			if p.Tool != nil && p.Tool.Name == "AskUserQuestion" {
				ask = p.Tool
			}
			if p.Tool != nil && p.Tool.Name == "ExitPlanMode" {
				plan = p.Tool
			}
		}
	}
	if ask == nil || plan == nil {
		t.Fatal("calls not read")
	}
	want := []Ask{
		{Header: "Database", Question: "Which database?", Options: []AskOption{{Label: "PostgreSQL", Description: "Relational"}, {Label: "SQLite", Description: "One file", Preview: "db.Open(\"sqlite\")\n// no server"}}},
		{Header: "Features", Question: "Which features?", Multi: true, Options: []AskOption{{Label: "Search"}, {Label: "Export"}}},
	}
	if !reflect.DeepEqual(ask.Asks, want) {
		t.Errorf("asks %+v", ask.Asks)
	}
	if !reflect.DeepEqual(ask.Answers, []string{"SQLite", "Search, Export"}) {
		t.Errorf("answers %q", ask.Answers)
	}
	if plan.Plan != "# Plan\n1. Add a table\n" || plan.PlanCut || plan.PlanPath != "/home/x/.claude/plans/plan.md" || plan.Done {
		t.Errorf("plan %+v", plan)
	}
}

// Bounds: past them, what is kept says it is cut.
func TestClaudeAsksBounded(t *testing.T) {
	opts := make([]string, 10)
	for i := range opts {
		opts[i] = `{"label":"o"}`
	}
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"q","options":[` + strings.Join(opts, ",") + `]}]}}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"tool_use","id":"p1","name":"ExitPlanMode","input":{"plan":"` + strings.Repeat("x", 40000) + `"}}]}}`,
	))
	es := c.Since(0)
	a, p := es[0].Parts[0].Tool, es[1].Parts[0].Tool
	if len(a.Asks[0].Options) != maxOptions || !a.Asks[0].Cut {
		t.Errorf("options %d cut %v", len(a.Asks[0].Options), a.Asks[0].Cut)
	}
	if len(p.Plan) != maxPlan || !p.PlanCut {
		t.Errorf("plan %d cut %v", len(p.Plan), p.PlanCut)
	}
}

// A long question: its answer found by the question as Claude Code wrote
// it, though only its start is kept.
func TestClaudeAnswerOfLongQuestion(t *testing.T) {
	long := strings.Repeat("why ", 100) + "?"
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"`+long+`","options":[{"label":"a"},{"label":"b"}]}]}}]}}`,
		`{"type":"user","uuid":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"x"}]},"toolUseResult":{"answers":{"`+long+`":"b"}}}`,
	))
	if got := c.Since(0)[0].Parts[0].Tool.Answers; len(got) != 1 || got[0] != "b" {
		t.Fatalf("answers %q", got)
	}
}

// Two long questions alike in what is kept of them: neither is told an
// answer that may be the other's.
func TestClaudeAnswersAlike(t *testing.T) {
	head := strings.Repeat("x", 400)
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"`+head+`A","options":[{"label":"a"}]},{"question":"`+head+`B","options":[{"label":"b"}]},{"question":"short","options":[{"label":"c"}]}]}}]}}`,
		`{"type":"user","uuid":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"x"}]},"toolUseResult":{"answers":{"`+head+`A":"one","`+head+`B":"two","short":"c"}}}`,
	))
	if got := c.Since(0)[0].Parts[0].Tool.Answers; len(got) != 3 || got[0] != "" || got[1] != "" || got[2] != "c" {
		t.Fatalf("answers %q", got)
	}
}

// Two questions alike in what is kept, one of them answered: neither told.
func TestClaudeAnswersAlikeOneAnswered(t *testing.T) {
	head := strings.Repeat("x", 400)
	c := NewClaude()
	c.Feed(lines(
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{"questions":[{"question":"`+head+`A","options":[{"label":"a"}]},{"question":"`+head+`B","options":[{"label":"b"}]}]}}]}}`,
		`{"type":"user","uuid":"r1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"q1","content":"x"}]},"toolUseResult":{"answers":{"`+head+`A":"one"}}}`,
	))
	if got := c.Since(0)[0].Parts[0].Tool.Answers; len(got) != 2 || got[0] != "" || got[1] != "" {
		t.Fatalf("answers %q", got)
	}
}
