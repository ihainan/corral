package sftplite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

// serve runs a real SFTP server (github.com/pkg/sftp) on in-memory pipes,
// working in dir, and returns a client of it.
func serve(t *testing.T, dir string) *Client {
	t.Helper()
	cr, sw := io.Pipe() // server → client
	sr, cw := io.Pipe() // client → server
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, sftp.WithServerWorkingDirectory(dir))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	c, err := New(cr, cw, func() {
		cw.Close()
		cr.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	return c
}

func TestListStatAndChanges(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 250; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", i)), []byte("x"), 0o640)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "link"))
	c := serve(t, dir)

	home, err := c.Getwd()
	if err != nil || filepath.Clean(home) != filepath.Clean(dir) {
		t.Fatalf("getwd %q %v", home, err)
	}
	all, trunc, err := c.ReadDirLimit(dir, 1000)
	if err != nil || trunc || len(all) != 252 {
		t.Fatalf("full list: %d entries, truncated %v, %v", len(all), trunc, err)
	}
	some, trunc, err := c.ReadDirLimit(dir, 100)
	if err != nil || !trunc || len(some) != 100 {
		t.Fatalf("limited list: %d entries, truncated %v, %v", len(some), trunc, err)
	}
	exact, trunc, err := c.ReadDirLimit(dir, 252)
	if err != nil || trunc || len(exact) != 252 {
		t.Fatalf("list of exactly the limit: %d, truncated %v, %v", len(exact), trunc, err)
	}
	var link, file os.FileInfo
	for _, fi := range all {
		switch fi.Name() {
		case "link":
			link = fi
		case "f007":
			file = fi
		}
	}
	if link == nil || link.Mode()&fs.ModeSymlink == 0 || file.Mode() != 0o640 || file.Size() != 1 || file.ModTime().IsZero() {
		t.Fatalf("entries: link %v, file %v %d", link, file.Mode(), file.Size())
	}
	if fi, err := c.Stat(filepath.Join(dir, "link")); err != nil || !fi.IsDir() {
		t.Fatalf("stat follows the link: %v %v", fi, err)
	}
	if fi, err := c.Lstat(filepath.Join(dir, "link")); err != nil || fi.Mode()&fs.ModeSymlink == 0 || fi.Name() != "link" {
		t.Fatalf("lstat does not: %v %v", fi, err)
	}
	if _, err := c.Lstat(filepath.Join(dir, "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := c.ReadDirLimit(filepath.Join(dir, "nope"), 10); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("list missing: %v", err)
	}

	if err := c.Mkdir(filepath.Join(dir, "new")); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename(filepath.Join(dir, "f000"), filepath.Join(dir, "g000")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "g000")); err != nil {
		t.Fatal("rename did not happen")
	}
	if err := c.Remove(filepath.Join(dir, "g000")); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory(filepath.Join(dir, "new")); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(filepath.Join(dir, "g000")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("remove missing: %v", err)
	}
}

func TestConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), make([]byte, i), 0o600)
	}
	c := serve(t, dir)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for k := 0; k < 10; k++ {
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				fi, err := c.Stat(filepath.Join(dir, fmt.Sprintf("f%d", i)))
				if err == nil && fi.Size() != int64(i) {
					err = fmt.Errorf("f%d: size %d", i, fi.Size())
				}
				if err != nil {
					errs <- err
				}
			}(i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// A host that stops answering: Close ends the waiting request at once.
func TestCloseEndsWaitingRequests(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() { // answer the version, then read requests without answering
		var hdr [9]byte
		io.ReadFull(sr, hdr[:])
		v := []byte{0, 0, 0, 5, fxpVersion, 0, 0, 0, 3}
		sw.Write(v)
		io.Copy(io.Discard, sr)
	}()
	c, err := New(cr, cw, func() { cw.Close(); cr.Close() })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Stat("/x"); done <- err }()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("waiting request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request still waiting after Close")
	}
	if _, err := c.Stat("/y"); !errors.Is(err, ErrLost) {
		t.Fatalf("request after Close: %v", err)
	}
}

// An answer larger than the bound ends the session instead of being read.
func TestHugePacketRefused(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() {
		var hdr [9]byte
		io.ReadFull(sr, hdr[:])
		sw.Write([]byte{0, 0, 0, 5, fxpVersion, 0, 0, 0, 3})
		buf := make([]byte, 4096)
		sr.Read(buf) // the request
		var big [5]byte
		binary.BigEndian.PutUint32(big[:], maxPacket+10)
		big[4] = fxpName
		sw.Write(big[:])
		io.Copy(io.Discard, sr)
	}()
	c, err := New(cr, cw, func() { cw.Close(); cr.Close() })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.RealPath("."); !errors.Is(err, ErrLost) {
		t.Fatalf("huge answer: %v", err)
	}
}

