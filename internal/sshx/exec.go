package sshx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

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
	r, err := Exec(ctx, c, cmd, limit, ExecOptions{})
	return r.Out, err
}

// ExecOptions add to Output.
type ExecOptions struct {
	// Stdin is copied to the command's input, which is closed after it. A
	// failure to read it aborts the command: its input is not closed as if it
	// were complete. Reading must not block indefinitely; only the channel
	// ending stops the copy, and the channel's quota is held until then.
	Stdin   io.Reader
	OnLine  func(line string) // each output line as it arrives (from another goroutine)
	Release func()            // a further quota, given back once the channel has really ended
}

// ExecResult is what Exec learnt about the command.
type ExecResult struct {
	Out []byte
	// Ended: the command ran to its end on the host (its exit status came),
	// whatever the status. When false (cancelled, timed out, connection lost,
	// too much output) the command may still be running there.
	Ended bool
}

// ExitError is a command's non-zero exit status.
type ExitError struct{ Status int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("the remote command exited with status %d", e.Status)
}

// Exec is Output with options; see there.
func Exec(ctx context.Context, c *ssh.Client, cmd string, limit int, opt ExecOptions) (ExecResult, error) {
	extra := oneShot(opt.Release)
	aux, ok := takeAux(c)
	if !ok {
		extra()
		return ExecResult{}, ErrBusy
	}
	release := oneShot(func() { aux(); extra() })
	if err := ctx.Err(); err != nil {
		release()
		return ExecResult{}, err
	}
	type result struct {
		r   ExecResult
		err error
	}
	res := make(chan result, 1)
	go func() {
		defer release()
		r, err := runCommand(ctx, c, cmd, limit, opt)
		res <- result{r, err}
	}()
	select {
	case r := <-res:
		return r.r, r.err
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
}

// runCommand returns only once the session channel has really ended: its
// request stream is closed (the host closed the channel, or the connection
// ended) and the goroutines reading it have returned. It opens the channel
// itself rather than through ssh.Session, which hides that stream.
func runCommand(ctx context.Context, c *ssh.Client, cmd string, limit int, opt ExecOptions) (ExecResult, error) {
	ch, reqs, err := c.OpenChannel("session", nil) // waits for the host to answer, or for the connection to end
	if err != nil {
		return ExecResult{}, err
	}
	exitStatus := -1 // written by the request reader, read after it returned
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for r := range reqs {
			if r.Type == "exit-status" && len(r.Payload) >= 4 {
				exitStatus = int(binary.BigEndian.Uint32(r.Payload))
			}
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(io.Discard, ch.Stderr())
	}()
	// Giving up (or a refused command) asks the host to close, then waits for
	// the channel to end, however long the host takes.
	end := func() {
		ch.Close()
		io.Copy(io.Discard, ch)
		wg.Wait()
	}
	stop := context.AfterFunc(ctx, func() { ch.Close() })
	defer stop()
	ok, err := ch.SendRequest("exec", true, ssh.Marshal(&struct{ Command string }{cmd}))
	if err != nil || !ok {
		end()
		if ctx.Err() != nil {
			return ExecResult{}, ctx.Err()
		}
		if err == nil {
			err = errors.New("the host refused to run the command")
		}
		return ExecResult{}, err
	}
	var readErr, writeErr error // the input copy's, set before its wg.Done
	if opt.Stdin != nil {
		wg.Add(1)
		go func() { // ends with the input, or when the channel does
			defer wg.Done()
			buf := make([]byte, 32<<10)
			for {
				n, rerr := opt.Stdin.Read(buf)
				if n > 0 {
					if _, werr := ch.Write(buf[:n]); werr != nil {
						writeErr = werr // the host stopped reading; its answer says why
						return
					}
				}
				if rerr == io.EOF {
					ch.CloseWrite()
					return
				}
				if rerr != nil {
					readErr = rerr
					ch.Close() // abort: a cut-off input must not pass for all of it
					return
				}
			}
		}()
	} else {
		ch.CloseWrite()
	}
	var buf []byte
	over := false
	lineStart := 0
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := ch.Read(chunk)
		if n > 0 && !over {
			if len(buf)+n > limit {
				over = true
				ch.Close() // keep reading (and discarding) until the channel ends
			} else {
				buf = append(buf, chunk[:n]...)
				for opt.OnLine != nil {
					i := bytes.IndexByte(buf[lineStart:], '\n')
					if i < 0 {
						break
					}
					opt.OnLine(string(buf[lineStart : lineStart+i]))
					lineStart += i + 1
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	// Data EOF is not the end: the host may still send the exit status. Wait
	// for the channel to close on its own; only giving up closes it actively
	// (the cancel above, going over the limit, or a failed input).
	wg.Wait()
	ch.Close()
	r := ExecResult{Out: buf, Ended: exitStatus >= 0 && !over}
	switch {
	case over:
		return r, ErrOutputTooLarge
	case readErr != nil:
		return r, fmt.Errorf("reading the command's input: %w", readErr)
	case ctx.Err() != nil:
		return ExecResult{}, ctx.Err()
	case exitStatus < 0:
		return r, errors.New("the remote command ended without an exit status")
	case exitStatus > 0:
		return r, &ExitError{Status: exitStatus}
	case writeErr != nil:
		return r, fmt.Errorf("sending the command's input: %w", writeErr)
	}
	return r, nil
}
