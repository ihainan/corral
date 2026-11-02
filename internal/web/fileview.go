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
	if ctype == "image/gif" {
		// (A browser draws a frame bigger than the canvas the header says at
		// the frame's size: the largest the frames reach is what it decodes.)
		w, h, ok := gifReach(b)
		if !ok {
			return "", 0, 0
		}
		return ctype, max(cfg.Width, w), max(cfg.Height, h)
	}
	return ctype, cfg.Width, cfg.Height
}

// gifReach walks a GIF's blocks (not decoding them) for how far right and
// down its frames reach; not ok where it is not well formed.
func gifReach(b []byte) (w, h int, ok bool) {
	if len(b) < 13 {
		return 0, 0, false
	}
	i := 13
	if b[10]&0x80 != 0 { // (a global color table)
		i += 3 << (b[10]&7 + 1)
	}
	skipBlocks := func() bool { // (data sub-blocks, to the empty one)
		for i < len(b) {
			n := int(b[i])
			i++
			if n == 0 {
				return true
			}
			i += n
		}
		return false
	}
	for i < len(b) {
		switch b[i] {
		case 0x3b: // (its end)
			return w, h, true
		case 0x21: // (an extension)
			i += 2
			if !skipBlocks() {
				return 0, 0, false
			}
		case 0x2c: // (a frame)
			if i+10 > len(b) {
				return 0, 0, false
			}
			d := b[i+1 : i+10]
			left, top := int(binary.LittleEndian.Uint16(d[0:])), int(binary.LittleEndian.Uint16(d[2:]))
			fw, fh := int(binary.LittleEndian.Uint16(d[4:])), int(binary.LittleEndian.Uint16(d[6:]))
			w, h = max(w, left+fw), max(h, top+fh)
			i += 10
			if d[8]&0x80 != 0 { // (a local color table)
				i += 3 << (d[8]&7 + 1)
			}
			i++ // (the LZW code size)
			if !skipBlocks() {
				return 0, 0, false
			}
		default:
			return 0, 0, false
		}
	}
	return 0, 0, false // (no end: cut short)
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
