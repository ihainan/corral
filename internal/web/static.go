package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// staticHandler serves the single-page app: real files when they exist,
// index.html for every other path so client-side routes survive a reload.
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
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, err := fs.ReadFile(s.static, "index.html")
		if err != nil {
			http.Error(w, "index.html missing", http.StatusInternalServerError)
			return
		}
		w.Write(b)
	})
}
