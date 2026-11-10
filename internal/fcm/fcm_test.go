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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/backplane-labs/backplane/pkg/backplane/config"

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
