package web

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pngOf(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

// The view sends a picture of the host as itself (its type by its bytes,
// not its name), never cached, sandboxed, inline; anything else is refused
// before a byte of it goes out: SVG, HTML named .png, a folder, too many
// bytes, too many pixels. Each look is audited.
func TestFilesView(t *testing.T) {
	e := newFileEnv(t)
	var jb, gb bytes.Buffer
	jpeg.Encode(&jb, image.NewGray(image.Rect(0, 0, 3, 2)), nil)
	gif.Encode(&gb, image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White}), nil)
	// A lossless WebP of 3×2 (its header only matters here).
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8L\x00\x00\x00\x00\x2f")
	webp = binary.LittleEndian.AppendUint32(webp, uint32(3-1)|uint32(2-1)<<14)
	webp = append(webp, make([]byte, 8)...)
	files := map[string][]byte{"a.png": pngOf(3, 2), "b.jpg": jb.Bytes(), "c.gif": gb.Bytes(), "d.webp": webp}
	want := map[string]string{"a.png": "image/png", "b.jpg": "image/jpeg", "c.gif": "image/gif", "d.webp": "image/webp"}
	for name, data := range files {
		os.WriteFile(filepath.Join(e.root, name), data, 0o644)
		res, body := e.raw("GET", e.url("view", url.Values{"path": {e.root + "/" + name}}), nil, "")
		h := res.Header
		if res.StatusCode != 200 || !bytes.Equal(body, data) || h.Get("Content-Type") != want[name] {
			t.Fatalf("%s: %d %s %d bytes", name, res.StatusCode, h.Get("Content-Type"), len(body))
		}
		if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox; default-src 'none'" || h.Get("Content-Disposition") != `inline; filename=`+name {
			t.Fatalf("%s headers: %v", name, h)
		}
	}
	refused := map[string]struct {
		data   []byte
		status int
		code   string
	}{
		"x.svg":     {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 415, "not_an_image"},
		"page.png":  {[]byte("<html><script>alert(1)</script></html>"), 415, "not_an_image"},
		"fake.png":  {append([]byte("\x89PNG\r\n\x1a\n"), "<html>"...), 415, "not_an_image"}, // (a PNG's start, not a PNG)
		"huge.png":  {pngOf(10000, 6000), 413, "too_large"},                                  // (60 million pixels)
		"heavy.png": {append(pngOf(2, 2), make([]byte, viewMaxBytes)...), 413, "too_large"},
	}
	for name, c := range refused {
		os.WriteFile(filepath.Join(e.root, name), c.data, 0o644)
		res, body := e.raw("GET", e.url("view", url.Values{"path": {e.root + "/" + name}}), nil, "")
		if res.StatusCode != c.status || !bytes.Contains(body, []byte(`"`+c.code+`"`)) || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %s %.80q", name, res.StatusCode, res.Header.Get("Content-Type"), body)
		}
	}
	// A GIF whose canvas is 1×1 and one frame 8000×6500: refused by the frame (review548-1).
	if res, body := view(e, "big-frame.gif", bigFrameGIF()); res.StatusCode != 413 {
		t.Errorf("a frame past the canvas: %d %.80q", res.StatusCode, body)
	}
	os.Mkdir(filepath.Join(e.root, "dir.png"), 0o755)
	if res, _ := e.raw("GET", e.url("view", url.Values{"path": {e.root + "/dir.png"}}), nil, ""); res.StatusCode != 400 {
		t.Errorf("a folder: %d", res.StatusCode)
	}
	if res, _ := e.raw("GET", e.url("view", url.Values{"path": {e.root + "/none.png"}}), nil, ""); res.StatusCode != 404 {
		t.Errorf("not there: %d", res.StatusCode)
	}
	if res, _ := e.raw("GET", e.url("view", url.Values{"path": {"a.png"}}), nil, ""); res.StatusCode != 400 {
		t.Errorf("relative: %d", res.StatusCode)
	}
	// (Signed out: nothing.)
	n := 0
	for _, a := range auditActions(t, e.h) {
		if a == "file.view" {
			n++
		}
	}
	if n != 4 {
		t.Errorf("file.view audited %d times, want 4", n)
	}
	e.c.do("POST", "/api/logout", nil)
	if res, _ := e.raw("GET", e.url("view", url.Values{"path": {e.root + "/a.png"}}), nil, ""); res.StatusCode != 401 {
		t.Errorf("signed out: %d", res.StatusCode)
	}
}

func view(e *fileEnv, name string, data []byte) (*http.Response, []byte) {
	os.WriteFile(filepath.Join(e.root, name), data, 0o644)
	return e.raw("GET", e.url("view", url.Values{"path": {e.root + "/" + name}}), nil, "")
}

// bigFrameGIF is a well formed GIF of a 1×1 canvas holding one frame of
// 8000×6500 (its pixels a single LZW clear and end: what the frame says of
// its size is what counts).
func bigFrameGIF() []byte {
	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = append(b, 0x80, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff) // (a global table of two colors)
	b = append(b, 0x2c, 0, 0, 0, 0)
	b = binary.LittleEndian.AppendUint16(b, 8000)
	b = binary.LittleEndian.AppendUint16(b, 6500)
	b = append(b, 0, 2, 2, 0x4c, 0x01, 0, 0x3b)
	return b
}

func TestGifReach(t *testing.T) {
	var gb bytes.Buffer
	gif.Encode(&gb, image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White}), nil)
	if w, h, ok := gifReach(gb.Bytes()); !ok || w != 3 || h != 2 {
		t.Fatalf("a plain GIF: %d×%d %v", w, h, ok)
	}
	if w, h, ok := gifReach(bigFrameGIF()); !ok || w != 8000 || h != 6500 {
		t.Fatalf("the big frame: %d×%d %v", w, h, ok)
	}
	if _, _, ok := gifReach(bigFrameGIF()[:30]); ok {
		t.Fatal("cut short: taken")
	}
}
