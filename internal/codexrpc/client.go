// Package codexrpc speaks Codex App Server RPC without starting a daemon,
// changing credentials, or retrying commands with unknown outcomes.
package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	ErrClosed = errors.New("Codex connection closed")
	ErrBusy   = errors.New("too many pending Codex requests")
	ErrStale  = errors.New("Codex request is no longer pending")
)

// Unknown means the peer may have accepted a command. Reconcile, never retry.
type Unknown struct{ Cause error }

func (e *Unknown) Error() string { return "Codex request outcome is unknown" }
func (e *Unknown) Unwrap() error { return e.Cause }

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("Codex rejected request (%d)", e.Code) }

type message struct {
	seq    uint64
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type Event struct {
	Seq       uint64          `json:"seq"`
	Method    string          `json:"method"`
	Thread    string          `json:"thread"`
	Params    json.RawMessage `json:"params"`
	RequestID json.RawMessage `json:"request_id,omitempty"`
}

const maxEvents = 512
const maxEventBytes = 4 << 20

type Client struct {
	conn         *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	write        chan struct{}
	done         chan struct{}
	mu           sync.Mutex
	next         int64
	pending      map[int64]chan message
	requests     map[string]Event
	requestBytes int
	events       []Event
	bytes        int
	seq          uint64
	changed      chan struct{}
}

// HTTPClient may open a remote Unix socket through an authenticated SSH
// channel. URL is a fixed control endpoint, never a user-provided address.
func Dial(ctx context.Context, url string, httpClient *http.Client) (*Client, error) {
	w, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, err
	}
	w.SetReadLimit(16 << 20)
	life, cancel := context.WithCancel(context.Background())
	c := &Client{conn: w, ctx: life, cancel: cancel, write: make(chan struct{}, 1), done: make(chan struct{}), pending: map[int64]chan message{}, requests: map[string]Event{}, changed: make(chan struct{})}
	go c.read()
	err = c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "corral", "title": "Corral", "version": "0.2"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil)
	if err == nil {
		err = c.send(ctx, message{Method: "initialized", Params: json.RawMessage(`{}`)})
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Close()                { c.cancel(); c.conn.CloseNow() }

func (c *Client) send(ctx context.Context, m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	select {
	case c.write <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
	defer func() { <-c.write }()
	return c.conn.Write(ctx, websocket.MessageText, b)
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	_, err := c.CallAt(ctx, method, params, result)
	return err
}

// CallAt returns the event cursor at the response's position on the wire.
// A snapshot can replay subsequent deltas without appending earlier ones twice.
func (c *Client) CallAt(ctx context.Context, method string, params, result any) (uint64, error) {
	b, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	if len(c.pending) >= 64 {
		c.mu.Unlock()
		return 0, ErrBusy
	}
	c.next++
	id := c.next
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err = c.send(ctx, message{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: b}); err != nil {
		return 0, &Unknown{err}
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.seq, m.Error
		}
		if result == nil {
			return m.seq, nil
		}
		return m.seq, json.Unmarshal(m.Result, result)
	case <-ctx.Done():
		return 0, &Unknown{ctx.Err()}
	case <-c.done:
		return 0, &Unknown{ErrClosed}
	}
}

func eventThread(params json.RawMessage) string {
	var p struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ThreadID != "" {
		return p.ThreadID
	}
	return p.Thread.ID
}

func approval(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput", "item/permissions/requestApproval", "mcpServer/elicitation/request":
		return true
	}
	return false
}

func (c *Client) read() {
	defer func() { c.cancel(); c.conn.CloseNow(); close(c.done) }()
	for {
		_, b, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(b, &m) != nil {
			return
		}
		if m.Method == "" {
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			m.seq = c.seq
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- m:
				default:
				}
			}
			continue
		}
		thread := eventThread(m.Params)
		if len(m.ID) > 0 && (!approval(m.Method) || thread == "" || len(m.ID) > 128) {
			ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
			err = c.send(ctx, message{ID: m.ID, Error: &RPCError{Code: -32601, Message: "This client does not handle this request"}})
			cancel()
			if err != nil {
				return
			}
			continue
		}
		if thread == "" || (!strings.HasPrefix(m.Method, "thread/") && !strings.HasPrefix(m.Method, "turn/") && !strings.HasPrefix(m.Method, "item/") && m.Method != "serverRequest/resolved" && m.Method != "mcpServer/elicitation/request" && m.Method != "error") {
			continue
		}
		if len(m.Params) > maxEventBytes {
			return
		}
		c.mu.Lock()
		if len(m.ID) > 0 && (len(c.requests) >= 128 || c.requestBytes+len(m.Params) > maxEventBytes) {
			c.mu.Unlock()
			return
		}
		if m.Method == "serverRequest/resolved" {
			var p struct {
				ID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			c.requestBytes -= len(c.requests[string(p.ID)].Params)
			delete(c.requests, string(p.ID))
		}
		c.seq++
		e := Event{Seq: c.seq, Method: m.Method, Thread: thread, Params: m.Params, RequestID: m.ID}
		if len(m.ID) > 0 {
			c.requestBytes -= len(c.requests[string(m.ID)].Params)
			c.requests[string(m.ID)] = e
			c.requestBytes += len(m.Params)
		}
		c.events = append(c.events, e)
		c.bytes += len(m.Params)
		for len(c.events) > maxEvents || c.bytes > maxEventBytes {
			c.bytes -= len(c.events[0].Params)
			c.events[0] = Event{}
			c.events = c.events[1:]
		}
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
	}
}

// A replay gap requires a fresh snapshot, never silently dropping updates.
func (c *Client) Events(after uint64, thread string) (events []Event, seq uint64, gap bool, changed <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gap = after > c.seq || (len(c.events) > 0 && after < c.events[0].Seq-1)
	for _, e := range c.events {
		if e.Seq > after && e.Thread == thread {
			events = append(events, e)
		}
	}
	return events, c.seq, gap, c.changed
}

func (c *Client) Requests(thread string) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for _, r := range c.requests {
		if r.Thread == thread {
			out = append(out, r)
		}
	}
	return out
}

// Consume before writing so competing pages cannot answer the same request.
func (c *Client) Reply(ctx context.Context, thread string, id json.RawMessage, build func(Event) (any, error)) error {
	c.mu.Lock()
	e, ok := c.requests[string(id)]
	if !ok || e.Thread != thread {
		c.mu.Unlock()
		return ErrStale
	}
	result, err := build(e)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	delete(c.requests, string(id))
	c.requestBytes -= len(e.Params)
	c.mu.Unlock()
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err = c.send(ctx, message{ID: id, Result: b}); err != nil {
		return &Unknown{err}
	}
	return nil
}
