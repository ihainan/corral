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
	return Exec(ctx, c, cmd, limit, ExecOptions{})
}

// ExecOptions add to Output.
type ExecOptions struct {
	Stdin   io.Reader         // copied to the command's input, which is then closed
	OnLine  func(line string) // each output line as it arrives (from another goroutine)
	Release func()            // a further quota, given back once the channel has really ended
}

// Exec is Output with options; see there.
func Exec(ctx context.Context, c *ssh.Client, cmd string, limit int, opt ExecOptions) ([]byte, error) {
	extra := oneShot(opt.Release)
	aux, ok := takeAux(c)
	if !ok {
		extra()
		return nil, ErrBusy
	}
	release := oneShot(func() { aux(); extra() })
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
		out, err := runCommand(ctx, c, cmd, limit, opt)
		res <- result{out, err}
	}()
	select {
	case r := <-res:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runCommand returns only once the session channel has really ended: its
// request stream is closed (the host closed the channel, or the connection
// ended) and the goroutines reading it have returned. It opens the channel
// itself rather than through ssh.Session, which hides that stream.
func runCommand(ctx context.Context, c *ssh.Client, cmd string, limit int, opt ExecOptions) ([]byte, error) {
	ch, reqs, err := c.OpenChannel("session", nil) // waits for the host to answer, or for the connection to end
	if err != nil {
		return nil, err
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
			return nil, ctx.Err()
		}
		if err == nil {
			err = errors.New("the host refused to run the command")
		}
		return nil, err
	}
	if opt.Stdin != nil {
		wg.Add(1)
		go func() { // ends with the input, or when the channel does
			defer wg.Done()
			io.Copy(ch, opt.Stdin)
			ch.CloseWrite()
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
	// (the cancel above, or going over the limit).
	wg.Wait()
	ch.Close()
	switch {
	case over:
		return buf, ErrOutputTooLarge
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case exitStatus < 0:
		return buf, errors.New("the remote command ended without an exit status")
	case exitStatus > 0:
		return buf, fmt.Errorf("the remote command exited with status %d", exitStatus)
	}
	return buf, nil
}
