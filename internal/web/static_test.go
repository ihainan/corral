package web

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// A file stored compressed beside it goes so to a browser that takes that
// (br before gzip), as it is to one that does not; its type is the file's;
// caches are told it varies.
func TestStaticCompressed(t *testing.T) {
	s := &Server{static: fstest.MapFS{
		"index.html":           {Data: []byte("<html>page</html>")},
		"index.html.gz":        {Data: []byte("GZ-INDEX")},
		"assets/app.js":        {Data: []byte("console.log(1)")},
		"assets/app.js.br":     {Data: []byte("BR-APP")},
		"assets/app.js.gz":     {Data: []byte("GZ-APP")},
		"assets/plain.css":     {Data: []byte("a{}")},
		"manifest.webmanifest": {Data: []byte("{}")},
	}}
	h := s.staticHandler()
	get := func(method, p, ae string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, p, nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	cases := []struct{ p, ae, body, enc string }{
		{"/assets/app.js", "gzip, deflate, br", "BR-APP", "br"},
		{"/assets/app.js", "gzip", "GZ-APP", "gzip"},
		{"/assets/app.js", "br;q=0, gzip", "GZ-APP", "gzip"},
		{"/assets/app.js", "", "console.log(1)", ""},
		{"/assets/app.js", "identity", "console.log(1)", ""},
		{"/assets/app.js", "*", "BR-APP", "br"},
		{"/agents", "gzip, br", "GZ-INDEX", "gzip"}, // (a route of the app: its page, stored as gzip only)
		{"/", "", "<html>page</html>", ""},
		{"/assets/plain.css", "gzip, br", "a{}", ""},
	}
	for _, c := range cases {
		w := get("GET", c.p, c.ae)
		if w.Code != 200 || w.Body.String() != c.body || w.Header().Get("Content-Encoding") != c.enc {
			t.Errorf("%s %q: %d %q encoding %q", c.p, c.ae, w.Code, w.Body.String(), w.Header().Get("Content-Encoding"))
		}
	}
	w := get("GET", "/assets/app.js", "br")
	if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" || w.Header().Get("Vary") != "Accept-Encoding" || w.Body.String() != "BR-APP" {
		t.Errorf("headers: %v", w.Header())
	}
	if w := get("GET", "/agents", "br"); w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("page headers: %v", w.Header())
	}
	if w := get("GET", "/assets/app.js", ""); w.Header().Get("Vary") != "Accept-Encoding" {
		t.Errorf("uncompressed, not said to vary: %v", w.Header())
	}
	if w := get("HEAD", "/assets/app.js", "gzip"); w.Body.Len() != 0 || w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("HEAD: %q %v", w.Body.String(), w.Header())
	}
	// Conditions, as the file server answers them.
	cond := func(k, v string) int {
		r := httptest.NewRequest("GET", "/assets/app.js", nil)
		r.Header.Set("Accept-Encoding", "br")
		r.Header.Set(k, v)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := cond("If-None-Match", "*"); c != 304 {
		t.Errorf("If-None-Match *: %d", c)
	}
	if c := cond("If-Match", `"nope"`); c != 412 {
		t.Errorf("If-Match: %d", c)
	}
	if w := get("GET", "/manifest.webmanifest", ""); w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Errorf("manifest type: %v", w.Header())
	}
	if !acceptsEncoding("GZIP;q=0.5", "gzip") || acceptsEncoding("gzip;q=0", "gzip") || acceptsEncoding("deflate", "gzip") || !acceptsEncoding("*;q=1, br;q=0", "gzip") || acceptsEncoding("*;q=1, br;q=0", "br") {
		t.Error("Accept-Encoding read wrong")
	}
}

// A long JSON answer goes gzipped to a browser that takes it, the same
// once unpacked; a short one, or to one that does not, as it is.
func TestWriteJSONCompressed(t *testing.T) {
	long := map[string]string{"text": strings.Repeat("对话内容 ", 2000)}
	send := func(v any, ae string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/x", nil)
		if ae != "" {
			r.Header.Set("Accept-Encoding", ae)
		}
		w := httptest.NewRecorder()
		writeJSONCompressed(w, r, 200, v)
		return w
	}
	w := send(long, "gzip, br")
	if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("not gzipped: %v", w.Header())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	var back map[string]string
	if json.Unmarshal(got, &back) != nil || back["text"] != long["text"] {
		t.Fatal("unpacked, not the same")
	}
	if w := send(long, ""); w.Header().Get("Content-Encoding") != "" || !strings.Contains(w.Body.String(), "对话内容") {
		t.Fatal("gzipped for one that does not take it")
	}
	if w := send(map[string]int{"n": 1}, "gzip"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "{\"n\":1}\n" {
		t.Fatal("a short one gzipped")
	}
}
