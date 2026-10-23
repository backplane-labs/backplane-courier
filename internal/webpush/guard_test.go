package webpush_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"

	"github.com/backplane-labs/backplane-courier/internal/webpush"
	"github.com/backplane-labs/backplane-courier/provider"
)

func TestForbidden(t *testing.T) {
	t.Parallel()

	for addr, want := range map[string]bool{
		"127.0.0.1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "169.254.169.254": true,
		"100.64.1.1": true, "0.0.0.0": true, "0.1.2.3": true, "224.0.0.1": true, "::1": true, "::": true,
		"fe80::1": true, "fc00::1": true, "fd12::1": true, "ff02::1": true, "::ffff:10.0.0.1": true,
		"8.8.8.8": false, "142.250.74.10": false, "2001:4860:4860::8888": false,
	} {
		if got := webpush.Forbidden(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

// The default client checks the address it dials, after DNS: an allowed
// name that resolves inside is refused for good, and nothing is posted.
func TestDialsOnlyPublicAddresses(t *testing.T) {
	t.Parallel()

	hit := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(srv.Close)

	served, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := channel(t)
	cfg.Hosts = []string{"localhost"}

	_, err = webpush.New(cfg, nil).Send(context.Background(), webpush.Message{
		Subscription: newBrowser(t).subscription("https://localhost:" + served.Port() + "/push"), Payload: []byte(`{}`),
	})

	var refusal *provider.Error
	if !errors.As(err, &refusal) || refusal.Code != webpush.CodeForbidden || !refusal.Permanent || hit {
		t.Fatalf("internal address: %v (posted %v)", err, hit)
	}
}
