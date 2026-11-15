package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testPeer(t *testing.T, serve func(context.Context, *websocket.Conn)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		var init message
		if readMessage(ctx, conn, &init) != nil || init.Method != "initialize" {
			return
		}
		if writeMessage(ctx, conn, message{ID: init.ID, Result: json.RawMessage(`{}`)}) != nil {
			return
		}
		if readMessage(ctx, conn, &init) != nil || init.Method != "initialized" {
			return
		}
		serve(ctx, conn)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func readMessage(ctx context.Context, c *websocket.Conn, out any) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func writeMessage(ctx context.Context, c *websocket.Conn, m message) error {
	b, _ := json.Marshal(m)
	return c.Write(ctx, websocket.MessageText, b)
}

func TestConcurrentRepliesAndSnapshotCursor(t *testing.T) {
	c := testPeer(t, func(ctx context.Context, conn *websocket.Conn) {
		var batch []message
		for range 20 {
			var m message
			if readMessage(ctx, conn, &m) != nil {
				return
			}
			batch = append(batch, m)
		}
		_ = writeMessage(ctx, conn, message{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"a","itemId":"x","delta":"before"}`)})
		for i := len(batch) - 1; i >= 0; i-- {
			if writeMessage(ctx, conn, message{ID: batch[i].ID, Result: batch[i].Params}) != nil {
				return
			}
		}
		_ = writeMessage(ctx, conn, message{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"a","itemId":"x","delta":"after"}`)})
		<-ctx.Done()
	})
	var group sync.WaitGroup
	for i := range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			var got int
			seq, err := c.CallAt(context.Background(), "echo", i, &got)
			if err != nil || got != i || seq != 1 {
				t.Errorf("echo %d: %d, seq %d, %v", i, got, seq, err)
			}
		}()
	}
	group.Wait()
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	for {
		events, _, gap, changed := c.Events(1, "a")
		if gap {
			t.Fatal("unexpected replay gap")
		}
		if len(events) == 1 {
			if !strings.Contains(string(events[0].Params), "after") {
				t.Fatal(events)
			}
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("missing later delta")
		}
	}
}

func TestCancelUnknownAndLateResponse(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	c := testPeer(t, func(ctx context.Context, conn *websocket.Conn) {
		var m message
		if readMessage(ctx, conn, &m) != nil {
			return
		}
		close(arrived)
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		_ = writeMessage(ctx, conn, message{ID: m.ID, Result: json.RawMessage(`{}`)})
		if readMessage(ctx, conn, &m) != nil {
			return
		}
		_ = writeMessage(ctx, conn, message{ID: m.ID, Result: json.RawMessage(`{}`)})
		<-ctx.Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "turn/start", nil, nil) }()
	<-arrived
	cancel()
	var unknown *Unknown
	if err := <-done; !errors.As(err, &unknown) {
		t.Fatalf("accepted request cancellation must remain unknown: %v", err)
	}
	close(release)
	if err := c.Call(context.Background(), "thread/read", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalIsolationAndSingleReply(t *testing.T) {
	replies := make(chan message, 1)
	c := testPeer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = writeMessage(ctx, conn, message{ID: json.RawMessage(`"approval"`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"a","command":"echo ok"}`)})
		var reply message
		if readMessage(ctx, conn, &reply) == nil {
			replies <- reply
		}
		<-ctx.Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for len(c.Requests("a")) == 0 {
		_, _, _, changed := c.Events(0, "a")
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("missing approval")
		}
	}
	build := func(Event) (any, error) { return map[string]string{"decision": "accept"}, nil }
	id := json.RawMessage(`"approval"`)
	if err := c.Reply(ctx, "b", id, build); !errors.Is(err, ErrStale) {
		t.Fatalf("cross-thread reply: %v", err)
	}
	if err := c.Reply(ctx, "a", id, func(Event) (any, error) { return nil, errors.New("invalid input") }); err == nil {
		t.Fatal("invalid response accepted")
	}
	if len(c.Requests("a")) != 1 {
		t.Fatal("invalid input consumed approval")
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); outcomes <- c.Reply(ctx, "a", id, build) }()
	}
	wg.Wait()
	close(outcomes)
	success, stale := 0, 0
	for err := range outcomes {
		if err == nil {
			success++
		} else if errors.Is(err, ErrStale) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success %d stale %d", success, stale)
	}
	select {
	case reply := <-replies:
		if string(reply.Result) != `{"decision":"accept"}` {
			t.Fatal(string(reply.Result))
		}
	case <-ctx.Done():
		t.Fatal("no reply")
	}
}

func TestReplayGapAndDisconnect(t *testing.T) {
	c := testPeer(t, func(ctx context.Context, conn *websocket.Conn) {
		for range maxEvents + 2 {
			_ = writeMessage(ctx, conn, message{Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"a","itemId":"x","delta":"x"}`)})
		}
	})
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect not propagated")
	}
	_, seq, gap, _ := c.Events(0, "a")
	if !gap || seq != maxEvents+2 {
		t.Fatalf("cursor %d gap %v", seq, gap)
	}
	if err := c.Call(context.Background(), "turn/start", nil, nil); err == nil {
		t.Fatal("closed connection accepted command")
	}
}
