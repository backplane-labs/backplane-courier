package plugin_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

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

	var perr *provider.Error
	if _, err := client.Send(t.Context(), provider.Delivery{Attempt: 2}); !errors.As(err, &perr) || perr.RetryAfter != 3*time.Second || perr.Permanent {
		t.Fatalf("later: %v", err)
	}

	if err := client.Check(t.Context(), provider.Channel{}); !errors.As(err, &perr) || !perr.Permanent || perr.Code != "AUTH" {
		t.Fatalf("check: %v", err)
	}

	stop()

	// Gone: a retryable failure that says so.
	deadline := time.Now().Add(5 * time.Second)

	for {
		_, err := client.Send(t.Context(), provider.Delivery{Attempt: 1})
		if errors.As(err, &perr) && !perr.Permanent && perr.Code == "PLUGIN_Unavailable" && strings.Contains(perr.Message, "not reachable") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("down: %v", err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	deadline = time.Now().Add(5 * time.Second)
	for client.Health(t.Context()) == nil {
		if time.Now().After(deadline) {
			t.Fatal("healthy after it stopped")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// A server that is not a courier plugin: the call is final, not retried.
func TestNotAPlugin(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "x.sock")

	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	srv := grpc.NewServer()

	go func() { _ = srv.Serve(lis) }()

	t.Cleanup(srv.Stop)

	client, err := plugin.Dial("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	var perr *provider.Error
	if _, err := client.Send(t.Context(), provider.Delivery{Attempt: 1}); !errors.As(err, &perr) || !perr.Permanent || perr.Code != "PLUGIN_Unimplemented" {
		t.Fatalf("unimplemented: %v", err)
	}
}

// A plugin that comes back is served again within seconds.
func TestReconnect(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "r.sock")

	serve := func() context.CancelFunc {
		ctx, stop := context.WithCancel(context.Background())

		lis, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
		if err != nil {
			t.Fatal(err)
		}

		go func() { _ = plugin.ServeListener(ctx, lis, echo{}) }()

		return stop
	}

	stop := serve()

	client, err := plugin.Dial("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	waitHealth := func(ok bool) {
		t.Helper()

		deadline := time.Now().Add(6 * time.Second)
		for (client.Health(t.Context()) == nil) != ok {
			if time.Now().After(deadline) {
				t.Fatalf("health not %v", ok)
			}

			time.Sleep(100 * time.Millisecond)
		}
	}

	waitHealth(true)
	stop()
	waitHealth(false)

	// Several failed probes grow gRPC's backoff; the plugin comes back.
	for range 10 {
		_ = client.Health(t.Context())
	}

	time.Sleep(time.Second)

	stop = serve()
	defer stop()

	waitHealth(true)
}

type slow struct{ echo }

func (slow) Send(ctx context.Context, _ provider.Delivery) ([]byte, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

// A plugin slower than the channel's timeout fails retryably, saying so.
func TestSlowPlugin(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "s.sock")

	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = plugin.ServeListener(t.Context(), lis, slow{}) }()

	client, err := plugin.Dial("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	var perr *provider.Error
	if _, err := client.Send(ctx, provider.Delivery{}); !errors.As(err, &perr) || perr.Permanent ||
		perr.Code != "PLUGIN_DeadlineExceeded" || !strings.Contains(perr.Message, "did not answer in time") {
		t.Fatalf("slow: %v", err)
	}
}