// scripted is a host that answers the version with version, then each
// request with the next of answers (type, then the body after the id).
func scripted(t *testing.T, version uint32, answers ...[]byte) (*Client, error) {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() {
		var hdr [9]byte
		io.ReadFull(sr, hdr[:])
		v := []byte{0, 0, 0, 5, fxpVersion}
		sw.Write(binary.BigEndian.AppendUint32(v, version))
		for _, a := range answers {
			var h [4]byte
			if _, err := io.ReadFull(sr, h[:]); err != nil {
				return
			}
			req := make([]byte, binary.BigEndian.Uint32(h[:]))
			if _, err := io.ReadFull(sr, req); err != nil {
				return
			}
			p := binary.BigEndian.AppendUint32(nil, uint32(len(a)+4))
			p = append(p, a[0])
			p = append(p, req[1:5]...) // the request id
			sw.Write(append(p, a[1:]...))
		}
		io.Copy(io.Discard, sr)
	}()
	c, err := New(cr, cw, func() { cw.Close(); cr.Close() })
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func u32(v uint32) []byte  { return binary.BigEndian.AppendUint32(nil, v) }
func sstr(s string) []byte { return append(u32(uint32(len(s))), s...) }
func cat(bs ...[]byte) []byte {
	var out []byte
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

func TestOtherVersionsRefused(t *testing.T) {
	for _, v := range []uint32{2, 4, 6} {
		if _, err := scripted(t, v); err == nil {
			t.Errorf("version %d accepted", v)
		}
	}
}

func TestMalformedAnswersFail(t *testing.T) {
	okStatus := cat([]byte{fxpStatus}, u32(StatusOK))
	cases := []struct {
		name   string
		answer []byte
		run    func(*Client) error
	}{
		{"status OK without a message", okStatus, func(c *Client) error { return c.Mkdir("/x") }},
		{"status OK with a cut message", cat(okStatus, u32(50), []byte("ab")), func(c *Client) error { return c.Mkdir("/x") }},
		{"unknown attribute flags", cat([]byte{fxpAttrs}, u32(0x10)), func(c *Client) error { _, err := c.Stat("/x"); return err }},
		{"realpath with two names", cat([]byte{fxpName}, u32(2), sstr("/a"), sstr(""), u32(0), sstr("/b"), sstr(""), u32(0)),
			func(c *Client) error { _, err := c.RealPath("."); return err }},
		{"status OK with a cut language", cat(okStatus, sstr(""), u32(5), []byte("ab")), func(c *Client) error { return c.Mkdir("/x") }},
		{"realpath without attributes", cat([]byte{fxpName}, u32(1), sstr("/a"), sstr("/a")), func(c *Client) error { _, err := c.RealPath("."); return err }},
	}
	for i, tc := range cases {
		c, err := scripted(t, 3, tc.answer)
		if err != nil {
			t.Fatal(err)
		}
		err = tc.run(c)
		if (i == 0) != (err == nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestListingRefusesBadEntries(t *testing.T) {
	handle := cat([]byte{fxpHandle}, sstr("h"))
	closed := cat([]byte{fxpStatus}, u32(StatusOK))
	entry := func(name string) []byte { return cat(sstr(name), sstr(""), u32(0)) }
	// A good entry, then one cut short: the listing fails even when the
	// first entry alone reaches the limit.
	cut := cat([]byte{fxpName}, u32(2), entry("a"), sstr("b"))
	c, _ := scripted(t, 3, handle, cut, closed)
	if _, _, err := c.ReadDirLimit("/d", 1); err == nil {
		t.Error("cut entry accepted")
	}
	long := cat([]byte{fxpName}, u32(1), entry(string(make([]byte, maxName+1))))
	c, _ = scripted(t, 3, handle, long, closed)
	if _, _, err := c.ReadDirLimit("/d", 10); err == nil {
		t.Error("over-long name accepted")
	}
	// Closing the folder reports a lost connection: so does the listing.
	eof := cat([]byte{fxpStatus}, u32(StatusEOF))
	lost := cat([]byte{fxpStatus}, u32(StatusConnectionLost))
	c, _ = scripted(t, 3, handle, eof, lost)
	if _, _, err := c.ReadDirLimit("/d", 10); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("lost on close: %v", err)
	}
	// ...also when the listing failed already.
	denied := cat([]byte{fxpStatus}, u32(StatusPermissionDenied))
	c, _ = scripted(t, 3, handle, denied, lost)
	if _, _, err := c.ReadDirLimit("/d", 10); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("lost on close after a failure: %v", err)
	}
}

// A host that stops reading while sending a bad answer: the request being
// written ends instead of waiting for Close.
func TestBadAnswerEndsBlockedWriter(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() {
		var hdr [9]byte
		io.ReadFull(sr, hdr[:])
		sw.Write([]byte{0, 0, 0, 5, fxpVersion, 0, 0, 0, 3})
		var h [4]byte
		io.ReadFull(sr, h[:]) // the start of the first request, then nothing more
		var big [5]byte
		binary.BigEndian.PutUint32(big[:], maxPacket+10)
		sw.Write(big[:])
	}()
	c, err := New(cr, cw, func() { cw.Close(); cr.Close() })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Stat("/x"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrLost) {
			t.Fatalf("blocked request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request still blocked")
	}
}
