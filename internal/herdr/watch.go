package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// Transition is an agent status change in one pane.
type Transition struct {
	Pane      Pane
	Workspace string // workspace label, for messages
	From, To  string
}

var errStreamEnded = errors.New("herdr event stream ended")

const (
	stepDial      = "dial"
	stepSnapshot  = "snapshot"
	stepSubscribe = "subscribe"
	stepStream    = "stream"
	stepRefresh   = "refresh"
)

type watchError struct {
	step string
	err  error
}

func (e *watchError) Error() string { return e.err.Error() }
func (e *watchError) Unwrap() error { return e.err }

func shouldLogFailure(n int) bool {
	return n > 0 && (n&(n-1)) == 0
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
	// OnDisconnect, if set, is called when a healthy watcher enters a lost
	// state (first failure of a streak), with the failing step and whether
	// sshx.ErrBusy caused it.
	OnDisconnect func(step string, busy bool)
	// OnReconnect, if set, is called when a lost watcher recovers, with the
	// outage duration and consecutive failure count.
	OnReconnect func(downFor time.Duration, failures int)
	// OnEvent, if set, is called for each decoded herdr event envelope
	// (e.g. "pane_moved") before the watcher refreshes its snapshot.
	OnEvent func(event string, data json.RawMessage)
	// SessionID and ServerName identify the Corral session and herdr server
	// in disconnect/recovery logs.
	SessionID  string
	ServerName string
	// Log receives disconnect and recovery logs (defaults to slog.Default()).
	Log *slog.Logger

	baseBackoff      time.Duration                                    // defaults to time.Second; overridden in tests
	maxBackoff       time.Duration                                    // defaults to time.Minute; overridden in tests
	refreshRetryBase time.Duration                                    // defaults to time.Second; overridden in tests
	refreshRetryMax  time.Duration                                    // defaults to 10 * time.Second; overridden in tests
	refreshGiveUp    time.Duration                                    // defaults to 20 * time.Second; overridden in tests
	kickSettle       time.Duration                                    // defaults to 250 * time.Millisecond; overridden in tests
	nowFunc          func() time.Time                                 // defaults to time.Now; overridden in tests
	retryTimerFunc   func(time.Duration) (<-chan time.Time, func())   // defaults to time.NewTimer; overridden in tests

	mu     sync.Mutex
	last   *Snapshot
	lastAt time.Time // (when last was read)
	lostAt time.Time // (when the current failure streak began; zero when healthy)
	again  chan struct{}
}

func NewWatcher(c *ssh.Client, socketPath string, onChange func(*Snapshot), onTransition func(Transition)) *Watcher {
	return &Watcher{client: c, socketPath: socketPath, onChange: onChange, onTrans: onTransition, again: make(chan struct{}, 1)}
}

// Again has the snapshot read again soon: for a change herdr tells no
// event of (a pane renamed).
func (w *Watcher) Again() {
	select {
	case w.again <- struct{}{}:
	default:
	}
}

// Latest returns the most recent snapshot, or nil before the first read.
func (w *Watcher) Latest() *Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// LatestAt is Latest with when it was read.
func (w *Watcher) LatestAt() (*Snapshot, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last, w.lastAt
}

// LostAt returns when the current consecutive failure streak started, or zero
// when the watcher is healthy.
func (w *Watcher) LostAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lostAt
}

func (w *Watcher) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *Watcher) now() time.Time {
	if w.nowFunc != nil {
		return w.nowFunc()
	}
	return time.Now()
}

