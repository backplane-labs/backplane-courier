// Package netguard keeps courier's posts to URLs products hand it
// (Web Push endpoints, webhooks) off the installation's own network.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// ErrForbiddenAddress: the destination's host resolved to an address
// inside a network — loopback, private, link-local, shared, unspecified,
// multicast, another special-purpose range, or IPv6 embedding IPv4 (NAT64,
// 6to4, Teredo). Checked on the address dialed, after DNS, so a name that
// resolves inside (or rebinds to it) is refused too.
var ErrForbiddenAddress = errors.New("destination resolves to an internal address")

// Dialing of the guarded transport.
const (
	dialTimeout = 30 * time.Second
	keepAlive   = 30 * time.Second
)

// globalUnicast6 is the only IPv6 range courier posts to: everything outside
// it is not global unicast — IPv4-compatible and SIIT forms, NAT64,
// discard, unique-local, link-local, site-local, multicast.
//
//nolint:gochecknoglobals // a constant prefix
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// special are the IANA special-purpose ranges inside IPv4 and 2000::/3 that
// netip does not classify: not globally reachable, or embedding an IPv4
// address a relay would reach (Teredo, 6to4), so an internal one,
// 169.254.169.254 included, would pass under them.
//
//nolint:gochecknoglobals // constant prefixes
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (carrier-grade NAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking, often used inside networks
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast included
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments, Teredo included
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4
}

// Forbidden reports whether courier refuses to post to addr. A zoned
// address is scoped to an interface, never public.
func Forbidden(addr netip.Addr) bool {
	if addr.Zone() != "" {
		return true
	}

	addr = addr.Unmap()

	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() ||
		(addr.Is6() && !globalUnicast6.Contains(addr)) {
		return true
	}

	for _, p := range special {
		if p.Contains(addr) {
			return true
		}
	}

	return false
}

// Transport dials only public addresses and goes direct — through a proxy
// the address courier dials is the proxy's, not the destination's.
func Transport() *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive, Control: refuseInternal}

	t, _ := http.DefaultTransport.(*http.Transport)
	t = t.Clone()
	t.Proxy = nil
	t.DialContext = dialer.DialContext

	return t
}

// refuseInternal is the dialer's check of the resolved address.
func refuseInternal(_, address string, _ syscall.RawConn) error {
	addr, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrForbiddenAddress, address)
	}

	if Forbidden(addr.Addr()) {
		return fmt.Errorf("%w: %s", ErrForbiddenAddress, addr.Addr())
	}

	return nil
}

// Allowed reports whether host matches hosts: an exact name or *.suffix;
// never an IP address, zoned ones included.
func Allowed(host string, hosts []string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))

	for _, h := range hosts {
		h = strings.ToLower(h)
		if suffix, ok := strings.CutPrefix(h, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == h {
			return true
		}
	}

	return false
}
