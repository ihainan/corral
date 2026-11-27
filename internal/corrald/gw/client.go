package gw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/corrald/proto"
	"github.com/ihainan/corral/internal/sshx"
)

var (
	// ErrClientClosed is returned when a call is made on a closed Client.
	ErrClientClosed = errors.New("corrald client closed")
	// ErrUnknownResult is returned when the outcome of a mutating RPC cannot be
	// confirmed (disconnect/timeout after send, op_expired, op_unknown, or epoch
	// change across reconnect), so the gateway must return 409
	// corrald_unknown_result and NEVER retry with the same op_id.
	ErrUnknownResult = errors.New("corrald write result unknown")
)

// DefaultCallTimeout bounds RPC calls when ctx carries no shorter deadline.
var DefaultCallTimeout = 15 * time.Second

// DefaultDialTimeout bounds opening the direct-streamlocal channel.
var DefaultDialTimeout = 10 * time.Second

// NewWriteOpID generates a protocol-compliant timestamped op_id for mutating
// RPC calls (pane.send, pane.answer, etc.) using proto.NewOpID.
func NewWriteOpID() string {
	return proto.NewOpID(time.Now())
}

// Client speaks the corrald framed JSON-RPC + event stream protocol over a
// single Unix domain socket connection forwarded via SSH direct-streamlocal.
type Client struct {
	conn   net.Conn
	wsem   chan struct{}
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan proto.Response
	closed  bool
	err     error
	done    chan struct{}

	eventMu sync.RWMutex
	onEvent func(proto.Event)

	metaMu  sync.RWMutex
	epoch   string
	lastSeq uint64
	hello   *proto.HelloResult
}

// Dial opens a direct-streamlocal@openssh.com channel to socketPath over sshClient
// and starts the Client frame reader loop.
func Dial(ctx context.Context, sshClient *ssh.Client, socketPath string, onEvent func(proto.Event)) (*Client, error) {
	if sshClient == nil {
		return nil, errors.New("ssh client is nil")
	}
	if socketPath == "" {
		return nil, errors.New("socket path is empty")
	}
	dctx, cancel := context.WithTimeout(ctx, DefaultDialTimeout)
	defer cancel()
	conn, err := sshx.DialVia(dctx, sshClient, "unix", socketPath, sshx.DialOptions{})
	if err != nil {
		return nil, fmt.Errorf("dial corrald socket %s: %w", socketPath, err)
	}
	return NewClient(conn, onEvent), nil
}

// NewClient wraps an already-established stream (used by Dial and unit tests).
func NewClient(conn net.Conn, onEvent func(proto.Event)) *Client {
	wsem := make(chan struct{}, 1)
	wsem <- struct{}{}
	c := &Client{
		conn:    conn,
		wsem:    wsem,
		pending: make(map[uint64]chan proto.Response),
		done:    make(chan struct{}),
		onEvent: onEvent,
	}
	go c.readLoop()
	return c
}

// SetEventHandler replaces the asynchronous event callback.
func (c *Client) SetEventHandler(fn func(proto.Event)) {
	c.eventMu.Lock()
	c.onEvent = fn
	c.eventMu.Unlock()
}

// Done is closed when the underlying connection terminates.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the transport error that closed the client, if any.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Closed reports whether the client connection is closed.
func (c *Client) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Cursor returns the latest observed daemon epoch and event sequence number.
func (c *Client) Cursor() (epoch string, lastSeq uint64) {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	return c.epoch, c.lastSeq
}

// LastHello returns the most recent daemon.hello result, if any.
func (c *Client) LastHello() *proto.HelloResult {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	if c.hello == nil {
		return nil
	}
	cp := *c.hello
	return &cp
}

// Close closes the underlying connection and unblocks all pending requests.
func (c *Client) Close() error {
	c.fail(ErrClientClosed)
	return c.conn.Close()
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err == nil {
		err = io.EOF
	}
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	close(c.done)
	c.mu.Unlock()
}

