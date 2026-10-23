package herdr

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Transition is an agent status change in one pane.
type Transition struct {
	Pane      Pane
	Workspace string // workspace label, for messages
	From, To  string
}

// Watcher keeps a fresh snapshot of a herdr session and reports agent status
// changes. Following herdr's guidance, events are only a signal to re-read the
// snapshot; we never patch an old snapshot with event contents.
type Watcher struct {
	client     *ssh.Client
	socketPath string
	onChange   func(*Snapshot)
	onTrans    func(Transition)
	// OnLost, if set, is called when the connection to herdr fails: what was
	// reported last may no longer be so until the next snapshot.
	OnLost func()

	mu   sync.Mutex
	last *Snapshot
}

func NewWatcher(c *ssh.Client, socketPath string, onChange func(*Snapshot), onTransition func(Transition)) *Watcher {
	return &Watcher{client: c, socketPath: socketPath, onChange: onChange, onTrans: onTransition}
}

// Latest returns the most recent snapshot, or nil before the first read.
func (w *Watcher) Latest() *Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// Run blocks until ctx ends, reconnecting with backoff after failures.
func (w *Watcher) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := w.once(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = time.Second
		} else if w.OnLost != nil {
			w.OnLost()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// once refreshes the snapshot, subscribes, and returns when the pane set changes
// (so the per-pane subscriptions are rebuilt) or the stream breaks.
func (w *Watcher) once(ctx context.Context) error {
	snap, err := w.refresh(ctx)
	if err != nil {
		return err
	}
	ev, err := Dial(ctx, w.client, w.socketPath)
	if err != nil {
		return err
	}
	defer ev.Close()

	subs := []map[string]any{
		{"type": "workspace.created"}, {"type": "workspace.updated"}, {"type": "workspace.closed"}, {"type": "workspace.renamed"},
		{"type": "workspace.focused"}, {"type": "tab.created"}, {"type": "tab.closed"}, {"type": "tab.focused"}, {"type": "tab.renamed"},
		{"type": "pane.created"}, {"type": "pane.closed"}, {"type": "pane.exited"}, {"type": "pane.focused"}, {"type": "pane.agent_detected"},
	}
	for _, p := range snap.Panes {
		subs = append(subs, map[string]any{"type": "pane.agent_status_changed", "pane_id": p.ID})
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	kick := make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- ev.Subscribe(sctx, subs, func(json.RawMessage) {
			select {
			case kick <- struct{}{}:
			default:
			}
		})
	}()
	paneKey := paneSet(snap)
	// Periodic refresh as a safety net in case an event was dropped.
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case <-kick:
			time.Sleep(250 * time.Millisecond) // let bursts settle
			for len(kick) > 0 {
				<-kick
			}
		case <-tick.C:
		}
		s, err := w.refresh(ctx)
		if err != nil {
			return err
		}
		if paneSet(s) != paneKey {
			return nil // resubscribe with the new pane list
		}
	}
}

func (w *Watcher) refresh(ctx context.Context) (*Snapshot, error) {
	c, err := Dial(ctx, w.client, w.socketPath)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	prev := w.last
	w.last = snap
	w.mu.Unlock()
	if prev != nil && w.onTrans != nil {
		before := map[string]string{}
		for _, p := range prev.Panes {
			before[p.ID] = p.AgentStatus
		}
		labels := map[string]string{}
		for _, ws := range snap.Workspaces {
			labels[ws.ID] = ws.Label
		}
		now := map[string]bool{}
		for _, p := range snap.Panes {
			now[p.ID] = true
			if from, ok := before[p.ID]; ok && from != p.AgentStatus {
				w.onTrans(Transition{Pane: p, Workspace: labels[p.WorkspaceID], From: from, To: p.AgentStatus})
			}
		}
		// A pane that closed (an agent waiting in it waits no more).
		for _, p := range prev.Panes {
			if !now[p.ID] {
				w.onTrans(Transition{Pane: p, Workspace: labels[p.WorkspaceID], From: p.AgentStatus, To: StatusGone})
			}
		}
	}
	if w.onChange != nil {
		w.onChange(snap)
	}
	return snap, nil
}

func paneSet(s *Snapshot) string {
	ids := make([]string, 0, len(s.Panes))
	for _, p := range s.Panes {
		ids = append(ids, p.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
