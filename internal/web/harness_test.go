package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ihainan/corral/internal/auth"
	"github.com/ihainan/corral/internal/config"
	"github.com/ihainan/corral/internal/netguard"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
	"github.com/ihainan/corral/internal/testutil"
	"github.com/ihainan/corral/internal/vault"
)

type harness struct {
	t     *testing.T
	ts    *httptest.Server
	srv   *Server
	cfg   *config.Config
	store *store.Store
	auth  *auth.Service
	terms *term.Manager
}

type harnessOpt func(*config.Config)

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t}
	var handler http.Handler
	h.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(h.ts.Close)
	u, _ := url.Parse(h.ts.URL)
	h.cfg = &config.Config{
		PublicURL:          u,
		MasterKey:          bytes.Repeat([]byte{7}, 32),
		AllowedNetworks:    []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		DeniedNetworks:     []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("0.0.0.0/8")},
		DetachTimeout:      time.Hour,
		LoginIdleTimeout:   time.Hour,
		LoginAbsoluteLimit: 24 * time.Hour,
		MaxSessionsPerUser: 5,
		UploadMaxBytes:     1 << 20,
	}
	for _, o := range opts {
		o(h.cfg)
	}
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v, _ := vault.New(h.cfg.MasterKey)
	h.store = st
	h.auth = auth.NewService(st, h.cfg.LoginIdleTimeout, h.cfg.LoginAbsoluteLimit)
	h.terms = term.NewManager(h.cfg.DetachTimeout, h.cfg.MaxSessionsPerUser)
	t.Cleanup(h.terms.Shutdown)
	h.srv = New(Deps{Config: h.cfg, Store: st, Auth: h.auth, Vault: v,
		Guard: netguard.New(h.cfg.AllowedNetworks, h.cfg.DeniedNetworks), Terms: h.terms,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	handler = h.srv.Handler()
	return h
}

type client struct {
	h      *harness
	cookie string
	extra  map[string]string
}

type resp struct {
	Status int
	Body   []byte
}

func (r resp) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

func (r resp) code() string {
	s, _ := r.json()["error"].(string)
	return s
}

func (c *client) do(method, path string, body any) resp {
	c.h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.h.ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", c.h.cfg.Origin())
	for k, v := range c.extra {
		req.Header.Set(k, v)
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
	for _, ck := range res.Cookies() {
		if ck.Name == c.h.srv.cookieName() {
			c.cookie = ck.Value
		}
	}
	return resp{Status: res.StatusCode, Body: b}
}

func (c *client) upload(sessionID string, data []byte) resp {
	c.h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "x.png")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.h.ts.URL+"/api/sessions/"+sessionID+"/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", c.h.cfg.Origin())
	req.AddCookie(&http.Cookie{Name: c.h.srv.cookieName(), Value: c.cookie})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Body: b}
}

// admin creates the first account through the setup flow and signs it in.
func (h *harness) admin() *client {
	h.t.Helper()
	tok, err := h.auth.PrepareSetup(context.Background())
	if err != nil || tok == "" {
		h.t.Fatalf("setup token: %v", err)
	}
	c := &client{h: h}
	r := c.do("POST", "/api/setup", map[string]string{"token": tok, "username": "root", "password": "root-password-1"})
	if r.Status != 200 {
		h.t.Fatalf("setup: %d %s", r.Status, r.Body)
	}
	return c
}

// member creates a new account through an invite.
func (h *harness) member(admin *client, name string) *client {
	h.t.Helper()
	r := admin.do("POST", "/api/admin/invites", map[string]any{"role": "member"})
	if r.Status != 200 {
		h.t.Fatalf("invite: %d %s", r.Status, r.Body)
	}
	link := r.json()["url"].(string)
	tok := link[strings.LastIndex(link, "/")+1:]
	c := &client{h: h}
	r = c.do("POST", "/api/invites/"+tok+"/redeem", map[string]string{"username": name, "password": name + "-password-1"})
	if r.Status != 200 {
		h.t.Fatalf("redeem: %d %s", r.Status, r.Body)
	}
	return c
}

func (h *harness) sshServer(sftpRoot string) *testutil.SSHServer {
	return testutil.StartSSHServer(h.t, "alice", "alice-pass", sftpRoot)
}

func mustStatus(t *testing.T, r resp, want int, what string) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("%s: status %d, want %d: %s", what, r.Status, want, r.Body)
	}
}

func storeAuditQuery() store.AuditQuery { return store.AuditQuery{Limit: 500} }
