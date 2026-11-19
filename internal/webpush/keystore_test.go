package webpush_test

import (
	"sync"
	"testing"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/backplanetest"
	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/backplane-labs/backplane/pkg/backplane/deps"
	infranats "github.com/backplane-labs/backplane/pkg/backplane/infra/nats"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/backplane-labs/backplane-courier/internal/webpush"
)

// jetStream is an embedded NATS server with JetStream for one test.
func jetStream(t *testing.T) string {
	t.Helper()

	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	go srv.Start()
	t.Cleanup(srv.Shutdown)

	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}

	return srv.ClientURL()
}

// replica is one courier process: its own connection and store.
func replica(t *testing.T, url, channel string) webpush.Pair {
	t.Helper()

	h := backplanetest.New(t)
	section := infranats.Config{URL: config.Secret(url)}
	conn := deps.NewDependency(h.Root(), infranats.New(section))
	pair := webpush.NewStore(conn, section, 1).Pair(h.Root(), channel)
	h.Start()

	return pair.Get()
}

// Replicas starting together on a fresh store end with one pair, valid,
// and a replica started later reads the same: a subscription stays bound.
func TestStoreMintsOnePairForAllReplicas(t *testing.T) {
	t.Parallel()

	url := jetStream(t)

	var wg sync.WaitGroup

	pairs := make([]webpush.Pair, 4)
	for i := range pairs {
		wg.Go(func() { pairs[i] = replica(t, url, "browser push") })
	}

	wg.Wait()

	for _, p := range pairs {
		if p != pairs[0] || p.Public == "" {
			t.Fatalf("replicas disagree: %+v", pairs)
		}
	}

	cfg := webpush.Config{Subject: "mailto:ops@example.com", PublicKey: pairs[0].Public, PrivateKey: config.Secret(pairs[0].Private)}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("minted pair does not validate: %v", err)
	}

	if later := replica(t, url, "browser push"); later != pairs[0] {
		t.Fatalf("a later replica minted again: %+v", later)
	}

	if other := replica(t, url, "other"); other == pairs[0] {
		t.Fatal("another channel shares the pair")
	}
}
