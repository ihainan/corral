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

func TestCodeServerConfig(t *testing.T) {
	t.Setenv("CORRAL_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CORRAL_PUBLIC_URL", "https://ssh.example.com")
	t.Setenv("CORRAL_FORWARD_DOMAIN", "fwd.example.com")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CodeServer || c.CodeServerVersion != "4.138.0" || len(c.CodeServerSHA256["amd64"]) != 64 || len(c.CodeServerSHA256["arm64"]) != 64 ||
		c.CodeServerDownload("amd64") != "https://github.com/coder/code-server/releases/download/v4.138.0/code-server-4.138.0-linux-amd64.tar.gz" ||
		c.CodeServerFile("arm64") != "code-server-4.138.0-linux-arm64.tar.gz" {
		t.Fatalf("defaults: %+v %s", c, c.CodeServerDownload("amd64"))
	}
	t.Setenv("CORRAL_FORWARD_DOMAIN", "")
	if c, _ := Load(); c == nil || c.CodeServer {
		t.Fatal("code-server on without a forward domain")
	}
	t.Setenv("CORRAL_CODE_SERVER", "on")
	if _, err := Load(); err == nil {
		t.Fatal("CORRAL_CODE_SERVER=on accepted without a forward domain")
	}
	t.Setenv("CORRAL_FORWARD_DOMAIN", "fwd.example.com")
	for k, bad := range map[string][]string{
		"CORRAL_CODE_SERVER":         {"yes", "1"},
		"CORRAL_CODE_SERVER_VERSION": {"latest", "4.138", "4.138.0-rc1", "4.138.0; rm -rf ~", "v4.138.0"},
		"CORRAL_CODE_SERVER_URL": {"http://mirror.example/code-server-{version}.tar.gz", "https://mirror.example/latest.tar.gz",
			"https://mirror.example/{version}/$(id).tar.gz", "ftp://mirror.example/{version}"},
		"CORRAL_CODE_SERVER_DIR": {"/nonexistent/corral-code-server"},
	} {
		for _, v := range bad {
			t.Setenv(k, v)
			if _, err := Load(); err == nil {
				t.Errorf("%s=%q accepted", k, v)
			}
			t.Setenv(k, "")
		}
	}
	dir := t.TempDir()
	t.Setenv("CORRAL_CODE_SERVER_DIR", dir)
	t.Setenv("CORRAL_CODE_SERVER_URL", "https://mirror.example/cs/{version}/{arch}.tgz")
	t.Setenv("CORRAL_CODE_SERVER_VERSION", "4.100.2")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CORRAL_CODE_SERVER_SHA256") {
		t.Fatalf("a version without digests was accepted: %v", err)
	}
	for _, bad := range []string{"amd64=xyz", "amd64", "x86=" + strings.Repeat("a", 64), "amd64=" + strings.Repeat("a", 63)} {
		t.Setenv("CORRAL_CODE_SERVER_SHA256", bad)
		if _, err := Load(); err == nil {
			t.Errorf("CORRAL_CODE_SERVER_SHA256=%q accepted", bad)
		}
	}
	t.Setenv("CORRAL_CODE_SERVER_SHA256", "amd64="+strings.Repeat("A", 64)+", arm64="+strings.Repeat("b", 64))
	c, err = Load()
	if err != nil || c.CodeServerDir != dir || c.CodeServerDownload("arm64") != "https://mirror.example/cs/4.100.2/arm64.tgz" ||
		c.CodeServerSHA256["amd64"] != strings.Repeat("a", 64) || c.CodeServerSHA256["arm64"] != strings.Repeat("b", 64) {
		t.Fatalf("custom: %+v %v", c, err)
	}
	t.Setenv("CORRAL_CODE_SERVER", "off")
	if c, _ := Load(); c == nil || c.CodeServer {
		t.Fatal("off ignored")
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
