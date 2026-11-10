package webpush_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/backplane-labs/backplane-courier/internal/webpush"
	"github.com/backplane-labs/backplane-courier/provider"
)

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

// An insecure channel posts over http to a push service inside, a test
// stand's, whose refusal of a gone subscription is final.
func TestInsecurePostsInside(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(srv.Close)

	cfg := channel(t)
	cfg.Insecure = true

	m := webpush.Message{Subscription: newBrowser(t).subscription(srv.URL + "/push/gone"), Payload: []byte(`{}`)}
	if err := m.Validate(cfg); err != nil {
		t.Fatal(err)
	}

	_, err := webpush.New(cfg, nil).Send(context.Background(), m)

	var refusal *provider.Error
	if !errors.As(err, &refusal) || refusal.Code != "gone" || !refusal.Permanent || hits.Load() != 1 {
		t.Fatalf("insecure channel: %v (%d posts)", err, hits.Load())
	}
}
