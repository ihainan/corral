package gw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/sshx"
)

// DefaultExecTimeout bounds a single remote corrald helper command.
var DefaultExecTimeout = 30 * time.Second

// DefaultSFTPTimeout bounds an SFTP session setup + file transfer when ctx
// carries no shorter deadline.
var DefaultSFTPTimeout = 60 * time.Second

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
// The SSH channel is wired to ctx via context.AfterFunc so cancellation or
// deadline expiry immediately closes the underlying channel, unblocking
// SendRequest, sftp.NewClientPipe handshake, and any in-flight reads/writes.
func openSFTP(ctx context.Context, c *ssh.Client) (*sftp.Client, func(), error) {
	var cancelTimeout context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ctx, cancelTimeout = context.WithTimeout(ctx, DefaultSFTPTimeout)
	}
	cancelIf := func() {
		if cancelTimeout != nil {
			cancelTimeout()
		}
	}

	releaseSlot, err := sshx.AcquireSession(ctx, c, sshx.SessionSFTP, true, 3*time.Second)
	if err != nil {
		cancelIf()
		return nil, nil, err
	}

	type openRes struct {
		ch   ssh.Channel
		reqs <-chan *ssh.Request
		err  error
	}
	openCh := make(chan openRes, 1)
	go func() {
		ch, reqs, err := c.OpenChannel("session", nil)
		openCh <- openRes{ch: ch, reqs: reqs, err: err}
	}()

	var (
		ch   ssh.Channel
		reqs <-chan *ssh.Request
	)
	select {
	case r := <-openCh:
		if r.err != nil {
			releaseSlot()
			cancelIf()
			return nil, nil, r.err
		}
		ch, reqs = r.ch, r.reqs
	case <-ctx.Done():
		go func() {
			r := <-openCh
			if r.err == nil && r.ch != nil {
				_ = r.ch.Close()
				for req := range r.reqs {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}
			}
			releaseSlot()
		}()
		cancelIf()
		return nil, nil, ctx.Err()
	}

	// Ensure any context cancellation or deadline closes the SSH channel
	// immediately, unblocking subsystem request, SFTP handshake, and I/O.
	stopWatch := context.AfterFunc(ctx, func() {
		_ = ch.Close()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, ch.Stderr()) }()

	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(&struct{ Name string }{"sftp"}))
	if err != nil || !ok {
		stopWatch()
		_ = ch.Close()
		go func() {
			<-done
			releaseSlot()
		}()
		cancelIf()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err == nil {
			err = errors.New("host refused sftp subsystem")
		}
		return nil, nil, err
	}

	client, err := sftp.NewClientPipe(ch, ch)
	if err != nil {
		stopWatch()
		_ = ch.Close()
		go func() {
			<-done
			releaseSlot()
		}()
		cancelIf()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, err
	}

	cleanup := func() {
		stopWatch()
		_ = client.Close()
		_ = ch.Close()
		cancelIf()
		go func() {
			<-done
			releaseSlot()
		}()
	}
	return client, cleanup, nil
}

// verifySFTPDirChain walks every component of targetDir starting under home
// using Lstat, rejecting symlinks, non-directories, and unexpected owners.
// When mustExist is false, missing leaf components are permitted (e.g. checking
// ~/.local/lib/corral before corrald install creates it).
func verifySFTPDirChain(sc *sftp.Client, home string, expectedUID int64, targetDir string, mustExist bool) error {
	cleanHome := path.Clean(home)
	cleanTarget := path.Clean(targetDir)
	if !strings.HasPrefix(cleanHome, "/") || !strings.HasPrefix(cleanTarget, cleanHome+"/") {
		return fmt.Errorf("unsafe path outside home: %s", targetDir)
	}
	rel := strings.TrimPrefix(cleanTarget, cleanHome+"/")
	parts := strings.Split(rel, "/")
	cur := cleanHome
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("unsafe path component in %s", targetDir)
		}
		cur = cur + "/" + part
		info, err := sc.Lstat(cur)
		if err != nil {
			if !mustExist && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("lstat %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe symlink at %s", cur)
		}
		if !info.IsDir() {
			return fmt.Errorf("unsafe non-directory at %s", cur)
		}
		if expectedUID >= 0 {
			if st, ok := info.Sys().(*sftp.FileStat); ok && int64(st.UID) != expectedUID {
				return fmt.Errorf("unsafe owner %d on %s (want %d)", st.UID, cur, expectedUID)
			}
		}
	}
	return nil
}

