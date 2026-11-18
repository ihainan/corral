// Package netguard decides which network addresses the gateway may connect to.
//
// The gateway sits inside the network, so anything that lets a user choose a
// destination (SSH targets, later port forwards) must pass through here. Names
// are resolved once, every resulting address is checked, and the caller dials
// the checked address itself so a second DNS answer cannot swap it out.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

var ErrForbidden = errors.New("destination is not allowed")

type Guard struct {
	allowed  []netip.Prefix
	denied   []netip.Prefix
	resolver *net.Resolver
}

func New(allowed, denied []netip.Prefix) *Guard {
	return &Guard{allowed: allowed, denied: denied, resolver: net.DefaultResolver}
}

// Allowed says whether a may be connected to: it is in an allowed network
// and in no denied one, and so is every IPv4 address an IPv6 one carries
// (NAT64, 6to4, Teredo, IPv4-compatible): 64:ff9b::a9fe:a9fe is
// 169.254.169.254 by way of a NAT64 gateway.
func (g *Guard) Allowed(a netip.Addr) bool {
	a = a.Unmap()
	if !g.allowedOne(a) {
		return false
	}
	for _, v4 := range embedded4(a) {
		if !g.allowedOne(v4) {
			return false
		}
	}
	return true
}

var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	siit       = netip.MustParsePrefix("::ffff:0:0:0/96") // (IPv4-translated, RFC 2765: not the mapped ::ffff:0:0/96)
	sixToFour  = netip.MustParsePrefix("2002::/16")
	teredo     = netip.MustParsePrefix("2001::/32")
	compat4    = netip.MustParsePrefix("::/96")
)

// embedded4: the IPv4 addresses an IPv6 address may stand for, by the
// translations that reach one from it (RFC 6052, 8215, 2765, 3056, 4380,
// 4291). A network's own NAT64 prefix (RFC 6052's network-specific) cannot
// be known here: its addresses are taken as IPv6 ones.
func embedded4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	v4 := func(x, y, z, w byte) netip.Addr { return netip.AddrFrom4([4]byte{x, y, z, w}) }
	switch {
	case nat64.Contains(a), siit.Contains(a):
		return []netip.Addr{v4(b[12], b[13], b[14], b[15])}
	case compat4.Contains(a): // (not :: or ::1, nor another of 0.0.0.0/8: never an IPv4 one)
		if b[12] == 0 {
			return nil
		}
		return []netip.Addr{v4(b[12], b[13], b[14], b[15])}
	case nat64Local.Contains(a):
		// (The local-use block takes a prefix of any of RFC 6052's lengths
		// within it, and which one the network uses is not known here:
		// each reading is held to the rules, but those that come out in
		// 0.0.0.0/8 — the zeros of another length's prefix, never an
		// address a NAT64 is given.)
		var out []netip.Addr
		for _, c := range []netip.Addr{
			v4(b[6], b[7], b[9], b[10]),    // /48
			v4(b[7], b[9], b[10], b[11]),   // /56
			v4(b[9], b[10], b[11], b[12]),  // /64
			v4(b[12], b[13], b[14], b[15]), // /96
		} {
			if c.As4()[0] != 0 {
				out = append(out, c)
			}
		}
		return out
	case sixToFour.Contains(a):
		return []netip.Addr{v4(b[2], b[3], b[4], b[5])}
	case teredo.Contains(a): // (its server, and its client obscured)
		return []netip.Addr{v4(b[4], b[5], b[6], b[7]), v4(^b[12], ^b[13], ^b[14], ^b[15])}
	}
	return nil
}

func (g *Guard) allowedOne(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range g.denied {
		if p.Contains(a) {
			return false
		}
	}
	for _, p := range g.allowed {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve returns the permitted addresses for host. If a name resolves to any
// forbidden address the whole name is rejected, rather than quietly picking a
// permitted one: mixed answers usually mean something is off.
func (g *Guard) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if !g.Allowed(a) {
			return nil, fmt.Errorf("%w: %s", ErrForbidden, a)
		}
		return []netip.Addr{a.Unmap()}, nil
	}
	addrs, err := g.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", host)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if !g.Allowed(a) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrForbidden, host, a.Unmap())
		}
		out = append(out, a.Unmap())
	}
	return out, nil
}

// DialTCP resolves, checks and connects, trying each permitted address in turn.
func (g *Guard) DialTCP(ctx context.Context, host string, port int) (net.Conn, error) {
	addrs, err := g.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	var lastErr error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// CheckLiteral refuses an address that is never a host, whatever the network:
// unspecified, link-local (cloud metadata lives there) and multicast. It is
// for destinations the gateway does not dial itself (the second hop through
// a jump host, which resolves names on its side); names pass.
func CheckLiteral(host string) error {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	a = a.Unmap()
	if a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%w: %s", ErrForbidden, a)
	}
	return nil
}
