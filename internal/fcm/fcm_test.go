package fcm_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/backplane-labs/backplane-courier/internal/fcm"
	"github.com/backplane-labs/backplane-courier/provider"
)

// serviceAccount is a service account key whose token_uri is tokenURI.
func serviceAccount(t *testing.T, tokenURI string) config.Secret {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "stand", "client_email": "courier@stand.iam.gserviceaccount.com",
		"private_key_id": "k1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri": tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}

	return config.Secret(raw)
}

// A channel runs entirely against a stand: the key's token_uri issues the
// token and endpoint receives the send; the refusal of an unregistered
// token is final.
func TestStandEndpoints(t *testing.T) {
	t.Parallel()

	var tokens, sends atomic.Int32

	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			tokens.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"stand-token","token_type":"Bearer","expires_in":3600}`))
		case r.URL.Path == "/v1/projects/stand/messages:send" && r.Header.Get("Authorization") == "Bearer stand-token":
			sends.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found.",` +
				`"details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stand.Close)

	c, err := fcm.New(fcm.Config{Credentials: serviceAccount(t, stand.URL+"/token"), Endpoint: stand.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Send(t.Context(), []byte(`{"message":{"token":"gone-device"}}`))

	var refused *provider.Error
	if !errors.As(err, &refused) || refused.Code != "UNREGISTERED" || !refused.Permanent {
		t.Fatalf("send: %v", err)
	}

	if tokens.Load() != 1 || sends.Load() != 1 {
		t.Fatalf("token requests %d, sends %d", tokens.Load(), sends.Load())
	}

	if strings.Contains(err.Error(), "stand-token") {
		t.Fatal("the error carries the token")
	}
}

// A send carries the caller's trace to FCM.
func TestSendCarriesTrace(t *testing.T) {
	t.Parallel()

	got := make(chan string, 1)
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"stand-token","token_type":"Bearer","expires_in":3600}`))

			return
		}

		got <- r.Header.Get("Traceparent")
		_, _ = w.Write([]byte(`{"name":"projects/stand/messages/1"}`))
	}))
	t.Cleanup(stand.Close)

	c, err := fcm.New(fcm.Config{Credentials: serviceAccount(t, stand.URL+"/token"), Endpoint: stand.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, span := sdktrace.NewTracerProvider().Tracer("test").Start(t.Context(), "caller")
	t.Cleanup(func() { span.End() })

	if _, err := c.Send(ctx, []byte(`{"message":{"token":"device"}}`)); err != nil {
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
