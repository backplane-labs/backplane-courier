package webpush

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/backplane-labs/backplane/pkg/backplane/deps"
	infranats "github.com/backplane-labs/backplane/pkg/backplane/infra/nats"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bucket keeps the minted VAPID pairs, one key per channel. It holds
// private keys: only courier's NATS identity may use $KV.courier_webpush.>.
const Bucket = "courier_webpush"

// Store mints a channel's pair once into Bucket and keeps it there: a
// subscription is bound to the public key, so the pair never rotates.
type Store struct {
	conn     deps.Dependency[*natsgo.Conn]
	cfg      infranats.Config
	replicas int
}

// NewStore is the pairs' store on conn (cfg is its section: JetStream API
// prefix and timeouts), the bucket replicated replicas times.
func NewStore(conn deps.Dependency[*natsgo.Conn], cfg infranats.Config, replicas int) Store {
	return Store{conn: conn, cfg: cfg, replicas: max(replicas, 1)}
}

// Pair is channel's pair as a dependency: provided once the store answers,
// the same for every replica.
func (s Store) Pair(parent deps.Scope, channel string) deps.Dependency[Pair] {
	return deps.NewDependency(parent, deps.Func(func(ctx context.Context) (Pair, error) {
		return s.pair(ctx, channel)
	}), deps.Name("webpush keys "+channel))
}

func (s Store) pair(ctx context.Context, channel string) (Pair, error) {
	js, err := s.cfg.JetStream(s.conn.Get())
	if err != nil {
		return Pair{}, fmt.Errorf("webpush: keys: %w", err)
	}

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: Bucket, Description: "VAPID key pairs of courier's Web Push channels", History: 1, Replicas: s.replicas,
	})
	if err != nil {
		return Pair{}, fmt.Errorf("webpush: keys: bucket: %w", err)
	}

	raw, err := infranats.MintOnce(ctx, kv, keyOf(channel), func() ([]byte, error) {
		pair, err := MintPair()
		if err != nil {
			return nil, err
		}

		return json.Marshal(pair)
	})
	if err != nil {
		return Pair{}, fmt.Errorf("webpush: keys: %w", err)
	}

	var pair Pair
	if err := json.Unmarshal(raw, &pair); err != nil {
		return Pair{}, fmt.Errorf("webpush: keys: stored pair: %w", err)
	}

	if err := checkPair(pair.Public, pair.Private); err != nil {
		return Pair{}, fmt.Errorf("webpush: keys: stored pair: %w", err)
	}

	return pair, nil
}

// keyOf is a channel's KV key: any channel name, in the key's alphabet.
func keyOf(channel string) string { return base64.RawURLEncoding.EncodeToString([]byte(channel)) }
