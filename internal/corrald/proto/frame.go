package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxFrameSize is the maximum JSON payload size for a single wire frame (4 MiB).
const MaxFrameSize = 4 << 20

var (
	// ErrFrameTooLarge is returned when a frame length exceeds MaxFrameSize.
	ErrFrameTooLarge = errors.New("corrald frame exceeds 4 MiB limit")
	// ErrEmptyFrame is returned when a frame declares a zero-length payload.
	ErrEmptyFrame = errors.New("corrald frame length is zero")
	// ErrInvalidJSON is returned when a frame payload is not valid UTF-8 JSON.
	ErrInvalidJSON = errors.New("corrald frame payload is not valid UTF-8 JSON")
)

// WriteFrame writes a single length-prefixed frame (4-byte big-endian uint32
// length followed by payload) to w.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return ErrEmptyFrame
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if len(payload) <= 64<<10 {
		buf := make([]byte, 4+len(payload))
		copy(buf[:4], hdr[:])
		copy(buf[4:], payload)
		return writeAll(w, buf)
	}
	if err := writeAll(w, hdr[:]); err != nil {
		return err
	}
	return writeAll(w, payload)
}

// WriteJSON marshals v to JSON and writes it as a single wire frame.
func WriteJSON(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, payload)
}

// ReadFrame reads one length-prefixed frame from r and validates that its
// length is within (0, MaxFrameSize] and its bytes are valid UTF-8 JSON.
// If r hits EOF cleanly before any header byte is read, io.EOF is returned.
// Truncated headers or bodies return io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, ErrEmptyFrame
	}
	if n > MaxFrameSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if !utf8.Valid(buf) || !json.Valid(buf) {
		return nil, ErrInvalidJSON
	}
	return buf, nil
}

// ReadJSON reads one wire frame from r and unmarshals it into dst.
func ReadJSON(r io.Reader, dst any) error {
	payload, err := ReadFrame(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	return nil
}

func writeAll(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}
