package apns_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/backplane-labs/backplane-courier/internal/apns"
	"github.com/backplane-labs/backplane-courier/internal/apns/apnstest"
	"github.com/backplane-labs/backplane-courier/provider"
)

const token = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

func ttl(v int64) *int64 { return &v }

func TestValidate(t *testing.T) {
	t.Parallel()

	cfg := apns.Config{Topic: "ru.komeet.app"}
	voip := apns.Message{Token: token, Topic: "ru.komeet.app.voip", PushType: "voip", Priority: 10, Payload: json.RawMessage(`{"k":"x"}`)}

	for name, c := range map[string]struct {
		msg  apns.Message
		cfg  apns.Config
		want error
	}{
		"alert with the channel's topic": {msg: apns.Message{Token: token, Payload: json.RawMessage(`{"aps":{}}`)}, cfg: cfg},
		"voip":                           {msg: voip, cfg: cfg},
		"voip, default priority":         {msg: apns.Message{Token: token, Topic: voip.Topic, PushType: "voip", Payload: voip.Payload}},
		"no token":                       {msg: apns.Message{Payload: voip.Payload}, cfg: cfg, want: apns.ErrToken},
		"token not hex":                  {msg: apns.Message{Token: "zz" + token[2:], Payload: voip.Payload}, cfg: cfg, want: apns.ErrToken},
		"odd token":                      {msg: apns.Message{Token: token[1:], Payload: voip.Payload}, cfg: cfg, want: apns.ErrToken},
		"no topic":                       {msg: apns.Message{Token: token, Payload: voip.Payload}, want: apns.ErrTopic},
		"unknown type":                   {msg: apns.Message{Token: token, PushType: "ring", Payload: voip.Payload}, cfg: cfg, want: apns.ErrPushType},
		"priority 7":                     {msg: apns.Message{Token: token, Priority: 7, Payload: voip.Payload}, cfg: cfg, want: apns.ErrPriority},
		"voip at priority 5":             {msg: apns.Message{Token: token, Topic: voip.Topic, PushType: "voip", Priority: 5, Payload: voip.Payload}, want: apns.ErrVoIP},
		"voip to the app topic":          {msg: apns.Message{Token: token, PushType: "voip", Payload: voip.Payload}, cfg: cfg, want: apns.ErrVoIP},
		"ttl too long":                   {msg: apns.Message{Token: token, TTLSeconds: ttl(apns.MaxTTLSeconds + 1), Payload: voip.Payload}, cfg: cfg, want: apns.ErrTTL},
		"negative ttl":                   {msg: apns.Message{Token: token, TTLSeconds: ttl(-1), Payload: voip.Payload}, cfg: cfg, want: apns.ErrTTL},
		"long collapse id":               {msg: apns.Message{Token: token, CollapseID: strings.Repeat("c", 65), Payload: voip.Payload}, cfg: cfg, want: apns.ErrCollapseID},
		"id not a uuid":                  {msg: apns.Message{Token: token, ID: "x", Payload: voip.Payload}, cfg: cfg, want: apns.ErrID},
		"payload an array":               {msg: apns.Message{Token: token, Payload: json.RawMessage(`[1]`)}, cfg: cfg, want: apns.ErrPayload},
		"payload missing":                {msg: apns.Message{Token: token}, cfg: cfg, want: apns.ErrPayload},
		"alert over 4096":                {msg: apns.Message{Token: token, Payload: big(4097)}, cfg: cfg, want: apns.ErrTooBig},
		"voip up to 5120":                {msg: apns.Message{Token: token, Topic: voip.Topic, PushType: "voip", Payload: big(5120)}},
		"voip over 5120":                 {msg: apns.Message{Token: token, Topic: voip.Topic, PushType: "voip", Payload: big(5121)}, want: apns.ErrTooBig},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := c.msg.Validate(c.cfg); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// big is a JSON object of exactly n bytes.
func big(n int) json.RawMessage {
	return json.RawMessage(`{"k":"` + strings.Repeat("x", n-8) + `"}`)
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	good := apnstest.Start(t).Config("ru.komeet.app")

	for name, c := range map[string]struct {
		change func(*apns.Config)
		ok     bool
	}{
		"valid":          {change: func(*apns.Config) {}, ok: true},
		"short key id":   {change: func(c *apns.Config) { c.KeyID = "K1" }},
		"lower team id":  {change: func(c *apns.Config) { c.TeamID = "team000001" }},
		"not a key":      {change: func(c *apns.Config) { c.Key = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n" }},
		"no key at all":  {change: func(c *apns.Config) { c.Key = "" }},
		"no topic is ok": {change: func(c *apns.Config) { c.Topic = "" }, ok: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := good
			c.change(&cfg)

			if err := cfg.Validate(); (err == nil) != c.ok || (err != nil && !errors.Is(err, apns.ErrConfig)) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// A VoIP push reaches APNs with every header it names, under a provider
// token the key signs; APNs' apns-id comes back.
func TestSendVoIP(t *testing.T) {
	t.Parallel()

	srv := apnstest.Start(t)

	client, err := apns.New(srv.Config("ru.komeet.app"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_700_000_000, 0)

	apns.SetNow(client, func() time.Time { return now })

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(t.Context(), "caller")
	t.Cleanup(func() { span.End() })

	id, err := client.Send(ctx, apns.Message{
		Token: token, Topic: "ru.komeet.app.voip", PushType: "voip", Priority: 10, TTLSeconds: ttl(30),
		CollapseID: "call-1", ID: "12345678-1234-4234-8234-123456789012", Payload: json.RawMessage(`{"komeet":"c2VhbGVk"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	pushes := srv.Pushes()
	if id != "12345678-1234-4234-8234-123456789012" || len(pushes) != 1 {
		t.Fatalf("id %q, pushes %v", id, pushes)
	}

	p := pushes[0]
	for header, want := range map[string]string{
		"Apns-Topic": "ru.komeet.app.voip", "Apns-Push-Type": "voip", "Apns-Priority": "10",
		"Apns-Expiration": "1700000030", "Apns-Collapse-Id": "call-1",
	} {
		if got := p.Headers.Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}

	if p.Token != token || string(p.Payload) != `{"komeet":"c2VhbGVk"}` || p.Issued != now.Unix() {
		t.Fatalf("push %+v", p)
	}

	// The push carries the caller's trace.
	if tp := p.Headers.Get("Traceparent"); !strings.Contains(tp, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q", tp)
	}
}

// ttl_seconds 0 is apns-expiration 0 (now or never); an alert without
// priority or ttl leaves those headers to APNs.
func TestSendDefaults(t *testing.T) {
	t.Parallel()

	srv := apnstest.Start(t)

	client, err := apns.New(srv.Config("ru.komeet.app"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	for _, msg := range []apns.Message{
		{Token: token, TTLSeconds: ttl(0), Payload: json.RawMessage(`{"aps":{"alert":"hi"}}`)},
		{Token: token, Payload: json.RawMessage(`{"aps":{"alert":"hi"}}`)},
	} {
		if _, err := client.Send(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
	}

	now, defaults := srv.Pushes()[0].Headers, srv.Pushes()[1].Headers
	if now.Get("Apns-Expiration") != "0" || now.Get("Apns-Topic") != "ru.komeet.app" || now.Get("Apns-Push-Type") != "alert" {
		t.Fatalf("headers %v", now)
	}

	if defaults.Get("Apns-Expiration") != "" || defaults.Get("Apns-Priority") != "" || defaults.Get("Apns-Collapse-Id") != "" {
		t.Fatalf("defaults %v", defaults)
	}
}

// APNs' reasons: a dead token and a malformed push are final, with the
// reason as the code; throttling, outages and an expired provider token
// are retried.
func TestRefusals(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		status    int
		reason    string
		permanent bool
	}{
		"unregistered":           {status: 410, reason: "Unregistered", permanent: true},
		"expired token":          {status: 410, reason: "ExpiredToken", permanent: true},
		"bad device token":       {status: 400, reason: "BadDeviceToken", permanent: true},
		"token not for topic":    {status: 400, reason: "DeviceTokenNotForTopic", permanent: true},
		"payload too large":      {status: 413, reason: "PayloadTooLarge", permanent: true},
		"topic disallowed":       {status: 400, reason: "TopicDisallowed", permanent: true},
		"expired provider token": {status: 403, reason: "ExpiredProviderToken"},
		"idle timeout":           {status: 400, reason: "IdleTimeout"},
		"too many requests":      {status: 429, reason: "TooManyRequests"},
		"internal error":         {status: 500, reason: "InternalServerError"},
		"unavailable":            {status: 503, reason: "ServiceUnavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := apnstest.Start(t)
			srv.Answer(token, apnstest.Answer{Status: c.status, Reason: c.reason})

			client, err := apns.New(srv.Config("ru.komeet.app"), srv.Client())
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Send(t.Context(), apns.Message{Token: token, Payload: json.RawMessage(`{}`)})

			var refusal *provider.Error
			if !errors.As(err, &refusal) || refusal.Code != c.reason || refusal.Permanent != c.permanent {
				t.Fatalf("got %#v", err)
			}
		})
	}
}

// The provider token is reused within its lifetime, signed again after
// it, and at once after APNs calls it expired.
func TestProviderToken(t *testing.T) {
	t.Parallel()

	srv := apnstest.Start(t)

	client, err := apns.New(srv.Config("ru.komeet.app"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	var now atomic.Int64

	now.Store(1_700_000_000)
	apns.SetNow(client, func() time.Time { return time.Unix(now.Load(), 0) })

	send := func() error {
		_, err := client.Send(t.Context(), apns.Message{Token: token, Payload: json.RawMessage(`{}`)})

		return err
	}

	issued := func() []int64 {
		pushes := srv.Pushes()
		out := make([]int64, 0, len(pushes))

		for _, p := range pushes {
			out = append(out, p.Issued)
		}

		return out
	}

	// At 0, 30 and 45 minutes: the token of 0 serves the first two.
	for _, after := range []int64{0, 30 * 60, 15 * 60} {
		now.Add(after)

		if err := send(); err != nil {
			t.Fatal(err)
		}
	}

	srv.Answer(token, apnstest.Answer{Status: 403, Reason: "ExpiredProviderToken", Times: 1})

	if err := send(); err == nil {
		t.Fatal("expired provider token accepted")
	}

	now.Add(60)

	if err := send(); err != nil {
		t.Fatal(err)
	}

	want := []int64{1_700_000_000, 1_700_000_000, 1_700_002_700, 1_700_002_760}
	if got := issued(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("issued %v, want %v", got, want)
	}
}

// Check proves the key against APNs without delivering: the placeholder
// token answers BadDeviceToken; a key APNs refuses fails it.
func TestCheck(t *testing.T) {
	t.Parallel()

	srv := apnstest.Start(t)
	srv.Answer(strings.Repeat("0", 64), apnstest.Answer{Status: 400, Reason: "BadDeviceToken"})

	good, err := apns.New(srv.Config("ru.komeet.app"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	if err := good.Check(t.Context()); err != nil {
		t.Fatal(err)
	}

	other := apnstest.Start(t).Config("ru.komeet.app")
	other.Endpoint = srv.URL

	wrong, err := apns.New(other, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	var refusal *provider.Error
	if err := wrong.Check(t.Context()); !errors.As(err, &refusal) || refusal.Code != "InvalidProviderToken" {
		t.Fatalf("a foreign key: %v", err)
	}

	untopical, err := apns.New(srv.Config(""), srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	if err := untopical.Check(t.Context()); !errors.Is(err, apns.ErrConfig) {
		t.Fatalf("no topic: %v", err)
	}

	if len(srv.Pushes()) != 0 {
		t.Fatalf("a check delivered %v", srv.Pushes())
	}
}

// TestMain installs the W3C propagator, as the SDK does at start.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}
