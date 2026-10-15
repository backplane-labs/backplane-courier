package courier //nolint:testpackage // exercises the unexported plugin dependency

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	sp "github.com/gopherex/schemapb/go/schemapb"

	"github.com/backplane-labs/backplane-courier/plugin"
	"github.com/backplane-labs/backplane-courier/provider"
)

type versioned struct{ kind string }

func (v *versioned) Describe(context.Context) (provider.Descriptor, error) {
	type msg struct {
		To string `json:"to"`
	}

	s, err := sp.ReflectType[msg](sp.ID(sp.Namespace("x"), sp.SchemaName("m"), sp.Ver(1, 0, 0)))

	return provider.Descriptor{Kind: v.kind, Message: s}, err
}

func (v *versioned) Check(context.Context, provider.Channel) error { return nil }

func (v *versioned) Send(context.Context, provider.Delivery) ([]byte, error) { return nil, nil }

// A plugin that serves but now describes another contract is not ready.
func TestPluginDependency(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "p.sock")

	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	p := &versioned{kind: "sms"}

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)

	go func() { _ = plugin.ServeListener(ctx, lis, p) }()

	client, err := plugin.Dial("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	want, err := client.WaitDescribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	dep := pluginDependency{name: "sms", client: client, want: want}
	if err := dep.Probe(t.Context(), client); err != nil {
		t.Fatalf("ready: %v", err)
	}

	p.kind = "pager"

	if err := dep.Probe(t.Context(), client); !errors.Is(err, ErrContractChanged) {
		t.Fatalf("contract changed: %v", err)
	}

	stop()

	if err := dep.Probe(t.Context(), client); err == nil {
		t.Fatal("ready while the plugin is gone")
	}
}
