// Package sftplite is a small SFTP (version 3) client for the file panel's
// quick operations: listing a folder (and stopping after a given number of
// entries, so a huge folder is never read whole), stat, mkdir, rename and
// remove; and reading part of a file (an agent's conversation log). Requests are multiplexed on one channel; closing the client ends
// every request still waiting at once. Transfers use github.com/pkg/sftp.
package sftplite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sync"
	"time"
)

// Packet types (draft-ietf-secsh-filexfer-02).
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpOpen     = 3
	fxpClose    = 4
	fxpRead     = 5
	fxpLstat    = 7
	fxpOpendir  = 11
	fxpReaddir  = 12
	fxpRemove   = 13
	fxpMkdir    = 14
	fxpRmdir    = 15
	fxpRealpath = 16
	fxpStat     = 17
	fxpRename   = 18
	fxpStatus   = 101
	fxpHandle   = 102
	fxpData     = 103
	fxpName     = 104
	fxpAttrs    = 105
)

// Status codes.
const (
	StatusOK               = 0
	StatusEOF              = 1
	StatusNoSuchFile       = 2
	StatusPermissionDenied = 3
	StatusFailure          = 4
	StatusBadMessage       = 5
	StatusNoConnection     = 6
	StatusConnectionLost   = 7
	StatusOpUnsupported    = 8
)

// maxPacket bounds one answer: a host cannot make the client hold more.
const maxPacket = 1 << 20

// maxName bounds a listed name (Linux allows 255 bytes), so a listing holds
// at most its number of entries times this.
const maxName = 4096

var (
	// ErrLost: the channel ended (or was closed); the client is of no
	// further use. It matches io.ErrUnexpectedEOF.
	ErrLost      = fmt.Errorf("sftp channel lost: %w", io.ErrUnexpectedEOF)
	errBadPacket = errors.New("malformed sftp packet")
)

// StatusError is the host's answer to a request that failed.
type StatusError struct {
	Code uint32
	Msg  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("sftp status %d: %s", e.Code, e.Msg) }

// Is lets errors.Is see the usual meanings: fs.ErrNotExist, fs.ErrPermission,
// and io.ErrUnexpectedEOF for a host that lost its connection.
func (e *StatusError) Is(target error) bool {
	switch e.Code {
	case StatusNoSuchFile:
		return target == fs.ErrNotExist
	case StatusPermissionDenied:
		return target == fs.ErrPermission
	case StatusNoConnection, StatusConnectionLost:
		return target == io.ErrUnexpectedEOF
	}
	return false
}

type reply struct {
	typ  byte
	data []byte // after the request id
}

type Client struct {
	w         io.Writer
	wmu       sync.Mutex
	closer    func()
	closeOnce sync.Once

	mu      sync.Mutex
	next    uint32
	waiting map[uint32]chan reply
	err     error // set once the channel is gone
}

// New starts a session over r (the host's answers) and w (requests);
// closer, called by Close, must make both fail. It returns after the
// version exchange: a caller that must not wait forever for a silent host
// closes r and w from elsewhere.
func New(r io.Reader, w io.Writer, closer func()) (*Client, error) {
	c := &Client{w: w, closer: closer, waiting: map[uint32]chan reply{}}
	var init [9]byte
	binary.BigEndian.PutUint32(init[0:], 5)
	init[4] = fxpInit
	binary.BigEndian.PutUint32(init[5:], 3)
	if _, err := w.Write(init[:]); err != nil {
		return nil, err
	}
	typ, data, err := readPacket(r)
	if err != nil {
		return nil, err
	}
	if typ != fxpVersion || len(data) < 4 || binary.BigEndian.Uint32(data) != 3 {
		return nil, errors.New("the host does not speak sftp version 3")
	}
	go c.read(r)
	return c, nil
}

func readPacket(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n < 1 || n > maxPacket {
		return 0, nil, fmt.Errorf("sftp packet of %d bytes refused", n)
	}
	data := make([]byte, n-1)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return hdr[4], data, nil
}

func (c *Client) read(r io.Reader) {
	for {
		typ, data, err := readPacket(r)
		if err == nil && len(data) < 4 {
			err = errBadPacket
		}
		if err != nil {
			c.Close() // (also ends requests still being written)
			return
		}
		id := binary.BigEndian.Uint32(data)
		c.mu.Lock()
		ch := c.waiting[id]
		delete(c.waiting, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- reply{typ: typ, data: data[4:]}
		}
	}
}

// fail ends the client: every waiting request returns ErrLost.
func (c *Client) fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = ErrLost
	for id, ch := range c.waiting {
		close(ch)
		delete(c.waiting, id)
	}
}

// Close ends the session; requests still waiting return ErrLost.
func (c *Client) Close() error {
	c.fail()
	if c.closer != nil {
		c.closeOnce.Do(c.closer)
	}
	return nil
}

// packet builds a request.
type packet []byte

