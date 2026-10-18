package sshx

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/crypto/ssh"
)

// MaxOutput caps what Output keeps from a remote command. Discovery commands
// print a few kilobytes; a host printing more is broken or hostile.
var MaxOutput = 1 << 20

var ErrOutputTooLarge = errors.New("remote command printed too much output")

// Output runs a fixed command over c and returns its stdout; stderr is
// discarded. It stops reading, closing the channel, once the output passes
// limit bytes. When ctx ends it closes the channel, and if the host does not
// let go (channel open, exec or close unanswered) closes c after StuckGrace:
// Output only returns once nothing of it is left running.
func Output(ctx context.Context, c *ssh.Client, cmd string, limit int) ([]byte, error) {
	outer := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var sess *ssh.Session
	finished := watch(ctx, c, func() {
		mu.Lock()
		s := sess
		mu.Unlock()
		if s != nil {
			s.Close()
		}
	})
	defer finished()
	s, err := c.NewSession()
	if err != nil {
		return nil, orCtx(outer, err)
	}
	mu.Lock()
	sess = s
	mu.Unlock()
	// Runs before finished; cancelling first puts the close under the watch
	// too, in case the host never answers it.
	defer func() { cancel(); s.Close() }()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	w := &capWriter{limit: limit, full: cancel} // too much: end it like a timeout
	s.Stdout = w
	if err := s.Start(cmd); err != nil {
		return nil, orCtx(outer, err)
	}
	err = s.Wait()
	switch {
	case w.over:
		return w.buf, ErrOutputTooLarge
	case outer.Err() != nil:
		return nil, outer.Err()
	}
	return w.buf, err
}

// orCtx prefers the context's error: a failure caused by giving up is reported as such.
func orCtx(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// capWriter keeps up to limit bytes and fails (closing the channel) beyond.
type capWriter struct {
	buf   []byte
	limit int
	over  bool
	full  func()
}

func (w *capWriter) Write(p []byte) (int, error) {
	if len(w.buf)+len(p) > w.limit {
		if !w.over {
			w.over = true
			w.full()
		}
		return 0, ErrOutputTooLarge
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}