func (w *Watcher) newRetryTimer(d time.Duration) (<-chan time.Time, func()) {
	if w.retryTimerFunc != nil {
		return w.retryTimerFunc(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Run blocks until ctx ends, reconnecting with backoff after failures.
func (w *Watcher) Run(ctx context.Context) {
	base := w.baseBackoff
	if base <= 0 {
		base = time.Second
	}
	capBackoff := w.maxBackoff
	if capBackoff <= 0 {
		capBackoff = time.Minute
	}
	backoff := base
	var (
		failCount   int
		firstFailAt time.Time
	)
	onConnected := func() {
		if failCount == 0 {
			return
		}
		downFor := time.Since(firstFailAt).Round(time.Millisecond)
		fc := failCount
		w.logger().Info("herdr watch recovered",
			"session", w.SessionID,
			"server", w.ServerName,
			"socket", w.socketPath,
			"failures", fc,
			"down_for", downFor,
		)
		failCount = 0
		firstFailAt = time.Time{}
		w.mu.Lock()
		w.lostAt = time.Time{}
		w.mu.Unlock()
		if w.OnReconnect != nil {
			w.OnReconnect(downFor, fc)
		}
	}
	for ctx.Err() == nil {
		began := time.Now()
		err := w.once(ctx, onConnected)
		if ctx.Err() != nil {
			return
		}
		// A connection that ran a while before it ended starts the waits over.
		if err == nil || time.Since(began) > 30*time.Second {
			backoff = base
		}
		if err != nil {
			now := time.Now()
			failCount++
			step := "unknown"
			var we *watchError
			if errors.As(err, &we) {
				step = we.step
			}
			busy := errors.Is(err, sshx.ErrBusy)
			if failCount == 1 {
				firstFailAt = now
				w.mu.Lock()
				w.lostAt = now
				w.mu.Unlock()
				if w.OnDisconnect != nil {
					w.OnDisconnect(step, busy)
				}
			}
			if shouldLogFailure(failCount) {
				w.logger().Warn("herdr watch lost",
					"session", w.SessionID,
					"server", w.ServerName,
					"socket", w.socketPath,
					"step", step,
					"busy", busy,
					"failures", failCount,
					"down_for", now.Sub(firstFailAt).Round(time.Millisecond),
					"err", err,
				)
			}
			if w.OnLost != nil {
				w.OnLost()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, capBackoff)
	}
}

// once refreshes the snapshot, subscribes, and returns when the pane set changes
// (so the per-pane subscriptions are rebuilt) or the stream breaks.
func (w *Watcher) once(ctx context.Context, onConnected func()) error {
	snap, err := w.refresh(ctx)
	if err != nil {
		return err
	}
	baseSubs := []map[string]any{
		{"type": "workspace.created"}, {"type": "workspace.updated"}, {"type": "workspace.closed"}, {"type": "workspace.renamed"},
		{"type": "workspace.focused"}, {"type": "tab.created"}, {"type": "tab.closed"}, {"type": "tab.focused"}, {"type": "tab.renamed"},
		{"type": "pane.created"}, {"type": "pane.closed"}, {"type": "pane.exited"}, {"type": "pane.focused"}, {"type": "pane.agent_detected"},
	}
	extSubs := append(append([]map[string]any(nil), baseSubs...),
		map[string]any{"type": "pane.moved"},
		map[string]any{"type": "pane.updated"},
	)
	for _, p := range snap.Panes {
		perPane := map[string]any{"type": "pane.agent_status_changed", "pane_id": p.ID}
		baseSubs = append(baseSubs, perPane)
		extSubs = append(extSubs, perPane)
	}

	ev, err := DialWatch(ctx, w.client, w.socketPath)
	if err != nil {
		return &watchError{step: stepDial, err: err}
	}
	if err := ev.StartSubscribe(ctx, extSubs); err != nil {
		ev.Close()
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			return &watchError{step: stepSubscribe, err: err}
		}
		// Older herdr versions reject newer subscription types (pane.moved / pane.updated)
		// and close the connection after an error: re-Dial cleanly with legacy subscriptions.
		ev, err = DialWatch(ctx, w.client, w.socketPath)
		if err != nil {
			return &watchError{step: stepDial, err: err}
		}
		if err := ev.StartSubscribe(ctx, baseSubs); err != nil {
			ev.Close()
			return &watchError{step: stepSubscribe, err: err}
		}
	}
	defer ev.Close()
	if onConnected != nil {
		onConnected()
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	kick := make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- ev.ReadEvents(sctx, func(raw json.RawMessage) {
			if w.OnEvent != nil {
				var env struct {
					Event string          `json:"event"`
					Data  json.RawMessage `json:"data"`
				}
				if json.Unmarshal(raw, &env) == nil && env.Event != "" {
					w.OnEvent(env.Event, env.Data)
				}
			}
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
	settle := w.kickSettle
	if settle <= 0 {
		settle = 250 * time.Millisecond
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if err == nil {
				err = errStreamEnded // (an ended stream is a lost one: the state is old until the next snapshot)
			}
			return &watchError{step: stepStream, err: err}
		case <-kick:
			t := time.NewTimer(settle) // let bursts settle
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case err := <-errc:
				t.Stop()
				if err == nil {
					err = errStreamEnded
				}
				return &watchError{step: stepStream, err: err}
			case <-t.C:
			}
			for len(kick) > 0 {
				<-kick
			}
		case <-tick.C:
		case <-w.again:
		}
		s, err := w.refreshWithRetry(ctx, errc)
		if err != nil {
			return err
		}
		if s == nil {
			return nil
		}
		if paneSet(s) != paneKey {
			return nil // resubscribe with the new pane list
		}
	}
}

// refreshWithRetry retries transient snapshot refresh failures while the event
// subscription stream (errc) remains healthy. It backs off exponentially up to
// refreshRetryMax and only gives up after refreshGiveUp of consecutive failures
// (or immediately if the subscription stream ends).
func (w *Watcher) refreshWithRetry(ctx context.Context, errc <-chan error) (*Snapshot, error) {
	retryBase := w.refreshRetryBase
	if retryBase <= 0 {
		retryBase = time.Second
	}
	retryMax := w.refreshRetryMax
	if retryMax <= 0 {
		retryMax = 10 * time.Second
	}
	giveUp := w.refreshGiveUp
	if giveUp <= 0 {
		giveUp = 20 * time.Second
	}

	var (
		failStart time.Time
		delay     = retryBase
		attempts  int
	)
	for {
		s, err := w.refresh(ctx)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		select {
		case serr := <-errc:
			if serr == nil {
				serr = errStreamEnded
			}
			return nil, &watchError{step: stepStream, err: serr}
		default:
		}

		rawErr := err
		var we *watchError
		if errors.As(err, &we) {
			rawErr = we.err
		}

		now := w.now()
		if attempts == 0 {
			failStart = now
		}
		attempts++
		elapsed := now.Sub(failStart)
		if elapsed >= giveUp {
			return nil, &watchError{step: stepRefresh, err: rawErr}
		}

		wait := delay
		if remaining := giveUp - elapsed; wait > remaining {
			wait = remaining
		}
		w.logger().Debug("herdr watch refresh failed",
			"session", w.SessionID,
			"server", w.ServerName,
			"socket", w.socketPath,
			"busy", errors.Is(rawErr, sshx.ErrBusy),
			"attempt", attempts,
			"failing_for", elapsed.Round(time.Millisecond),
			"retry_in", wait.Round(time.Millisecond),
			"err", rawErr,
		)

		waitCh, stop := w.newRetryTimer(wait)
		select {
		case <-ctx.Done():
			stop()
			return nil, nil
		case serr := <-errc:
			stop()
			if serr == nil {
				serr = errStreamEnded
			}
			return nil, &watchError{step: stepStream, err: serr}
		case <-waitCh:
			stop()
		}
		delay = min(delay*2, retryMax)
	}
}

func (w *Watcher) refresh(ctx context.Context) (*Snapshot, error) {
	c, err := DialWatch(ctx, w.client, w.socketPath)
	if err != nil {
		return nil, &watchError{step: stepDial, err: err}
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, &watchError{step: stepSnapshot, err: err}
	}
	w.mu.Lock()
	prev := w.last
	w.last, w.lastAt = snap, time.Now()
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
