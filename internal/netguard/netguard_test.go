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
		"8.8.8.8": false, "142.250.74.10": false, "2001:4860:4860::8888": false,
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
		"bots.example.org": false, "evil.example.com": false, "8.8.8.8": false,
	} {
		if got := netguard.Allowed(host, hosts); got != want {
			t.Errorf("%s: %v", host, got)
		}
	}
}