func (c *Client) readLoop() {
	for {
		var env proto.Envelope
		if err := proto.ReadJSON(c.conn, &env); err != nil {
			c.fail(err)
			_ = c.conn.Close()
			return
		}
		if env.IsEvent() {
			ev := env.AsEvent()
			c.metaMu.Lock()
			if ev.Epoch != "" && ev.Epoch != c.epoch {
				c.epoch = ev.Epoch
				c.lastSeq = ev.Seq
			} else if ev.Seq > c.lastSeq {
				c.lastSeq = ev.Seq
			}
			c.metaMu.Unlock()
			c.eventMu.RLock()
			fn := c.onEvent
			c.eventMu.RUnlock()
			if fn != nil {
				fn(ev)
			}
			continue
		}
		if env.IsResponse() {
			resp := env.AsResponse()
			c.mu.Lock()
			ch := c.pending[resp.ID]
			delete(c.pending, resp.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	}
}

// Call invokes a read-only or lifecycle RPC method on corrald and decodes the
// result into dst (if non-nil).
func (c *Client) Call(ctx context.Context, method string, params any, dst any) error {
	return c.call(ctx, method, params, dst, false)
}

// CallMutating invokes a write RPC (e.g. pane.send / pane.answer).
// If the connection drops or times out after the write begins, or if corrald
// responds with op_expired / op_unknown, ErrUnknownResult is returned so the
// caller returns 409 corrald_unknown_result and never blindly retries.
func (c *Client) CallMutating(ctx context.Context, method string, params any, dst any) error {
	return c.call(ctx, method, params, dst, true)
}

func (c *Client) call(ctx context.Context, method string, params any, dst any, mutating bool) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		rawParams = b
	}

	id := c.nextID.Add(1)
	ch := make(chan proto.Response, 1)

	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return err
		}
		return ErrClientClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	cleanupPending := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	// Acquire write slot while respecting ctx deadline/cancellation and client close.
	select {
	case <-c.wsem:
	case <-c.done:
		cleanupPending()
		if err := c.Err(); err != nil {
			return err
		}
		return ErrClientClosed
	case <-ctx.Done():
		cleanupPending()
		return ctx.Err()
	}

	if err := ctx.Err(); err != nil {
		c.wsem <- struct{}{}
		cleanupPending()
		return err
	}

	// Bound the write itself: set write deadline when supported and close the
	// underlying connection immediately if ctx expires while blocked on write
	// (e.g. SSH channel window exhausted because remote stopped reading).
	if dl, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(dl)
	}
	stopWriteWatch := context.AfterFunc(ctx, func() {
		c.fail(ctx.Err())
		_ = c.conn.Close()
	})
	werr := proto.WriteJSON(c.conn, proto.Request{
		ID:     id,
		Method: method,
		Params: rawParams,
	})
	stopWriteWatch()
	_ = c.conn.SetWriteDeadline(time.Time{})
	c.wsem <- struct{}{}

	if werr != nil {
		cleanupPending()
		c.fail(werr)
		_ = c.conn.Close()
		if mutating {
			return fmt.Errorf("%w: %v", ErrUnknownResult, werr)
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return werr
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			if mutating {
				return ErrUnknownResult
			}
			if err := c.Err(); err != nil {
				return err
			}
			return ErrClientClosed
		}
		if resp.Error != nil {
			if mutating && (resp.Error.Code == proto.ErrOpExpired || resp.Error.Code == proto.ErrOpUnknown) {
				return fmt.Errorf("%w: %w", ErrUnknownResult, resp.Error)
			}
			return resp.Error
		}
		if dst != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, dst); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		cleanupPending()
		if mutating {
			return fmt.Errorf("%w: %v", ErrUnknownResult, ctx.Err())
		}
		return ctx.Err()
	}
}

// Hello performs daemon.hello negotiation and records the daemon's epoch/seq.
func (c *Client) Hello(ctx context.Context, params proto.HelloParams) (*proto.HelloResult, error) {
	if params.ProtoMin == 0 {
		params.ProtoMin = proto.ProtoMin
	}
	if params.ProtoMax == 0 {
		params.ProtoMax = proto.ProtoMax
	}
	var res proto.HelloResult
	if err := c.Call(ctx, proto.MethodDaemonHello, params, &res); err != nil {
		return nil, err
	}
	c.metaMu.Lock()
	c.hello = &res
	if res.Epoch != "" && res.Epoch != c.epoch {
		c.epoch = res.Epoch
		c.lastSeq = res.Seq
	} else {
		if res.Epoch != "" {
			c.epoch = res.Epoch
		}
		if res.Seq > c.lastSeq {
			c.lastSeq = res.Seq
		}
	}
	c.metaMu.Unlock()
	return &res, nil
}

