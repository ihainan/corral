package web

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// staticHandler serves the single-page app: real files when they exist,
// index.html for every other path so client-side routes survive a reload.
// A file the build stored compressed beside it (name.br, name.gz: see
// web/vite.config.ts) goes so to a browser that takes it.
func (s *Server) staticHandler() http.Handler {
	if s.static == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("Corral API is running, but this binary was built without the web interface.\n"))
		})
	}
	files := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(s.static, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				if s.serveCompressed(w, r, p, "") {
					return
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		if s.serveCompressed(w, r, "index.html", "text/html; charset=utf-8") {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, err := fs.ReadFile(s.static, "index.html")
		if err != nil {
			http.Error(w, "index.html missing", http.StatusInternalServerError)
			return
		}
		w.Write(b)
	})
}

// (Not in every system's table: a manifest sent compressed keeps its type.)
func init() { _ = mime.AddExtensionType(".webmanifest", "application/manifest+json") }

// staticEncodings: the forms a file may be stored in beside it, best first.
var staticEncodings = []struct{ name, ext string }{{"br", ".br"}, {"gzip", ".gz"}}

// serveCompressed sends p as stored compressed, in the first form the
// request takes, and says so; false where there is none (the file goes as
// it is). ctype: its type, else by its name's extension.
func (s *Server) serveCompressed(w http.ResponseWriter, r *http.Request, p, ctype string) bool {
	stored := false
	for _, e := range staticEncodings {
		b, err := fs.ReadFile(s.static, p+e.ext)
		if err != nil {
			continue
		}
		stored = true
		if !acceptsEncoding(r.Header.Get("Accept-Encoding"), e.name) {
			continue
		}
		if ctype == "" {
			ctype = mime.TypeByExtension(path.Ext(p))
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		h := w.Header()
		h.Add("Vary", "Accept-Encoding")
		h.Set("Content-Type", ctype)
		h.Set("Content-Encoding", e.name)
		// (As the file server would answer it: conditions, ranges, HEAD —
		// of the bytes sent, which are the compressed ones.)
		http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(b))
		return true
	}
	if stored {
		w.Header().Add("Vary", "Accept-Encoding") // (the same address is sent compressed to others)
	}
	return false
}

// acceptsEncoding: the Accept-Encoding header takes enc (named, or by "*"),
// not refused with q=0.
func acceptsEncoding(header, enc string) bool {
	star := false
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		zero := false
		for _, prm := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(prm), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q <= 0 {
					zero = true
				}
			}
		}
		switch name {
		case enc:
			return !zero
		case "*":
			star = !zero
		}
	}
	return star
}
