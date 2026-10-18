package sshx

import (
	"context"
	"errors"
	"io"

	"golang.org/x/crypto/ssh"
)

// MaxOutput caps what Output keeps from a remote command. Discovery commands
// print a few kilobytes; a host printing more is broken or hostile.
var MaxOutput = 1 << 20

var ErrOutputTooLarge = errors.New("remote command printed too much output")

// Output runs a fixed command over c as a side operation (see MaxAuxPerConn)
// and returns its stdout; stderr is discarded. Past limit bytes it stops
// keeping output and closes the channel. It returns when the command is done
// or ctx ends; in the latter case the channel is closed and the operation
// keeps its place against MaxAuxPerConn until the host has really let go of
// it. The connection itself is never closed: it may carry a terminal.
func Output(ctx context.Context, c *ssh.Client, cmd string, limit int) ([]byte, error) {
	release, ok := takeAux(c)
	if !ok {
		return nil, ErrBusy
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	type result struct {
		out []byte
		err error
	}
	res := make(chan result, 1)
	go func() {
		defer release()
		out, err := runCommand(ctx, c, cmd, limit)
		res <- result{out, err}
	}()
	select {
	case r := <-res:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runCommand returns only once the session channel has ended.
func runCommand(ctx context.Context, c *ssh.Client, cmd string, limit int) ([]byte, error) {
	s, err := c.NewSession() // waits for the host to answer the open, or for the connection to end
	if err != nil {
		return nil, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { s.Close() }) // giving up: ask the host to close
	defer stop()
	stdout, err := s.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := s.Start(cmd); err != nil {
		s.Close()
		io.Copy(io.Discard, stdout) // returns once the channel has ended
		return nil, err
	}
	var buf []byte
	over := false
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := stdout.Read(chunk)
		if n > 0 && !over {
			if len(buf)+n > limit {
				over = true
				s.Close() // keep reading (and discarding) until the channel ends
			} else {
				buf = append(buf, chunk[:n]...)
			}
		}
		if rerr != nil {
			break
		}
	}
	err = s.Wait() // the channel's requests end with the channel
	switch {
	case over:
		return buf, ErrOutputTooLarge
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	return buf, err
}
