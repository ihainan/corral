package web

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/store"
)

func TestLineageTrackerMetadataMoveAndSessionProtection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	lt := &lineageTracker{
		srv:          h.srv,
		ownerID:      1,
		hostID:       10,
		server:       "srv-lineage",
		socket:       "/run/herdr/s1.sock",
		herdrSession: "default",
		live:         map[string]livePaneInfo{},
		termToPane:   map[string]string{},
		knownPanes:   map[string]bool{},
		paneLogs:     map[string]knownPaneLog{},
		probedSize:   map[string]int64{},
		retriedPane:  map[string]bool{},
	}

	// Snapshot 1: child pane carries corral_parent_pane metadata token.
	snap1 := &herdr.Snapshot{
		Version: "0.8.0",
		Workspaces: []herdr.Workspace{
			{ID: "w1", Label: "main", Number: 1},
		},
		Agents: []herdr.Pane{
			{
				ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", AgentStatus: "idle",
				TerminalID:   "term-1",
				AgentSession: &herdr.AgentSessionInfo{Value: "sess-parent-1"},
			},
			{
				ID: "w1:p2", WorkspaceID: "w1", Agent: "claude", AgentStatus: "working",
				TerminalID:   "term-2",
				AgentSession: &herdr.AgentSessionInfo{Value: "sess-child-1"},
				Tokens: map[string]string{
					"corral_parent_pane":    "w1:p1",
					"corral_parent_term":    "term-1",
					"corral_parent_session": "sess-parent-1",
					"corral_child_session":  "sess-child-1",
				},
			},
		},
	}

	lt.OnSnapshot(ctx, snap1)
	sum := summarize("default", snap1, &agentTimes{at: map[string]agentTime{}}, false)
	lt.decorate(&sum)
	if len(sum.Workspaces[0].Agents) != 2 {
		t.Fatalf("expected 2 agents, got %+v", sum.Workspaces[0].Agents)
	}
	child := sum.Workspaces[0].Agents[1]
	if child.ParentPane != "w1:p1" || child.ParentTermID != "term-1" || child.LineageSource != store.LineageMetadata || child.TerminalID != "term-2" {
		t.Fatalf("unexpected decorated child: %+v", child)
	}

	// Event: pane_moved moves child from w1:p2 -> w2:p1.
	lt.OnEvent(ctx, "pane_moved", json.RawMessage(`{"type":"pane_moved","previous_pane_id":"w1:p2","pane":{"pane_id":"w2:p1","terminal_id":"term-2"}}`))

	snap2 := &herdr.Snapshot{
		Version: "0.8.0",
		Workspaces: []herdr.Workspace{
			{ID: "w1", Label: "main", Number: 1},
			{ID: "w2", Label: "sub", Number: 2},
		},
		Agents: []herdr.Pane{
			snap1.Agents[0],
			{
				ID: "w2:p1", WorkspaceID: "w2", Agent: "claude", AgentStatus: "done",
				TerminalID:   "term-2",
				AgentSession: snap1.Agents[1].AgentSession,
			},
		},
	}
	lt.OnSnapshot(ctx, snap2)
	sum2 := summarize("default", snap2, &agentTimes{at: map[string]agentTime{}}, false)
	lt.decorate(&sum2)
	movedChild := sum2.Workspaces[1].Agents[0]
	if movedChild.ParentPane != "w1:p1" || movedChild.LineageSource != store.LineageMetadata {
		t.Fatalf("moved child lost lineage: %+v", movedChild)
	}

	// Symmetric occupant session protection: if parent's pane switches to a different
	// agent session, the stale edge must NOT be applied.
	snap3 := &herdr.Snapshot{
		Version:    "0.8.0",
		Workspaces: snap2.Workspaces,
		Agents: []herdr.Pane{
			{
				ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", AgentStatus: "idle",
				TerminalID:   "term-1",
				AgentSession: &herdr.AgentSessionInfo{Value: "sess-parent-DIFFERENT"},
			},
			snap2.Agents[1],
		},
	}
	lt.OnSnapshot(ctx, snap3)
	sum3 := summarize("default", snap3, &agentTimes{at: map[string]agentTime{}}, false)
	lt.decorate(&sum3)
	if sum3.Workspaces[1].Agents[0].ParentPane != "" {
		t.Fatalf("expected lineage to be withheld when parent session changed, got %+v", sum3.Workspaces[1].Agents[0])
	}
}

func TestLineageTrackerStaleChildTokenIgnored(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	lt := &lineageTracker{
		srv:         h.srv,
		ownerID:     1,
		hostID:      10,
		server:      "srv-stale",
		live:        map[string]livePaneInfo{},
		termToPane:  map[string]string{},
		knownPanes:  map[string]bool{},
		paneLogs:    map[string]knownPaneLog{},
		probedSize:  map[string]int64{},
		retriedPane: map[string]bool{},
	}

	snap := &herdr.Snapshot{
		Version:    "0.8.0",
		Workspaces: []herdr.Workspace{{ID: "w1", Label: "main", Number: 1}},
		Agents: []herdr.Pane{
			{ID: "w1:p1", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-1"},
			{
				ID: "w1:p2", WorkspaceID: "w1", Agent: "claude", TerminalID: "term-2",
				AgentSession: &herdr.AgentSessionInfo{Value: "sess-new"},
				Tokens: map[string]string{
					"corral_parent_pane":   "w1:p1",
					"corral_child_session": "sess-old",
				},
			},
		},
	}
	lt.OnSnapshot(ctx, snap)
	sum := summarize("default", snap, &agentTimes{at: map[string]agentTime{}}, false)
	lt.decorate(&sum)
	if sum.Workspaces[0].Agents[1].ParentPane != "" {
		t.Fatalf("stale child session token should not bind: %+v", sum.Workspaces[0].Agents[1])
	}
}
