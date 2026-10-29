package netguard_test

import (
	"net/netip"
	"testing"

	"github.com/backplane-labs/backplane-courier/internal/netguard"
)

func TestForbidden(t *testing.T) {
	t.Parallel()

	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.64.1.1": true, "0.0.0.0": true, "0.1.2.3": true, "224.0.0.1": true, "::1": true, "::": true,
		"fe80::1": true, "fc00::1": true, "fd12::1": true, "ff02::1": true, "::ffff:10.0.0.1": true,
		// IANA special-purpose IPv4: not globally reachable.
		"192.0.0.8": true, "192.0.2.1": true, "198.18.0.1": true, "198.19.255.254": true, "198.51.100.1": true,
		"203.0.113.1": true, "240.0.0.1": true, "255.255.255.255": true,
		// IPv6 that embeds IPv4 (NAT64, 6to4, Teredo) reaches it through a gateway; deprecated site-local; discard.
		"64:ff9b::a9fe:a9fe": true, "64:ff9b:1::1": true, "2002:a9fe:a9fe::1": true, "2001:0:4136:e378::1": true,
		"fec0::1": true, "100::1": true, "2001:db8::1": true, "2001:2::1": true,
		// IPv4-compatible and SIIT forms; zoned (scoped) addresses of any kind.
		"::a9fe:a9fe": true, "::ffff:0:a9fe:a9fe": true, "64:ff9b::a9fe:a9fe%eth0": true,
		"2a00:1450:4001::1%eth0": true, "3fff::1": false,
		"8.8.8.8": false, "142.250.74.10": false, "2001:4860:4860::8888": false, "198.20.0.1": false,
		"2a00:1450:4001::1": false,
	} {
		if got := netguard.Forbidden(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()

	hosts := []string{"hooks.example.com", "*.bots.example.org"}
	for host, want := range map[string]bool{
		"hooks.example.com": true, "HOOKS.example.com.": true, "a.bots.example.org": true,
		"bots.example.org": false, "evil.example.com": false, "8.8.8.8": false, "fe80::1%eth0": false,
		"2a00:1450:4001::1": false,
	} {
		if got := netguard.Allowed(host, hosts); got != want {
			t.Errorf("%s: %v", host, got)
		}
	}
}
