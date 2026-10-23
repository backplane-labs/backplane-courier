package webpush

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrForbiddenAddress: the endpoint's host resolved to an address inside a
// network — loopback, private, link-local, shared, unspecified or
// multicast. Checked on the address dialed, after DNS, so a name that
// resolves inside (or rebinds to it) is refused too.
var ErrForbiddenAddress = errors.New("webpush: endpoint resolves to an internal address")

// Dialing of the default client.
const (
	dialTimeout = 30 * time.Second
	keepAlive   = 30 * time.Second
)

// sharedSpace is RFC 6598's carrier-grade NAT range, which netip does not
// count as private.
var sharedSpace = netip.MustParsePrefix("100.64.0.0/10") //nolint:gochecknoglobals // a constant prefix

// Forbidden reports whether courier refuses to post to ip.
func Forbidden(ip netip.Addr) bool {
	ip = ip.Unmap()

	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		sharedSpace.Contains(ip) || (ip.Is4() && ip.As4()[0] == 0)
}

// guarded is the default client's transport: it dials only public
// addresses and goes direct — through a proxy the address courier dials is
// the proxy's, not the endpoint's.
func guarded() *http.Transport {
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
