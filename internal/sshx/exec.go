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

// Output runs a fixed command over c and returns its stdout. It gives up when
// ctx ends (also while the channel is still being opened) and stops reading,
// closing the channel, once the output passes limit bytes. Stderr is
// discarded.
func Output(ctx context.Context, c *ssh.Client, cmd string, limit int) ([]byte, error) {
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	var mu sync.Mutex
	var sess *ssh.Session
	abandoned := false
	go func() {
		s, err := c.NewSession()
		if err != nil {
			done <- result{err: err}
			return
		}
		mu.Lock()
		if abandoned {
			mu.Unlock()
			s.Close()
			return
		}
		sess = s
		mu.Unlock()
		defer s.Close()
		w := &capWriter{limit: limit, full: func() { s.Close() }}
		s.Stdout = w
		if err := s.Start(cmd); err != nil {
			done <- result{err: err}
			return
		}
		err = s.Wait()
		if w.over {
			err = ErrOutputTooLarge
		}
		done <- result{out: w.buf, err: err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		mu.Lock()
		abandoned = true
		if sess != nil {
			sess.Close()
		}
		mu.Unlock()
		return nil, ctx.Err()
	}
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
