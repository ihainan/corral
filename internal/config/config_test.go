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

	t.Setenv("CORRAL_PUBLIC_URL", "https://ssh.example.com")
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