// sftpUploadBinarySecure verifies the directory chain (~/.local/lib/corral and
// stageDir) via Lstat, uploads localPath to a freshly created (O_CREATE|O_EXCL)
// temp file inside stageDir, chmods it 0700, checks remotePath is not a
// symlink, and renames it into place.
func sftpUploadBinarySecure(ctx context.Context, c *ssh.Client, localPath, home string, expectedUID int64, libDir, stageDir, remotePath string) error {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return fmt.Errorf("open sftp: %w", err)
	}
	defer cleanup()

	if libDir != "" && home != "" {
		if err := verifySFTPDirChain(sc, home, expectedUID, libDir, false); err != nil {
			return err
		}
	}
	if err := verifySFTPDirChain(sc, home, expectedUID, stageDir, true); err != nil {
		return err
	}

	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local binary: %w", err)
	}
	defer src.Close()

	tmpPath := stageDir + "/.corrald.upload." + newHexID()
	dst, err := sc.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	removeTmp := func() { _ = sc.Remove(tmpPath) }
	buf := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			_ = dst.Close()
			removeTmp()
			return ctx.Err()
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				_ = dst.Close()
				removeTmp()
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = dst.Close()
			removeTmp()
			return rerr
		}
	}
	if err := dst.Chmod(0o700); err != nil {
		_ = dst.Close()
		removeTmp()
		return err
	}
	if err := dst.Close(); err != nil {
		removeTmp()
		return err
	}
	if info, lerr := sc.Lstat(remotePath); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			removeTmp()
			return fmt.Errorf("unsafe existing target at %s", remotePath)
		}
		_ = sc.Remove(remotePath)
	}
	if err := sc.Rename(tmpPath, remotePath); err != nil {
		removeTmp()
		return err
	}
	return nil
}

// sftpReadbackSHA256 reads remotePath back over SFTP and computes its SHA-256
// digest on the gateway (no sha256sum dependency on the host).
func sftpReadbackSHA256(ctx context.Context, c *ssh.Client, remotePath string) (string, error) {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return "", fmt.Errorf("open sftp: %w", err)
	}
	defer cleanup()

	if info, lerr := sc.Lstat(remotePath); lerr != nil {
		return "", lerr
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("unsafe staged file %s", remotePath)
	}

	rd, err := sc.Open(remotePath)
	if err != nil {
		return "", err
	}
	defer rd.Close()
	h := sha256.New()
	buf := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, rerr := rd.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sftpReadFile reads a small state file (e.g. install.json, meta.json,
// failed.json, clients.json, ops/<id>.json) via SFTP, capped at maxBytes.
func sftpReadFile(ctx context.Context, c *ssh.Client, remotePath string, maxBytes int64) ([]byte, error) {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if info, lerr := sc.Lstat(remotePath); lerr != nil {
		return nil, lerr
	} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("unsafe file %s", remotePath)
	}
	f, err := sc.Open(remotePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

// sftpWriteFile writes a small state file safely via SFTP (mode 0600) using
// O_CREATE|O_EXCL temporary file + atomic rename after verifying parent dirs
// are not symlinks and owned by expectedUID.
func sftpWriteFile(ctx context.Context, c *ssh.Client, home string, expectedUID int64, remotePath string, data []byte) error {
	sc, cleanup, err := openSFTP(ctx, c)
	if err != nil {
		return err
	}
	defer cleanup()
	parentDir := path.Dir(remotePath)
	if home != "" {
		if err := verifySFTPDirChain(sc, home, expectedUID, parentDir, true); err != nil {
			return err
		}
	}
	if info, lerr := sc.Lstat(remotePath); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return fmt.Errorf("unsafe target %s", remotePath)
		}
	}
	tmp := parentDir + "/." + path.Base(remotePath) + ".tmp." + newHexID()
	f, err := sc.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
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
	if err := sc.Rename(tmp, remotePath); err != nil {
		// Some SFTP servers refuse Rename over an existing file; remove only
		// after re-verifying remotePath is not a symlink, then retry Rename.
		if info, lerr := sc.Lstat(remotePath); lerr == nil && info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
			_ = sc.Remove(remotePath)
			if err2 := sc.Rename(tmp, remotePath); err2 == nil {
				return nil
			}
		}
		_ = sc.Remove(tmp)
		return err
	}
	return nil
}
