package gw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// DefaultExecTimeout bounds a single remote corrald helper command.
var DefaultExecTimeout = 30 * time.Second

// runSSH is the single reviewed remote-execution call site in package gw.
// All callers pass fixed scripts built by commands.go with sshx.QuoteShell.
func runSSH(ctx context.Context, c *ssh.Client, cmd string, limit int, opt sshx.ExecOptions) (sshx.ExecResult, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultExecTimeout)
		defer cancel()
	}
	if limit <= 0 {
		limit = sshx.MaxOutput
	}
	opt.Interactive = true
	if opt.AwaitSlot == 0 {
		opt.AwaitSlot = 3 * time.Second
	}
	res, err := sshx.Exec(ctx, c, cmd, limit, opt)
	return res, err
}

// openSFTP opens an SFTP session on c under sshx's unified SessionSFTP ledger.
func openSFTP(ctx context.Context, c *ssh.Client) (*sftp.Client, func(), error) {
	releaseSlot, err := sshx.AcquireSession(ctx, c, sshx.SessionSFTP, true, 3*time.Second)
	if err != nil {
		return nil, nil, err
	}
	ch, reqs, err := c.OpenChannel("session", nil)
	if err != nil {
		releaseSlot()
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for r := range reqs {
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, ch.Stderr()) }()
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(&struct{ Name string }{"sftp"}))
	if err != nil || !ok {
		ch.Close()
		<-done
		releaseSlot()
		if err == nil {
			err = errors.New("host refused sftp subsystem")
		}
		return nil, nil, err
	}
	client, err := sftp.NewClientPipe(ch, ch)
	if err != nil {
		ch.Close()
		<-done
		releaseSlot()
		return nil, nil, err
	}
	cleanup := func() {
		_ = client.Close()
		_ = ch.Close()
		go func() {
			<-done
			releaseSlot()
		}()
	}
	return client, cleanup, nil
}

// sftpUploadAndVerify uploads localPath to remotePath (mode 0700), closes it,
// then reads remotePath back over SFTP and computes its SHA-256 digest on the
// gateway (no sha256sum dependency on the host).
func sftpUploadAndVerify(ctx context.Context, c *ssh.Client, localPath, remotePath string) (gotSHA256 string, uploadErr error, verifyErr error) {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return "", fmt.Errorf("open sftp: %w", err), nil
	}
	defer cleanup()

	src, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open local binary: %w", err), nil
	}
	defer src.Close()

	dst, err := sc.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return "", err, nil
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return "", err, nil
	}
	if err := dst.Chmod(0o700); err != nil {
		_ = dst.Close()
		return "", err, nil
	}
	if err := dst.Close(); err != nil {
		return "", err, nil
	}

	// Read back over SFTP and compute SHA-256 on the gateway.
	rd, err := sc.Open(remotePath)
	if err != nil {
		return "", nil, fmt.Errorf("readback open: %w", err)
	}
	defer rd.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rd); err != nil {
		return "", nil, fmt.Errorf("readback hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil, nil
}

// sftpReadFile reads a small state file (e.g. install.json, meta.json,
// failed.json, ops/<id>.json) via SFTP, capped at maxBytes.
func sftpReadFile(ctx context.Context, c *ssh.Client, remotePath string, maxBytes int64) ([]byte, error) {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	f, err := sc.Open(remotePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

// sftpWriteFile writes a small state file atomically-ish via SFTP (mode 0600).
func sftpWriteFile(ctx context.Context, c *ssh.Client, remotePath string, data []byte) error {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return err
	}
	defer cleanup()
	tmp := remotePath + ".tmp"
	f, err := sc.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = sc.Remove(tmp)
		return err
	}
	_ = f.Chmod(0o600)
	if err := f.Close(); err != nil {
		_ = sc.Remove(tmp)
		return err
	}
	_ = sc.Remove(remotePath)
	if err := sc.Rename(tmp, remotePath); err != nil {
		// Fallback direct overwrite if SFTP server does not support overwrite rename.
		f2, err2 := sc.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err2 != nil {
			_ = sc.Remove(tmp)
			return err
		}
		_, werr := f2.Write(data)
		_ = f2.Chmod(0o600)
		_ = f2.Close()
		_ = sc.Remove(tmp)
		return werr
	}
	return nil
}
