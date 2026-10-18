package config

import (
	"strings"
	"testing"
)

func TestForwardDomainRequiresHTTPS(t *testing.T) {
	t.Setenv("CORRAL_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CORRAL_FORWARD_DOMAIN", "fwd.example.com")

	t.Setenv("CORRAL_PUBLIC_URL", "http://ssh.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("forwarding over plain HTTP was accepted: %v", err)
	}

	// A loopback IP shares no parent domain with the forwards: allowed for
	// local development, when the server also listens on loopback only. A
	// loopback name is still refused.
	t.Setenv("CORRAL_PUBLIC_URL", "http://127.0.0.1:8080")
	for _, listen := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		t.Setenv("CORRAL_LISTEN", listen)
		if _, err := Load(); err != nil {
			t.Fatalf("loopback development setup on %s refused: %v", listen, err)
		}
	}
	for _, listen := range []string{"0.0.0.0:8080", ":8080", "10.1.2.3:8080", "localhost:8080"} {
		t.Setenv("CORRAL_LISTEN", listen)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CORRAL_LISTEN") {
			t.Fatalf("plain-HTTP forwarding listening on %s accepted: %v", listen, err)
		}
	}
	t.Setenv("CORRAL_LISTEN", "127.0.0.1:8080")
	t.Setenv("CORRAL_PUBLIC_URL", "http://localhost:8080")
	if _, err := Load(); err == nil {
		t.Fatal("forwarding over plain HTTP on a host name was accepted")
	}

	t.Setenv("CORRAL_PUBLIC_URL", "https://ssh.example.com")
	t.Setenv("CORRAL_LISTEN", "0.0.0.0:8080") // behind a TLS proxy: any address is fine
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ForwardDomain != "fwd.example.com" || !c.SecureCookies() {
		t.Fatalf("unexpected config: %+v", c)
	}

	// Without forwarding, plain HTTP stays allowed (local development).
	t.Setenv("CORRAL_FORWARD_DOMAIN", "")
	t.Setenv("CORRAL_PUBLIC_URL", "http://127.0.0.1:8080")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

// Browsers omit the default port from Origin; so must the configured origin.
func TestPublicURLDefaultPortIsDropped(t *testing.T) {
	t.Setenv("CORRAL_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CORRAL_FORWARD_DOMAIN", "")
	for raw, want := range map[string]string{
		"https://ssh.example.com:443":  "https://ssh.example.com",
		"http://127.0.0.1:80/":         "http://127.0.0.1",
		"http://[::1]:80":              "http://[::1]",
		"https://ssh.example.com:8443": "https://ssh.example.com:8443",
		"http://ssh.example.com:443":   "http://ssh.example.com:443",
	} {
		t.Setenv("CORRAL_PUBLIC_URL", raw)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.Origin() != want {
			t.Errorf("%s: origin %q, want %q", raw, c.Origin(), want)
		}
	}
}
