package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// DefaultMaxLogBytes is the 5 MiB per-file log size cap from spec section 3.
// At most one rotated backup (corrald.log.1) is kept, bounding total log size
// to 10 MiB.
const DefaultMaxLogBytes int64 = 5 << 20

// RotatingWriter is a thread-safe file writer that rotates path -> path+".1"
// once writing would exceed maxBytes.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

// OpenRotatingWriter opens or creates path with mode 0600 and configures
// single-backup rotation at maxBytes.
func OpenRotatingWriter(path string, maxBytes int64) (*RotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxLogBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &RotatingWriter{
		path:     path,
		maxBytes: maxBytes,
	}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// Write appends p, rotating corrald.log to corrald.log.1 when the write would
// exceed maxBytes.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	backup := w.path + ".1"
	_ = os.Remove(backup)
	if err := os.Rename(w.path, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate log: %w", err)
	}
	return w.openLocked()
}

// Close closes the underlying log file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// NewLogger creates a structured text logger backed by w.
func NewLogger(w io.Writer) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}
