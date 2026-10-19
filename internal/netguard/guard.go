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

func (g *Guard) Allowed(a netip.Addr) bool {
	a = a.Unmap()
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
