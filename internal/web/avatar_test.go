package web

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// raw: a request with these bytes as its body, as the page sends a picture.
func (c *client) raw(method, path, ctype string, body []byte, origin bool) (*http.Response, []byte) {
	c.h.t.Helper()
	req, _ := http.NewRequest(method, c.h.ts.URL+path, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if origin {
		req.Header.Set("Origin", c.h.cfg.Origin())
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: c.h.srv.cookieName(), Value: c.cookie})
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func avatarPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{200, 80, 40, 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// An account sets its picture (a PNG or JPEG, small), sees its time in /api/me,
// and anyone signed in fetches it — served as that picture and nothing to run;
// pages, SVG, large or huge ones and other sites' requests are refused.
func TestAvatar(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()
	id := strconv.Itoa(int(admin.do("GET", "/api/me", nil).json()["id"].(float64)))
	if _, ok := admin.do("GET", "/api/me", nil).json()["avatar_at"]; ok {
		t.Fatal("avatar_at said with no picture")
	}
	res, _ := admin.raw("GET", "/api/users/"+id+"/avatar", "", nil, false)
	if res.StatusCode != 404 {
		t.Fatalf("no picture: %d", res.StatusCode)
	}

	for name, body := range map[string][]byte{
		"html":     []byte("<html><script>alert(1)</script></html>"),
		"svg":      []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"png head": append(avatarPNG(t, 8, 8)[:40], []byte("<script>alert(1)</script>")...),
		"too big":  append(avatarPNG(t, 8, 8), make([]byte, 210<<10)...),
		"too wide": avatarPNG(t, 600, 10),
		"empty":    nil,
	} {
		if res, b := admin.raw("PUT", "/api/me/avatar", "image/png", body, true); res.StatusCode < 400 {
			t.Errorf("%s: taken (%d %s)", name, res.StatusCode, b)
		}
	}
	if res, _ := admin.raw("PUT", "/api/me/avatar", "image/png", avatarPNG(t, 64, 64), false); res.StatusCode != 403 {
		t.Fatalf("without the site's Origin: %d", res.StatusCode)
	}
	pic := avatarPNG(t, 256, 256)
	res, b := admin.raw("PUT", "/api/me/avatar", "application/octet-stream", pic, true)
	if res.StatusCode != 200 || !strings.Contains(string(b), `"avatar_at"`) {
		t.Fatalf("set: %d %s", res.StatusCode, b)
	}
	at := strconv.Itoa(int(admin.do("GET", "/api/me", nil).json()["avatar_at"].(float64)))

	// Another account sees it; one signed out does not.
	bob := h.member(admin, "bob")
	res, got := bob.raw("GET", "/api/users/"+id+"/avatar?v="+at, "", nil, false)
	if res.StatusCode != 200 || !bytes.Equal(got, pic) {
		t.Fatalf("fetch: %d, %d bytes", res.StatusCode, len(got))
	}
	for k, want := range map[string]string{"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "Cache-Control": "private, max-age=31536000, immutable"} {
		if v := res.Header.Get(k); v != want {
			t.Errorf("%s: %q, want %q", k, v, want)
		}
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("CSP: %q", res.Header.Get("Content-Security-Policy"))
	}
	if res, _ := bob.raw("GET", "/api/users/"+id+"/avatar?v=1", "", nil, false); res.Header.Get("Cache-Control") != "private, no-cache" {
		t.Errorf("an old address cached: %q", res.Header.Get("Cache-Control"))
	}
	if res, _ := (&client{h: h}).raw("GET", "/api/users/"+id+"/avatar", "", nil, false); res.StatusCode != 401 {
		t.Errorf("signed out: %d", res.StatusCode)
	}

	mustStatus(t, admin.do("DELETE", "/api/me/avatar", nil), 200, "remove")
	if _, ok := admin.do("GET", "/api/me", nil).json()["avatar_at"]; ok {
		t.Fatal("avatar_at still said")
	}
	if res, _ := bob.raw("GET", "/api/users/"+id+"/avatar", "", nil, false); res.StatusCode != 404 {
		t.Fatalf("removed: %d", res.StatusCode)
	}
}
