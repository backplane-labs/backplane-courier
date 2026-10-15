package plugin_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/backplane-labs/backplane-courier/plugin"
	"github.com/backplane-labs/backplane-courier/provider"
)

type echo struct{}

func (echo) Describe(context.Context) (provider.Descriptor, error) {
	return provider.Descriptor{Kind: "echo"}, nil
}

func (echo) Check(context.Context, provider.Channel) error { return provider.Permanent("AUTH", "no") }

func (echo) Send(_ context.Context, d provider.Delivery) ([]byte, error) {
	if d.Attempt > 1 {
		return nil, provider.Later("BUSY", "later", 3*time.Second)
	}

	return d.Message, nil
}

// A plugin that starts after courier is waited for; its health follows
// its life; failures cross the wire with their kind.
func TestPlugin(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "p.sock")

	client, err := plugin.Dial("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	if err := client.Health(t.Context()); err == nil {
		t.Fatal("healthy before it runs")
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go func() {
		time.Sleep(300 * time.Millisecond)

		lis, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
		if err != nil {
			return
		}

		_ = plugin.ServeListener(ctx, lis, echo{})
	}()

	wait, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	d, err := client.WaitDescribe(wait)
	if err != nil || d.Kind != "echo" {
		t.Fatalf("describe: %+v %v", d, err)
	}

	if err := client.Health(t.Context()); err != nil {
		t.Fatalf("health: %v", err)
	}

	if out, err := client.Send(t.Context(), provider.Delivery{Message: []byte(`{"a":1}`), Attempt: 1}); err != nil || string(out) != `{"a":1}` {
		t.Fatalf("send: %s %v", out, err)
	}

	var pe *provider.Error
	if _, err := client.Send(t.Context(), provider.Delivery{Attempt: 2}); !errors.As(err, &pe) || pe.RetryAfter != 3*time.Second || pe.Permanent {
		t.Fatalf("later: %v", err)
	}

	if err := client.Check(t.Context(), provider.Channel{}); !errors.As(err, &pe) || !pe.Permanent || pe.Code != "AUTH" {
		t.Fatalf("check: %v", err)
	}

	stop()

	deadline := time.Now().Add(5 * time.Second)
	for client.Health(t.Context()) == nil {
		if time.Now().After(deadline) {
			t.Fatal("healthy after it stopped")
		}

		time.Sleep(50 * time.Millisecond)
	}
}
