package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/backplane-labs/backplane-courier/internal/webhook"
	"github.com/backplane-labs/backplane-courier/provider"
)

// The example of the Standard Webhooks specification.
func TestSign(t *testing.T) {
	t.Parallel()

	got := webhook.Sign("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330",
		[]byte(`{"test": 2432232314}`))
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Fatalf("signature %s, want %s", got, want)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	ok := webhook.Message{URL: "https://hooks.example.com/bot/1", Secrets: []string{"s"}, Body: json.RawMessage(`{"a":1}`)}
	if err := ok.Validate(webhook.Config{}); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		cfg  webhook.Config
		edit func(m *webhook.Message)
		want error
	}{
		"http":           {edit: func(m *webhook.Message) { m.URL = "http://hooks.example.com/" }, want: webhook.ErrDestination},
		"not allowed":    {cfg: webhook.Config{Hosts: []string{"*.bots.example.org"}}, want: webhook.ErrDestination},
		"no host":        {edit: func(m *webhook.Message) { m.URL = "https:///x" }, want: webhook.ErrURL},
		"user info":      {edit: func(m *webhook.Message) { m.URL = "https://u:p@hooks.example.com/" }, want: webhook.ErrURL},
		"no secrets":     {edit: func(m *webhook.Message) { m.Secrets = nil }, want: webhook.ErrSecret},
		"empty secret":   {edit: func(m *webhook.Message) { m.Secrets = []string{"s", ""} }, want: webhook.ErrSecret},
		"bad whsec":      {edit: func(m *webhook.Message) { m.Secrets = []string{"s", "whsec_!!"} }, want: webhook.ErrSecret},
		"too many":       {edit: func(m *webhook.Message) { m.Secrets = make([]string, 9) }, want: webhook.ErrSecret},
		"no body":        {edit: func(m *webhook.Message) { m.Body = nil }, want: webhook.ErrBody},
		"not json":       {edit: func(m *webhook.Message) { m.Body = json.RawMessage(`{`) }, want: webhook.ErrBody},
		"id":             {edit: func(m *webhook.Message) { m.ID = "a\nb" }, want: webhook.ErrID},
		"reserved":       {edit: func(m *webhook.Message) { m.Headers = map[string]string{"webhook-signature": "x"} }, want: webhook.ErrHeader},
		"header newline": {edit: func(m *webhook.Message) { m.Headers = map[string]string{"X-A": "a\r\nb"} }, want: webhook.ErrHeader},
	} {
		m := ok
		if c.edit != nil {
			c.edit(&m)
		}

		if err := m.Validate(c.cfg); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}

	insecure := ok
	insecure.URL = "http://receiver.internal/hook"

	if err := insecure.Validate(webhook.Config{Insecure: true}); err != nil {
		t.Fatalf("insecure channel: %v", err)
	}
}

// Send signs the post the receiver verifies, and classifies its answer.
func TestSend(t *testing.T) {
	t.Parallel()

	status := make(chan int, 1)

	var got *http.Request

	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))

		code := <-status
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "7")
		}

		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	const (
		current  = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
		previous = "whsec_C2FVsBQIhrscChlQIMV+b5sSYspob7oD"
	)

	client := webhook.New(webhook.Config{Insecure: true}, nil)
	m := webhook.Message{
		URL: srv.URL + "/bot", Secrets: []string{current, previous}, Body: json.RawMessage(`{"a":1}`),
		Headers: map[string]string{"X-Bot": "7"},
	}

	status <- http.StatusNoContent

	id, err := client.Send(t.Context(), m, "msg_1")
	if err != nil || id != "msg_1" {
		t.Fatal(id, err)
	}

	timestamp := got.Header.Get(webhook.HeaderTimestamp)
	signs := webhook.Sign(current, "msg_1", timestamp, body) + " " + webhook.Sign(previous, "msg_1", timestamp, body)

	if got.Header.Get(webhook.HeaderSignature) != signs || got.Header.Get(webhook.HeaderID) != "msg_1" ||
		got.Header.Get("Content-Type") != "application/json" || got.Header.Get("X-Bot") != "7" || string(body) != `{"a":1}` {
		t.Fatalf("request %v %s", got.Header, body)
	}

	// A receiver of the reference library verifies with either secret, not
	// with a third.
	for secret, want := range map[string]bool{current: true, previous: true, "whsec_dGhpcmQtc2VjcmV0LWtleQ==": false} {
		receiver, err := standardwebhooks.NewWebhook(secret)
		if err != nil {
			t.Fatal(err)
		}

		if err := receiver.Verify(body, got.Header); (err == nil) != want {
			t.Errorf("verify with %s: %v, want ok %v", secret[:12], err, want)
		}
	}

	for code, want := range map[int]struct {
		code      string
		permanent bool
		after     time.Duration
	}{
		http.StatusGone:                {code: webhook.CodeGone, permanent: true},
		http.StatusBadRequest:          {code: webhook.CodeRefused, permanent: true},
		http.StatusTooManyRequests:     {code: "unavailable", after: 7 * time.Second},
		http.StatusInternalServerError: {},
	} {
		status <- code

		_, err := client.Send(t.Context(), m, "msg_1")

		var perr *provider.Error

		switch {
		case err == nil:
			t.Errorf("%d: no error", code)
		case want.code == "" && errors.As(err, &perr):
			t.Errorf("%d: %v, want a retried error", code, err)
		case want.code != "" && (!errors.As(err, &perr) || perr.Code != want.code || perr.Permanent != want.permanent ||
			perr.RetryAfter != want.after):
			t.Errorf("%d: %v, want %+v", code, err, want)
		}
	}
}

// A secure channel dials only public addresses: a receiver inside is
// refused for good, nothing posted.
func TestSecureRefusesInternal(t *testing.T) {
	t.Parallel()

	hit := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(srv.Close)

	m := webhook.Message{URL: srv.URL, Secrets: []string{"s"}, Body: json.RawMessage(`{}`)}

	_, err := webhook.New(webhook.Config{}, nil).Send(context.Background(), m, "msg_1")

	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != webhook.CodeForbidden || hit {
		t.Fatalf("internal receiver: %v (hit %v)", err, hit)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

// A delivery carries the caller's trace to the receiver.
func TestSendCarriesTrace(t *testing.T) {
	t.Parallel()

	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(t.Context(), "caller")
	t.Cleanup(func() { span.End() })

	m := webhook.Message{URL: srv.URL, Secrets: []string{"whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"}, Body: json.RawMessage(`{}`)}
	if _, err := webhook.New(webhook.Config{Insecure: true}, nil).Send(ctx, m, "msg_t"); err != nil {
		t.Fatal(err)
	}

	if tp := <-got; !strings.Contains(tp, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q", tp)
	}
}

// TestMain installs the W3C propagator, as the SDK does at start.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}