func newPacket(typ byte, id uint32) packet {
	p := packet{0, 0, 0, 0, typ}
	return p.u32(id)
}
func (p packet) u32(v uint32) packet { return binary.BigEndian.AppendUint32(p, v) }
func (p packet) str(s string) packet { return append(p.u32(uint32(len(s))), s...) }
func (p packet) done() []byte {
	binary.BigEndian.PutUint32(p, uint32(len(p)-4))
	return p
}

// call sends a request built by build (given its id) and waits for the answer.
func (c *Client) call(typ byte, build func(packet) packet) (reply, error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return reply{}, c.err
	}
	id := c.next
	c.next++
	ch := make(chan reply, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	p := newPacket(typ, id)
	if build != nil {
		p = build(p)
	}
	c.wmu.Lock()
	_, err := c.w.Write(p.done())
	c.wmu.Unlock()
	if err != nil {
		c.Close()
		return reply{}, ErrLost
	}
	r, ok := <-ch
	if !ok {
		return reply{}, ErrLost
	}
	return r, nil
}

// reader parses an answer.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) u32() uint32 {
	if len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) u64() uint64 {
	if len(r.b) < 8 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *reader) str() string {
	n := r.u32()
	if r.bad || uint32(len(r.b)) < n {
		r.bad = true
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

// status turns a STATUS answer into nil (OK) or a *StatusError.
func status(rp reply) error {
	if rp.typ != fxpStatus {
		return errBadPacket
	}
	r := &reader{b: rp.data}
	code := r.u32()
	var msg string
	if len(r.b) > 0 { // (some servers leave out the message and its language)
		msg = r.str()
		if len(r.b) > 0 {
			r.str()
		}
	}
	if r.bad {
		return errBadPacket
	}
	if code == StatusOK {
		return nil
	}
	return &StatusError{Code: code, Msg: msg}
}

// expect returns the answer if it has type typ, the status error otherwise.
func expect(rp reply, err error, typ byte) (*reader, error) {
	if err != nil {
		return nil, err
	}
	if rp.typ == typ {
		return &reader{b: rp.data}, nil
	}
	if rp.typ == fxpStatus {
		if e := status(rp); e != nil {
			return nil, e
		}
	}
	return nil, errBadPacket
}

// Attributes ---------------------------------------------------------------

const (
	attrSize        = 0x1
	attrUIDGID      = 0x2
	attrPermissions = 0x4
	attrACModTime   = 0x8
	attrExtended    = 0x80000000
	attrKnown       = attrSize | attrUIDGID | attrPermissions | attrACModTime | attrExtended
)

type fileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
}

func (f *fileInfo) Name() string       { return f.name }
func (f *fileInfo) Size() int64        { return f.size }
func (f *fileInfo) Mode() fs.FileMode  { return f.mode }
func (f *fileInfo) ModTime() time.Time { return f.mtime }
func (f *fileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f *fileInfo) Sys() any           { return nil }

// fileMode converts POSIX mode bits as SFTP sends them.
func fileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o010000:
		mode |= fs.ModeNamedPipe
	case 0o140000:
		mode |= fs.ModeSocket
	case 0o020000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		mode |= fs.ModeDevice
	}
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

func (r *reader) attrs(name string) *fileInfo {
	fi := &fileInfo{name: name}
	flags := r.u32()
	if flags&^attrKnown != 0 { // fields of unknown length would follow
		r.bad = true
		return fi
	}
	if flags&attrSize != 0 {
		fi.size = int64(r.u64())
	}
	if flags&attrUIDGID != 0 {
		r.u32()
		r.u32()
	}
	if flags&attrPermissions != 0 {
		fi.mode = fileMode(r.u32())
	}
	if flags&attrACModTime != 0 {
		r.u32()
		fi.mtime = time.Unix(int64(r.u32()), 0)
	}
	if flags&attrExtended != 0 {
		n := r.u32()
		for i := uint32(0); i < n && !r.bad; i++ {
			r.str()
			r.str()
		}
	}
	return fi
}

// Requests -------------------------------------------------------------------

func pathReq(p string) func(packet) packet { return func(pk packet) packet { return pk.str(p) } }

// RealPath resolves a path on the host.
func (c *Client) RealPath(p string) (string, error) {
	rp, err := c.call(fxpRealpath, pathReq(p))
	r, err := expect(rp, err, fxpName)
	if err != nil {
		return "", err
	}
	if n := r.u32(); n != 1 || r.bad {
		return "", errBadPacket
	}
	name := r.str()
	r.str() // long name
	r.attrs("")
	if r.bad {
		return "", errBadPacket
	}
	return name, nil
}

// Getwd is the directory the session starts in (the home directory).
func (c *Client) Getwd() (string, error) { return c.RealPath(".") }

