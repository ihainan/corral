// Package config loads Corral's runtime configuration from environment variables.
//
// Everything deployment-specific (domains, networks, keys) comes from here so the
// code itself never carries site details.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen    string
	DataDir   string
	PublicURL *url.URL

	MasterKey []byte

	// Targets a user may connect to. Addresses are checked after DNS resolution.
	AllowedNetworks []netip.Prefix
	DeniedNetworks  []netip.Prefix

	// Reverse proxies whose X-Forwarded-For we believe. Empty means trust nobody.
	TrustedProxies []netip.Prefix

	DetachTimeout      time.Duration
	LoginIdleTimeout   time.Duration
	LoginAbsoluteLimit time.Duration
	MaxSessionsPerUser int
	UploadMaxBytes     int64

	// Forwarded ports and editors are served from "<label>.<ForwardDomain>".
	// Empty disables both features.
	ForwardDomain string

	DefaultLanguage string
}

// SecureCookies reports whether cookies should carry the Secure attribute.
func (c *Config) SecureCookies() bool { return c.PublicURL.Scheme == "https" }

// Origin is the exact Origin header browsers send for the main site.
func (c *Config) Origin() string { return c.PublicURL.Scheme + "://" + c.PublicURL.Host }

const (
	defaultAllowed = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"
	// Link-local (incl. cloud metadata), unspecified, multicast and broadcast are never useful SSH targets.
	alwaysDenied = "0.0.0.0/8,169.254.0.0/16,224.0.0.0/4,240.0.0.0/4,::/128,fe80::/10,ff00::/8"
)

func Load() (*Config, error) {
	c := &Config{
		Listen:             env("CORRAL_LISTEN", "127.0.0.1:8080"),
		DataDir:            env("CORRAL_DATA_DIR", "./data"),
		DefaultLanguage:    env("CORRAL_DEFAULT_LANGUAGE", "zh-CN"),
		ForwardDomain:      strings.TrimPrefix(strings.ToLower(env("CORRAL_FORWARD_DOMAIN", "")), "."),
		MaxSessionsPerUser: 20,
		UploadMaxBytes:     100 << 20,
	}

	raw := env("CORRAL_PUBLIC_URL", "")
	if raw == "" {
		return nil, errors.New("CORRAL_PUBLIC_URL is required (e.g. https://ssh.example.com)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("CORRAL_PUBLIC_URL must be an absolute http(s) URL, got %q", raw)
	}
	c.PublicURL = &url.URL{Scheme: u.Scheme, Host: u.Host}
	// Forwarded ports are sibling subdomains that run arbitrary apps. Only the
	// __Host- cookie prefix, which needs HTTPS, stops one from planting a login
	// cookie for the parent domain that the main site would then accept.
	// A loopback IP address as the main site has no parent domain to share,
	// which keeps local development over plain HTTP possible.
	if c.ForwardDomain != "" && u.Scheme != "https" && !isLoopbackIP(u.Hostname()) {
		return nil, fmt.Errorf("CORRAL_FORWARD_DOMAIN needs CORRAL_PUBLIC_URL to use https:// (got %q): "+
			"over plain HTTP a forwarded app could set cookies for the main site", raw)
	}

	if c.MasterKey, err = loadMasterKey(); err != nil {
		return nil, err
	}

	if c.AllowedNetworks, err = parsePrefixes(env("CORRAL_ALLOWED_NETWORKS", defaultAllowed)); err != nil {
		return nil, fmt.Errorf("CORRAL_ALLOWED_NETWORKS: %w", err)
	}
	denied := alwaysDenied
	if extra := env("CORRAL_DENIED_NETWORKS", ""); extra != "" {
		denied += "," + extra
	}
	if c.DeniedNetworks, err = parsePrefixes(denied); err != nil {
		return nil, fmt.Errorf("CORRAL_DENIED_NETWORKS: %w", err)
	}
	if c.TrustedProxies, err = parsePrefixes(env("CORRAL_TRUSTED_PROXIES", "")); err != nil {
		return nil, fmt.Errorf("CORRAL_TRUSTED_PROXIES: %w", err)
	}

	if c.DetachTimeout, err = envDuration("CORRAL_DETACH_TIMEOUT", 24*time.Hour); err != nil {
		return nil, err
	}
	if c.LoginIdleTimeout, err = envDuration("CORRAL_LOGIN_IDLE_TIMEOUT", 7*24*time.Hour); err != nil {
		return nil, err
	}
	if c.LoginAbsoluteLimit, err = envDuration("CORRAL_LOGIN_MAX_AGE", 30*24*time.Hour); err != nil {
		return nil, err
	}
	if v := env("CORRAL_MAX_SESSIONS_PER_USER", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("CORRAL_MAX_SESSIONS_PER_USER: invalid %q", v)
		}
		c.MaxSessionsPerUser = n
	}
	if v := env("CORRAL_UPLOAD_MAX_MB", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("CORRAL_UPLOAD_MAX_MB: invalid %q", v)
		}
		c.UploadMaxBytes = int64(n) << 20
	}
	return c, nil
}

func loadMasterKey() ([]byte, error) {
	val := os.Getenv("CORRAL_MASTER_KEY")
	if path := os.Getenv("CORRAL_MASTER_KEY_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("CORRAL_MASTER_KEY_FILE: %w", err)
		}
		val = strings.TrimSpace(string(b))
	}
	if val == "" {
		return nil, errors.New("a master key is required to encrypt stored credentials: set CORRAL_MASTER_KEY " +
			"or CORRAL_MASTER_KEY_FILE to 32 random bytes in base64 (generate one with `corral genkey`); " +
			"keep it out of database backups")
	}
	key, err := base64.StdEncoding.DecodeString(val)
	if err != nil || len(key) != 32 {
		return nil, errors.New("master key must be exactly 32 bytes, base64-encoded")
	}
	return key, nil
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, err
			}
			part = netip.PrefixFrom(addr, addr.BitLen()).String()
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: invalid duration %q", key, v)
	}
	return d, nil
}

func isLoopbackIP(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
