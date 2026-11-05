package web

import (
	"bytes"
	"io"
	"net/http"
	"unicode/utf8"
)

// A text file of the session's host read for the page to show (the files
// panel's preview, a path an agent named): its words, as JSON — never the
// file as itself, so that nothing in it is taken for a page, whatever it
// is named (HTML, SVG are shown as their source). Read up to textMaxBytes:
// a longer file's beginning, cut at a whole character, and said cut. Only
// UTF-8 with no NUL near its start counts as text; anything else is
// refused, to be downloaded instead. Each read is audited.
const (
	textMaxBytes = 1 << 20
	textSniff    = 8 << 10 // (how far a NUL says "binary")
)

func (s *Server) apiFilesText(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	p, ok := cleanRemotePath(r.URL.Query().Get("path"))
	if !ok || p == "/" {
		writeError(w, http.StatusBadRequest, "bad_path", "the path must be absolute")
		return
	}
	t, ok := s.beginTransfer(w, r, sess)
	if !ok {
		return
	}
	defer t.end()
	client, err := s.openTransferSFTP(t, sess)
	if err != nil {
		writeFileError(w, err)
		return
	}
	// (Not a plain file — a FIFO, a device, or a link to one — refused before
	// it is opened: opening one may block till the transfer's watchdog. The
	// check after opening stays: the path may be changed in between.)
	if fi, err := client.Stat(p); err != nil {
		writeFileError(w, err)
		return
	} else if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not_a_file", "only a file can be shown")
		return
	}
	f, err := client.Open(p)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer f.Close()
	// (Of what was opened, not of the path again: the file read is the one checked.)
	fi, err := f.Stat()
	if err != nil {
		writeFileError(w, err)
		return
	}
	if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not_a_file", "only a file can be shown")
		return
	}
	// tail: its end instead (a command's output, a log: what is newest),
	// from a whole line; said cut at its start.
	tail := r.URL.Query().Get("tail") == "1"
	var data []byte
	cut := false
	if tail && fi.Size() > textMaxBytes {
		if _, err := f.Seek(fi.Size()-textMaxBytes, io.SeekStart); err != nil {
			writeFileError(w, err)
			return
		}
		data, err = io.ReadAll(io.LimitReader(&progressReader{r: f, t: t}, textMaxBytes))
		if err != nil {
			writeFileError(w, err)
			return
		}
		cut = true
		data = fromWholeLine(data)
	} else {
		data, err = io.ReadAll(io.LimitReader(&progressReader{r: f, t: t}, textMaxBytes+1))
		if err != nil {
			writeFileError(w, err)
			return
		}
		cut = len(data) > textMaxBytes
		if cut {
			data = wholeChars(data[:textMaxBytes])
		}
	}
	if bytes.IndexByte(data[:min(len(data), textSniff)], 0) >= 0 || !utf8.Valid(data) {
		writeError(w, http.StatusUnsupportedMediaType, "not_text", "this file is not text that can be shown")
		return
	}
	s.audit(r, "file.text", sess.HostName, map[string]any{"session": sess.ID, "path": p, "bytes": len(data), "cut": cut, "tail": tail})
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "size": fi.Size(), "text": string(data), "cut": cut, "tail": tail})
}

// fromWholeLine: the end of a file read from somewhere in its middle, from
// the start of its first whole line (else, a line longer than all of it,
// from its first whole character).
func fromWholeLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
		return b[i+1:]
	}
	for i := 0; i < len(b) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(b[i]) {
			return b[i:]
		}
	}
	return b
}

// wholeChars: b without a character cut short at its end (of UTF-8: what
// is after the last character's first byte, where that character is not
// all there). Anything else in it is left for the check of the whole.
func wholeChars(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}
