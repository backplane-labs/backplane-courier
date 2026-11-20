package admin_test

import (
	"testing"

	"github.com/backplane-labs/backplane/pkg/backplane/deps"

	"github.com/backplane-labs/backplane-courier/internal/admin"
	"github.com/backplane-labs/backplane-courier/internal/channels"
	"github.com/backplane-labs/backplane-courier/internal/webpush"
)

func TestHostsText(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		hosts    []string
		insecure bool
		want     string
	}{
		"default":         {want: "fallback"},
		"hosts":           {hosts: []string{"a.example", "*.b.example"}, want: "a.example, *.b.example"},
		"insecure":        {insecure: true, want: "any host; http and internal addresses allowed"},
		"insecure, hosts": {hosts: []string{"push-fake"}, insecure: true, want: "push-fake; http and internal addresses allowed"},
	} {
		if got := admin.HostsText(c.hosts, c.insecure, "fallback"); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// The console shows the key browsers subscribe with and where it comes
// from, the minted pair included.
func TestWebPushDetails(t *testing.T) {
	t.Parallel()

	pair, err := webpush.MintPair()
	if err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		cfg  webpush.Config
		keys string
	}{
		"minted":     {cfg: webpush.Config{Subject: "mailto:ops@example.com"}, keys: "minted by courier, kept in NATS KV courier_webpush"},
		"configured": {cfg: webpush.Config{Subject: "mailto:ops@example.com", PublicKey: pair.Public}, keys: "configuration"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ch := &channels.Channel{
				Kind: channels.ProviderWebPush, Spec: channels.Spec{WebPush: &c.cfg},
				WebPush: webpush.New(c.cfg, nil, deps.Static(pair)),
			}

			got := admin.Details(ch)
			if got["public_key"] != pair.Public || got["keys"] != c.keys {
				t.Fatalf("details = %v", got)
			}
		})
	}
}
