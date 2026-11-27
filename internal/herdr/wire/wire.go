// Package wire implements the transport-agnostic newline-delimited JSON RPC
// protocol spoken by a herdr server over a Unix domain socket (either directly
// from corrald on the host, or forwarded over SSH direct-streamlocal from the
// Corral gateway).
package wire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

const maxLine = 4 << 20

// DefaultCallTimeout bounds one request/response exchange when the caller does
// not pass an explicit duration via CallWithin.
var DefaultCallTimeout = 10 * time.Second

// APIError is an error reported by herdr itself (for example an unknown method
// or missing pane), as opposed to a transport failure.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string { return "herdr: " + e.Code + ": " + e.Message }

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Conn speaks herdr's newline-delimited JSON RPC protocol over an established
// stream connection.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
	seq  atomic.Int64
}

// NewConn wraps an established connection to a herdr Unix socket.
func NewConn(conn net.Conn) *Conn {
	return &Conn{
		conn: conn,
		r:    bufio.NewReaderSize(conn, 64<<10),
	}
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.conn.Close() }

// ReadLine reads one newline-delimited JSON frame up to maxLine bytes.
func (c *Conn) ReadLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, errors.New("herdr: response too large")
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// Call sends one request and waits for its response, for at most
// DefaultCallTimeout or until ctx ends.
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	return c.CallWithin(ctx, DefaultCallTimeout, method, params, out)
}

// CallWithin is Call with an explicit timeout bound on the exchange.
func (c *Conn) CallWithin(ctx context.Context, within time.Duration, method string, params any, out any) error {
	if params == nil {
		params = struct{}{}
	}
	id := strconv.FormatInt(c.seq.Add(1), 10)
	b, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stop()
	err = c.exchange(id, b, out)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("herdr: %s: %w", method, ctx.Err())
	}
	return err
}

func (c *Conn) exchange(id string, req []byte, out any) error {
	if _, err := c.conn.Write(append(req, '\n')); err != nil {
		return err
	}
	for {
		line, err := c.ReadLine()
		if err != nil {
			return err
		}
		var r response
		if err := json.Unmarshal(line, &r); err != nil {
			return fmt.Errorf("herdr: bad response: %w", err)
		}
		if r.ID != id {
			continue // an event or a stale reply
		}
		if r.Error != nil {
			return &APIError{Code: r.Error.Code, Message: r.Error.Message}
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

// ReadPane returns the visible text of a pane (ANSI stripped).
func (c *Conn) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	return c.ReadPaneSource(ctx, paneID, "visible", lines)
}

// ReadPaneSource reads pane text from the specified herdr source with ANSI
// escape sequences stripped.
func (c *Conn) ReadPaneSource(ctx context.Context, paneID, source string, lines int) (string, error) {
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": source, "strip_ansi": true}
	if lines > 0 {
		params["lines"] = lines
	}
	if err := c.Call(ctx, "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

// ReadPaneANSI reads pane text with ANSI color escape sequences intact.
func (c *Conn) ReadPaneANSI(ctx context.Context, paneID, source string, lines int) (string, error) {
	var res struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	params := map[string]any{"pane_id": paneID, "source": source, "format": "ansi"}
	if lines > 0 {
		params["lines"] = lines
	}
	if err := c.Call(ctx, "pane.read", params, &res); err != nil {
		return "", err
	}
	return res.Read.Text, nil
}

// GetPaneTitle queries pane.get and returns the raw terminal_title string and
// whether the terminal_title field was present as a JSON string.
func (c *Conn) GetPaneTitle(ctx context.Context, paneID string) (title string, hasTitle bool, err error) {
	var res struct {
		Pane *struct {
			TerminalTitle *string `json:"terminal_title"`
		} `json:"pane"`
		TerminalTitle *string `json:"terminal_title"`
	}
	if err := c.Call(ctx, "pane.get", map[string]any{"pane_id": paneID}, &res); err != nil {
		return "", false, err
	}
	if res.Pane != nil && res.Pane.TerminalTitle != nil {
		return *res.Pane.TerminalTitle, true, nil
	}
	if res.TerminalTitle != nil {
		return *res.TerminalTitle, true, nil
	}
	return "", false, nil
}

// SendKeys presses one or more named keys in paneID via pane.send_keys.
func (c *Conn) SendKeys(ctx context.Context, paneID string, keys []string) error {
	return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

// SendText types literal text into paneID via pane.send_text.
func (c *Conn) SendText(ctx context.Context, paneID, text string) error {
	return c.Call(ctx, "pane.send_text", map[string]any{"pane_id": paneID, "text": text}, nil)
}

// SendInput types text into paneID, then presses keys, via pane.send_input.
func (c *Conn) SendInput(ctx context.Context, paneID, text string, keys []string) error {
	return c.Call(ctx, "pane.send_input", map[string]any{"pane_id": paneID, "text": text, "keys": keys}, nil)
}

// ReadEvents reads raw newline-delimited event JSON frames until ctx ends or
// the connection closes.
func (c *Conn) ReadEvents(ctx context.Context, fn func(event json.RawMessage)) error {
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stop()
	for {
		line, err := c.ReadLine()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(json.RawMessage(line))
	}
}
