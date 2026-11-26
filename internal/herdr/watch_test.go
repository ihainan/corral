package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/testutil"
)

type capturedLog struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

type logRecorder struct {
	mu      sync.Mutex
	records []capturedLog
	notify  chan struct{}
}

func newLogRecorder() *logRecorder {
	return &logRecorder{notify: make(chan struct{}, 1)}
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]slog.Value, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Resolve()
		return true
	})
	r.mu.Lock()
	r.records = append(r.records, capturedLog{
		Level: rec.Level,
		Msg:   rec.Message,
		Attrs: attrs,
	})
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) snapshot() []capturedLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedLog, len(r.records))
	copy(out, r.records)
	return out
}

func (r *logRecorder) waitFor(t *testing.T, timeout time.Duration, pred func([]capturedLog) bool) []capturedLog {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		recs := r.snapshot()
		if pred(recs) {
			return recs
		}
		select {
		case <-r.notify:
		case <-deadline.C:
			t.Fatalf("timed out waiting for log condition; got %d records: %+v", len(recs), recs)
		}
	}
}

func TestWatcherLogsFailuresAndRecovery(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var attempt atomic.Int32
	subscribed := make(chan struct{})
	var subOnce sync.Once

	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		// Script 5 consecutive failures across steps (dial, snapshot, subscribe, subscribe, dial),
		// then succeed on attempt 6. Rate limiting logs warnings on failures 1, 2, 4 only.
		a := attempt.Load()
		if a == 0 || a == 4 {
			attempt.Add(1)
			_ = nc.Reject(ssh.ConnectionFailed, "socket refused")
			return true
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			r := bufio.NewReader(ch)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			switch req.Method {
			case "session.snapshot":
				cur := attempt.Load()
				if cur == 1 {
					attempt.Add(1)
					// Fail snapshot by closing without reply.
					return
				}
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.8.0","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1"}]}}}` + "\n"))
			case "events.subscribe":
				cur := attempt.Load()
				if cur == 2 || cur == 3 {
					attempt.Add(1)
					// Fail subscribe with transport close (non-APIError so no legacy retry).
					return
				}
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"ok":true}}` + "\n"))
				subOnce.Do(func() { close(subscribed) })
				_, _ = io.Copy(io.Discard, ch)
			}
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := newLogRecorder()
	var lostCount atomic.Int32
	var sawLostAt atomic.Bool
	w := NewWatcher(c, "/run/herdr/default.sock", nil, nil)
	w.SessionID = "sess-42"
	w.ServerName = "default"
	w.Log = slog.New(rec)
	w.baseBackoff = 2 * time.Millisecond
	w.maxBackoff = 4 * time.Millisecond
	w.OnLost = func() {
		lostCount.Add(1)
		if !w.LostAt().IsZero() {
			sawLostAt.Store(true)
		}
	}

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	recs := rec.waitFor(t, 4*time.Second, func(list []capturedLog) bool {
		return len(list) >= 4 && list[len(list)-1].Level == slog.LevelInfo
	})
	<-subscribed
	cancel()
	<-done

	if got := lostCount.Load(); got != 5 {
		t.Fatalf("OnLost calls = %d, want 5", got)
	}
	if !sawLostAt.Load() {
		t.Fatal("expected LostAt() to be non-zero during OnLost")
	}
	if !w.LostAt().IsZero() {
		t.Fatalf("expected LostAt() to be cleared after recovery, got %v", w.LostAt())
	}
	if len(recs) != 4 {
		t.Fatalf("expected 4 log records (warns at 1,2,4 + 1 recovery info), got %d: %+v", len(recs), recs)
	}

	wantWarns := []struct {
		failures int64
		step     string
	}{
		{1, stepDial},
		{2, stepSnapshot},
		{4, stepSubscribe},
	}
	for i, want := range wantWarns {
		got := recs[i]
		if got.Level != slog.LevelWarn {
			t.Fatalf("record[%d] level = %v, want WARN", i, got.Level)
		}
		if got.Attrs["session"].String() != "sess-42" {
			t.Fatalf("record[%d] session = %q, want sess-42", i, got.Attrs["session"].String())
		}
		if got.Attrs["server"].String() != "default" {
			t.Fatalf("record[%d] server = %q, want default", i, got.Attrs["server"].String())
		}
		if got.Attrs["socket"].String() != "/run/herdr/default.sock" {
			t.Fatalf("record[%d] socket = %q, want /run/herdr/default.sock", i, got.Attrs["socket"].String())
		}
		if got.Attrs["step"].String() != want.step {
			t.Fatalf("record[%d] step = %q, want %q", i, got.Attrs["step"].String(), want.step)
		}
		if got.Attrs["busy"].Bool() {
			t.Fatalf("record[%d] busy = true, want false", i)
		}
		if got.Attrs["failures"].Int64() != want.failures {
			t.Fatalf("record[%d] failures = %d, want %d", i, got.Attrs["failures"].Int64(), want.failures)
		}
		if _, ok := got.Attrs["down_for"]; !ok {
			t.Fatalf("record[%d] missing down_for attr", i)
		}
		if got.Attrs["err"].String() == "" {
			t.Fatalf("record[%d] expected non-empty err", i)
		}
	}

	info := recs[3]
	if info.Level != slog.LevelInfo {
		t.Fatalf("record[3] level = %v, want INFO", info.Level)
	}
	if info.Attrs["session"].String() != "sess-42" || info.Attrs["server"].String() != "default" || info.Attrs["socket"].String() != "/run/herdr/default.sock" {
		t.Fatalf("recovery attrs mismatch: %+v", info.Attrs)
	}
	if info.Attrs["failures"].Int64() != 5 {
		t.Fatalf("recovery failures = %d, want 5", info.Attrs["failures"].Int64())
	}
	if info.Attrs["down_for"].Duration() < 0 {
		t.Fatalf("recovery down_for = %v, want >= 0", info.Attrs["down_for"].Duration())
	}
}

func TestWatcherLogsBusyOnPostSubscribeRefresh(t *testing.T) {
	oldMax := sshx.MaxAuxPerConn
	sshx.MaxAuxPerConn = 2
	defer func() { sshx.MaxAuxPerConn = oldMax }()

	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	subReady := make(chan struct{}, 4)
	sendEvent := make(chan struct{})

	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			r := bufio.NewReader(ch)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			switch req.Method {
			case "session.snapshot":
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.8.0","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1"}]}}}` + "\n"))
			case "events.subscribe":
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"ok":true}}` + "\n"))
				select {
				case subReady <- struct{}{}:
				default:
				}
				<-sendEvent
				_, _ = ch.Write([]byte(`{"event":"pane.agent_status_changed","data":{}}` + "\n"))
				_, _ = io.Copy(io.Discard, ch)
			}
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := newLogRecorder()
	w := NewWatcher(c, "/run/herdr/busy.sock", nil, nil)
	w.SessionID = "sess-busy"
	w.ServerName = "default"
	w.Log = slog.New(rec)
	w.baseBackoff = 5 * time.Millisecond
	w.maxBackoff = 10 * time.Millisecond

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Wait until initial snapshot + subscribe succeeded (holding 1 aux slot).
	<-subReady

	// Occupy the remaining aux slot so the post-subscribe refresh Dial hits sshx.ErrBusy.
	hog, err := Dial(ctx, c, "/run/herdr/busy.sock")
	if err != nil {
		t.Fatalf("dial hog: %v", err)
	}
	close(sendEvent)

	recs := rec.waitFor(t, 4*time.Second, func(list []capturedLog) bool {
		return len(list) >= 1 && list[0].Level == slog.LevelWarn
	})
	// Release the hogged slot so the watcher can recover cleanly.
	hog.Close()

	recs = rec.waitFor(t, 4*time.Second, func(list []capturedLog) bool {
		return list[len(list)-1].Level == slog.LevelInfo
	})
	cancel()
	<-done

	first := recs[0]
	if first.Attrs["step"].String() != stepRefresh {
		t.Fatalf("step = %q, want %q", first.Attrs["step"].String(), stepRefresh)
	}
	if !first.Attrs["busy"].Bool() {
		t.Fatalf("busy = false, want true (err=%q)", first.Attrs["err"].String())
	}
	if first.Attrs["failures"].Int64() != 1 {
		t.Fatalf("failures = %d, want 1", first.Attrs["failures"].Int64())
	}
}

func TestWatcherLogsStreamBreak(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var subCalls atomic.Int32

	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			r := bufio.NewReader(ch)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			switch req.Method {
			case "session.snapshot":
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.8.0","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1"}]}}}` + "\n"))
			case "events.subscribe":
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"ok":true}}` + "\n"))
				if subCalls.Add(1) == 1 {
					// Break the first event stream immediately after subscribe.
					return
				}
				_, _ = io.Copy(io.Discard, ch)
			}
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := newLogRecorder()
	w := NewWatcher(c, "/run/herdr/stream.sock", nil, nil)
	w.SessionID = "sess-stream"
	w.ServerName = "default"
	w.Log = slog.New(rec)
	w.baseBackoff = 2 * time.Millisecond
	w.maxBackoff = 4 * time.Millisecond

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	recs := rec.waitFor(t, 4*time.Second, func(list []capturedLog) bool {
		return len(list) >= 2 && list[1].Level == slog.LevelInfo
	})
	cancel()
	<-done

	if recs[0].Level != slog.LevelWarn || recs[0].Attrs["step"].String() != stepStream || recs[0].Attrs["busy"].Bool() {
		t.Fatalf("unexpected stream break warn: %+v", recs[0])
	}
	if recs[1].Level != slog.LevelInfo || recs[1].Attrs["failures"].Int64() != 1 {
		t.Fatalf("unexpected recovery info: %+v", recs[1])
	}
}

func TestWatcherPaneSetChangeDoesNotLog(t *testing.T) {
	srv := testutil.StartSSHServer(t, "alice", "alice-pass", t.TempDir())
	var snapCalls atomic.Int32
	var subCalls atomic.Int32
	secondSub := make(chan struct{})
	var secondOnce sync.Once

	srv.HandleChannel(func(nc ssh.NewChannel) bool {
		if nc.ChannelType() != streamLocal {
			return false
		}
		ch, reqs, err := nc.Accept()
		if err != nil {
			return true
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			defer ch.Close()
			r := bufio.NewReader(ch)
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil {
				return
			}
			switch req.Method {
			case "session.snapshot":
				if snapCalls.Add(1) == 1 {
					_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.8.0","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1"}]}}}` + "\n"))
				} else {
					_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"snapshot":{"version":"0.8.0","panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"t1"},{"pane_id":"w1:p2","workspace_id":"w1","tab_id":"t1"}]}}}` + "\n"))
				}
			case "events.subscribe":
				_, _ = ch.Write([]byte(`{"id":"` + req.ID + `","result":{"ok":true}}` + "\n"))
				n := subCalls.Add(1)
				if n == 1 {
					// Trigger a post-subscribe refresh that sees a new pane and resubscribes cleanly.
					_, _ = ch.Write([]byte(`{"event":"pane.created","data":{}}` + "\n"))
				} else {
					secondOnce.Do(func() { close(secondSub) })
				}
				_, _ = io.Copy(io.Discard, ch)
			}
		}()
		return true
	})

	c := dialTest(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := newLogRecorder()
	var lostCalls atomic.Int32
	w := NewWatcher(c, "/run/herdr/ok.sock", nil, nil)
	w.SessionID = "sess-ok"
	w.ServerName = "default"
	w.Log = slog.New(rec)
	w.baseBackoff = 2 * time.Millisecond
	w.maxBackoff = 4 * time.Millisecond
	w.OnLost = func() { lostCalls.Add(1) }

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-secondSub:
	case <-ctx.Done():
		t.Fatal("timed out waiting for resubscribe after pane set change")
	}
	cancel()
	<-done

	if lostCalls.Load() != 0 {
		t.Fatalf("OnLost called %d times on normal pane set change, want 0", lostCalls.Load())
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Fatalf("expected 0 logs on normal pane set change, got %d: %+v", len(recs), recs)
	}
}
