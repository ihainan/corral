package web

import (
	"bytes"
	"encoding/binary"
	"image"
	_ "image/gif" // (DecodeConfig: their headers)
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
)

// A picture of the session's host shown on the page (the agents chat: one a
// step read, one the person sent). Only what an <img> shows as a picture
// goes out, by its own first bytes and under its own type: PNG, JPEG, GIF,
// WebP. Not SVG (a document that runs), nor anything the browser could take
// for a page. Read whole first, so that what is sent is what was checked.
const (
	viewMaxBytes  = 20 << 20
	viewMaxPixels = 50_000_000 // (the decoded picture's size in the browser)
)

func (s *Server) apiFilesView(w http.ResponseWriter, r *http.Request) {
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
	if fi.Size() > viewMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the picture is too large to show")
		return
	}
	data, err := io.ReadAll(io.LimitReader(&progressReader{r: f, t: t}, viewMaxBytes+1))
	if err != nil {
		writeFileError(w, err)
		return
	}
	if len(data) > viewMaxBytes { // (it grew since)
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the picture is too large to show")
		return
	}
	ctype, wide, high := pictureOf(data)
	if ctype == "" {
		writeError(w, http.StatusUnsupportedMediaType, "not_an_image", "this file is not a picture that can be shown")
		return
	}
	if wide <= 0 || high <= 0 || int64(wide)*int64(high) > viewMaxPixels {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the picture is too large to show")
		return
	}
	s.audit(r, "file.view", sess.HostName, map[string]any{"session": sess.ID, "path": p, "bytes": len(data)})
	disp := mime.FormatMediaType("inline", map[string]string{"filename": path.Base(p)})
	if disp == "" {
		disp = "inline"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", disp)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Cache-Control", "no-store") // (a later look is asked again: signed out, it is not shown)
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// pictureOf is a picture's type by its first bytes, and its size in pixels
// as its header says it; "" for anything else.
func pictureOf(b []byte) (ctype string, wide, high int) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		ctype = "image/png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		ctype = "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		ctype = "image/gif"
	case len(b) >= 30 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		wide, high = webpSize(b)
		if wide <= 0 {
			return "", 0, 0
		}
		return "image/webp", wide, high
	default:
		return "", 0, 0
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || "image/"+format != ctype {
		return "", 0, 0
	}
	return ctype, cfg.Width, cfg.Height
}

// webpSize reads a WebP's size from its first chunk: lossy (VP8), lossless
// (VP8L) or extended (VP8X, its canvas).
func webpSize(b []byte) (int, int) {
	switch string(b[12:16]) {
	case "VP8 ":
		// (The frame's start code, then 14 bits of width and of height.)
		if b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return 0, 0
		}
		return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	case "VP8L":
		if b[20] != 0x2f {
			return 0, 0
		}
		v := binary.LittleEndian.Uint32(b[21:25])
		return int(v&0x3fff) + 1, int(v>>14&0x3fff) + 1
	case "VP8X":
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1
	}
	return 0, 0
}
