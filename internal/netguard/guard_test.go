package netguard

import (
	"net/netip"
	"strings"
	"testing"
)

func prefixes(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range strings.Split(s, ",") {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

// The default as config gives it: everywhere, but loopback and what is never a host.
func TestAllowedDefaults(t *testing.T) {
	g := New(prefixes("0.0.0.0/0,::/0"), prefixes("0.0.0.0/8,169.254.0.0/16,100.100.100.200/32,168.63.129.16/32,224.0.0.0/4,240.0.0.0/4,::/128,fe80::/10,fd00:ec2::254/128,3fff::/20,ff00::/8,127.0.0.0/8,::1/128"))
	for a, want := range map[string]bool{
		"203.0.114.7":                   true,  // a public VPS
		"192.168.1.10":                  true,  // a LAN
		"100.101.102.103":               true,  // Tailscale
		"fd7a:115c:a1e0::1":             true,  // Tailscale IPv6
		"2606:4700::1111":               true,  // public IPv6
		"64:ff9b::cb00:7207":            true,  // NAT64 of a public address
		"127.0.0.1":                     false, // the gateway's own machine
		"::1":                           false,
		"::ffff:127.0.0.1":              false,
		"169.254.169.254":               false, // AWS/GCP/Azure metadata
		"100.100.100.200":               false, // Alibaba Cloud metadata
		"fd00:ec2::254":                 false, // AWS IPv6 metadata
		"64:ff9b::a9fe:a9fe":            false, // 169.254.169.254 through NAT64
		"64:ff9b:1:a9fe:a9:fe00::":      false, // the same, a local NAT64 /48
		"2002:a9fe:a9fe::1":             false, // the same, 6to4
		"2002:7f00:1::1":                false, // 127.0.0.1, 6to4
		"2001:0:a9fe:a9fe::":            false, // a Teredo server at 169.254.169.254
		"2001:0:cb00:7207::80ff:fefe":   false, // a Teredo client at 127.0.1.1
		"::a9fe:a9fe":                   false, // IPv4-compatible 169.254.169.254
		"::ffff:0:a9fe:a9fe":            false, // IPv4-translated (SIIT) 169.254.169.254
		"::ffff:0:7f00:1":               false, // the same, 127.0.0.1
		"64:ff9b:1:808:8:800:a9fe:a9fe": false, // local NAT64 by a /96 prefix: 169.254.169.254
		"64:ff9b:1::7f00:1":             false, // the same, 127.0.0.1
		"64:ff9b:1::808:808":            true,  // the same, 8.8.8.8
		"168.63.129.16":                 false, // Azure WireServer
		"3fff::1":                       false, // IPv6 documentation (RFC 9637)
		"224.0.0.1":                     false,
		"0.0.0.0":                       false,
	} {
		if got := g.Allowed(netip.MustParseAddr(a)); got != want {
			t.Errorf("%s: allowed %v, want %v", a, got, want)
		}
	}
}

// An explicit list is what it says: loopback when named, and an embedded
// IPv4 address held to the same list.
func TestAllowedExplicitList(t *testing.T) {
	g := New(prefixes("127.0.0.1/32,::1/128,10.0.0.0/8,64:ff9b::/96"), prefixes("169.254.0.0/16"))
	for a, want := range map[string]bool{
		"127.0.0.1":          true,
		"::1":                true,
		"10.1.2.3":           true,
		"64:ff9b::a01:203":   true,  // 10.1.2.3
		"64:ff9b::cb00:7207": false, // 203.0.114.7: not in the list
		"192.168.1.1":        false,
	} {
		if got := g.Allowed(netip.MustParseAddr(a)); got != want {
			t.Errorf("%s: allowed %v, want %v", a, got, want)
		}
	}
}