// Resume invokes daemon.resume to subscribe to events and replay missed frames.
func (c *Client) Resume(ctx context.Context, epoch string, lastSeq uint64) (*proto.ResumeResult, error) {
	var res proto.ResumeResult
	if err := c.Call(ctx, proto.MethodDaemonResume, proto.ResumeParams{
		Epoch:   epoch,
		LastSeq: lastSeq,
	}, &res); err != nil {
		return nil, err
	}
	c.metaMu.Lock()
	if res.Epoch != "" {
		c.epoch = res.Epoch
	}
	if res.CurrentSeq > c.lastSeq {
		c.lastSeq = res.CurrentSeq
	}
	c.metaMu.Unlock()
	return &res, nil
}

// Health queries daemon.health.
func (c *Client) Health(ctx context.Context) (*proto.HealthResult, error) {
	var res proto.HealthResult
	if err := c.Call(ctx, proto.MethodDaemonHealth, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Handoff triggers a zero-gap hot update to a staged/installed replacement binary.
func (c *Client) Handoff(ctx context.Context, params proto.HandoffParams) (*proto.HandoffResult, error) {
	var res proto.HandoffResult
	if err := c.Call(ctx, proto.MethodDaemonHandoff, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Shutdown requests graceful daemon exit.
func (c *Client) Shutdown(ctx context.Context, reason string) (*proto.ShutdownResult, error) {
	var res proto.ShutdownResult
	if err := c.Call(ctx, proto.MethodDaemonShutdown, proto.ShutdownParams{Reason: reason}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Forget unregisters a Corral user from the daemon's client registry.
func (c *Client) Forget(ctx context.Context, params proto.ForgetParams) (*proto.ForgetResult, error) {
	var res proto.ForgetResult
	if err := c.Call(ctx, proto.MethodDaemonForget, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Locate invokes agent.locate on corrald and returns the raw NUL-delimited
// output matching convoLocateWith for parsing with parseConvoLocate.
func (c *Client) Locate(ctx context.Context, params proto.AgentLocateParams) ([]byte, error) {
	var res proto.AgentLocateResult
	if err := c.Call(ctx, proto.MethodAgentLocate, params, &res); err != nil {
		return nil, err
	}
	return res.Out, nil
}

// Tails invokes convo.tails on corrald and returns the raw NUL-delimited tail
// output matching convoTailsCommand for parsing with splitTails.
func (c *Client) Tails(ctx context.Context, logs []string) ([]byte, error) {
	var res proto.ConvoTailsResult
	if err := c.Call(ctx, proto.MethodConvoTails, proto.ConvoTailsParams{Logs: logs}, &res); err != nil {
		return nil, err
	}
	return res.Out, nil
}

// MessageTails invokes convo.message_tails on corrald and returns the raw
// NUL-delimited filtered tail output matching convoMessageTailsCommand.
func (c *Client) MessageTails(ctx context.Context, logs []string) ([]byte, error) {
	var res proto.ConvoTailsResult
	if err := c.Call(ctx, proto.MethodConvoMessageTails, proto.ConvoTailsParams{Logs: logs}, &res); err != nil {
		return nil, err
	}
	return res.Out, nil
}

// PaneSend invokes the mutating, op_id-idempotent pane.send RPC on corrald.
// If the connection fails or times out after the request write begins,
// ErrUnknownResult is returned so the caller returns 409 corrald_unknown_result
// and never falls back to SSH.
func (c *Client) PaneSend(ctx context.Context, params proto.PaneSendParams) (*proto.PaneSendResult, error) {
	var res proto.PaneSendResult
	if err := c.CallMutating(ctx, proto.MethodPaneSend, params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