func (c *Client) stat(typ byte, p string) (os.FileInfo, error) {
	rp, err := c.call(typ, pathReq(p))
	r, err := expect(rp, err, fxpAttrs)
	if err != nil {
		return nil, err
	}
	fi := r.attrs(path.Base(p))
	if r.bad {
		return nil, errBadPacket
	}
	return fi, nil
}

// Stat follows links; Lstat does not.
func (c *Client) Stat(p string) (os.FileInfo, error)  { return c.stat(fxpStat, p) }
func (c *Client) Lstat(p string) (os.FileInfo, error) { return c.stat(fxpLstat, p) }

func (c *Client) simple(typ byte, build func(packet) packet) error {
	rp, err := c.call(typ, build)
	if err != nil {
		return err
	}
	return status(rp)
}

func (c *Client) Mkdir(p string) error {
	return c.simple(fxpMkdir, func(pk packet) packet { return pk.str(p).u32(0) })
}
func (c *Client) Remove(p string) error          { return c.simple(fxpRemove, pathReq(p)) }
func (c *Client) RemoveDirectory(p string) error { return c.simple(fxpRmdir, pathReq(p)) }
func (c *Client) Rename(from, to string) error {
	return c.simple(fxpRename, func(pk packet) packet { return pk.str(from).str(to) })
}

// readChunk is what one read asks for; hosts commonly answer less.
const readChunk = 128 << 10

// ReadRange reads up to n bytes of a file from offset off, fewer when the
// file ends first. The file is opened for reading only, and closed again.
func (c *Client) ReadRange(p string, off int64, n int) (data []byte, err error) {
	if off < 0 || n < 0 {
		return nil, errors.New("sftplite: negative offset or length")
	}
	rp, err := c.call(fxpOpen, func(pk packet) packet { return pk.str(p).u32(1).u32(0) }) // READ, no attributes
	r, err := expect(rp, err, fxpHandle)
	if err != nil {
		return nil, err
	}
	handle := r.str()
	if r.bad {
		return nil, errBadPacket
	}
	defer func() {
		if cerr := c.simple(fxpClose, pathReq(handle)); errors.Is(cerr, io.ErrUnexpectedEOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			data, err = nil, cerr
		}
	}()
	for len(data) < n {
		want := min(n-len(data), readChunk)
		at := off + int64(len(data))
		rp, err := c.call(fxpRead, func(pk packet) packet {
			return binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint64(pk.str(handle), uint64(at)), uint32(want))
		})
		if err != nil {
			return nil, err
		}
		if rp.typ == fxpStatus {
			serr := status(rp)
			var st *StatusError
			if errors.As(serr, &st) && st.Code == StatusEOF {
				return data, nil
			}
			if serr == nil {
				serr = errBadPacket
			}
			return nil, serr
		}
		if rp.typ != fxpData {
			return nil, errBadPacket
		}
		r := &reader{b: rp.data}
		chunk := r.str()
		if r.bad || len(chunk) > want {
			return nil, errBadPacket
		}
		if len(chunk) == 0 {
			return data, nil // (a host that answers nothing would loop forever)
		}
		data = append(data, chunk...)
	}
	return data, nil
}

// ReadDirLimit lists a folder, without "." and "..", in the host's order. It
// stops once it has more than max entries: then it returns the first max and
// truncated. The folder's handle is closed either way.
func (c *Client) ReadDirLimit(p string, max int) (entries []os.FileInfo, truncated bool, err error) {
	rp, err := c.call(fxpOpendir, pathReq(p))
	r, err := expect(rp, err, fxpHandle)
	if err != nil {
		return nil, false, err
	}
	handle := r.str()
	if r.bad {
		return nil, false, errBadPacket
	}
	defer func() {
		// A lost connection is what the caller must hear about (other
		// failures to close only leave a handle on the host).
		if cerr := c.simple(fxpClose, pathReq(handle)); errors.Is(cerr, io.ErrUnexpectedEOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			entries, truncated, err = nil, false, cerr
		}
	}()
	for {
		rp, err := c.call(fxpReaddir, pathReq(handle))
		if err != nil {
			return nil, false, err
		}
		if rp.typ == fxpStatus {
			serr := status(rp)
			var st *StatusError
			if errors.As(serr, &st) && st.Code == StatusEOF {
				return entries, false, nil
			}
			if serr == nil {
				serr = errBadPacket
			}
			return nil, false, serr
		}
		if rp.typ != fxpName {
			return nil, false, errBadPacket
		}
		r := &reader{b: rp.data}
		n := r.u32()
		for i := uint32(0); i < n && !r.bad; i++ {
			name := r.str()
			r.str() // long name
			fi := r.attrs(name)
			if r.bad || len(name) > maxName {
				return nil, false, errBadPacket
			}
			if name == "." || name == ".." {
				continue
			}
			if len(entries) == max {
				return entries, true, nil
			}
			entries = append(entries, fi)
		}
		if r.bad {
			return nil, false, errBadPacket
		}
	}
}
