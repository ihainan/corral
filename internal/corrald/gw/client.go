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
	// ErrUnknownResult is returned when the connection drops while a mutating
	// RPC (e.g. pane.send / pane.answer) is in flight, so the gateway must NOT
	// blindly retry or fall back to re-sending (consent-design "结果未确认的写操作").
	ErrUnknownResult = errors.New("corrald write result unknown after disconnect")
)

// DefaultCallTimeout bounds RPC calls when ctx carries no shorter deadline.
var DefaultCallTimeout = 15 * time.Second

// DefaultDialTimeout bounds opening the direct-streamlocal channel.
var DefaultDialTimeout = 10 * time.Second

// Client speaks the corrald framed JSON-RPC + event stream protocol over a
// single Unix domain socket connection forwarded via SSH direct-streamlocal.
type Client struct {
	conn   net.Conn
	wmu    sync.Mutex
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
	c := &Client{
		conn:    conn,
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
			if ev.Epoch != "" {
				c.epoch = ev.Epoch
			}
			if ev.Seq > c.lastSeq {
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

// CallMutating invokes a write RPC (e.g. pane.send / pane.answer). If the
// connection drops after the request was written and before a reply arrives,
// ErrUnknownResult is returned so the caller never blindly re-sends.
func (c *Client) CallMutating(ctx context.Context, method string, params any, dst any) error {
	return c.call(ctx, method, params, dst, true)
}

func (c *Client) call(ctx context.Context, method string, params any, dst any, mutating bool) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
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

	c.wmu.Lock()
	werr := proto.WriteJSON(c.conn, proto.Request{
		ID:     id,
		Method: method,
		Params: rawParams,
	})
	c.wmu.Unlock()
	if werr != nil {
		cleanupPending()
		c.fail(werr)
		_ = c.conn.Close()
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
	if res.Epoch != "" {
		c.epoch = res.Epoch
	}
	if res.Seq > c.lastSeq {
		c.lastSeq = res.Seq
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
