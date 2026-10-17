package webpush_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/golang-jwt/jwt/v5"

	"github.com/backplane-labs/backplane-courier/internal/webpush"
	"github.com/backplane-labs/backplane-courier/provider"
)

var b64 = base64.RawURLEncoding

// browser is a subscriber: its key pair and auth secret.
type browser struct {
	key  *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T) browser {
	t.Helper()

	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	auth := make([]byte, 16)
	_, _ = rand.Read(auth)

	return browser{key: key, auth: auth}
}

func (b browser) subscription(endpoint string) webpush.Subscription {
	return webpush.Subscription{Endpoint: endpoint, Keys: webpush.Keys{
		P256dh: b64.EncodeToString(b.key.PublicKey().Bytes()), Auth: b64.EncodeToString(b.auth),
	}}
}

// decrypt is RFC 8291's receiving side of one aes128gcm record.
func (b browser) decrypt(t *testing.T, body []byte) string {
	t.Helper()

	salt, rs, idLen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	serverPub, record := body[21:21+idLen], body[21+idLen:]

	if int(rs) < len(record) {
		t.Fatalf("record %d longer than rs %d", len(record), rs)
	}

	as, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatal(err)
	}

	shared, err := b.key.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}

	info := append(append([]byte("WebPush: info\x00"), b.key.PublicKey().Bytes()...), serverPub...)

	ikm, err := hkdf.Key(sha256.New, shared, b.auth, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}

	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)

	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)

	plain, err := gcm.Open(nil, nonce, record, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	end := bytes.LastIndexByte(plain, 2)
	if end < 0 || strings.Trim(string(plain[end+1:]), "\x00") != "" {
		t.Fatal("no last-record delimiter")
	}

	return string(plain[:end])
}

func channel(t *testing.T) webpush.Config {
	t.Helper()

	public, private, err := webpush.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}

	return webpush.Config{Subject: "mailto:ops@example.com", PublicKey: public, PrivateKey: config.Secret(private)}
}

func TestConfig(t *testing.T) {
	t.Parallel()

	good := channel(t)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}

	other := channel(t)

	for name, c := range map[string]webpush.Config{
		"subject":    {Subject: "ops@example.com", PublicKey: good.PublicKey, PrivateKey: good.PrivateKey},
		"no keys":    {Subject: good.Subject},
		"mismatched": {Subject: good.Subject, PublicKey: other.PublicKey, PrivateKey: good.PrivateKey},
		"garbage":    {Subject: good.Subject, PublicKey: good.PublicKey, PrivateKey: "!!"},
	} {
		if err := c.Validate(); !errors.Is(err, webpush.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMessageValidate(t *testing.T) {
	t.Parallel()

	sub := newBrowser(t).subscription("https://updates.push.services.mozilla.com/wpush/v2/abc")
	ok := webpush.Message{Subscription: sub, Payload: []byte(`{"title":"hi"}`)}

	if err := ok.Validate(nil); err != nil {
		t.Fatal(err)
	}

	ttl, long := int64(-1), strings.Repeat("a", webpush.MaxPayloadBytes)

	cases := map[string]struct {
		edit  func(m *webpush.Message)
		hosts []string
		want  error
	}{
		"http":          {edit: func(m *webpush.Message) { m.Subscription.Endpoint = "http://fcm.googleapis.com/x" }, want: webpush.ErrSubscription},
		"internal host": {edit: func(m *webpush.Message) { m.Subscription.Endpoint = "https://billing.internal/x" }, want: webpush.ErrEndpoint},
		"ip":            {edit: func(m *webpush.Message) { m.Subscription.Endpoint = "https://10.0.0.1/x" }, want: webpush.ErrEndpoint},
		"lookalike":     {edit: func(m *webpush.Message) { m.Subscription.Endpoint = "https://evilgoogleapis.com/x" }, want: webpush.ErrEndpoint},
		"own host":      {edit: func(m *webpush.Message) { m.Subscription.Endpoint = "https://push.example.com/x" }, hosts: []string{"push.example.com"}},
		"bad p256dh":    {edit: func(m *webpush.Message) { m.Subscription.Keys.P256dh = b64.EncodeToString([]byte("short")) }, want: webpush.ErrSubscription},
		"bad auth":      {edit: func(m *webpush.Message) { m.Subscription.Keys.Auth = "AAAA" }, want: webpush.ErrSubscription},
		"no payload":    {edit: func(m *webpush.Message) { m.Payload = nil }, want: webpush.ErrPayload},
		"not json":      {edit: func(m *webpush.Message) { m.Payload = []byte("hi") }, want: webpush.ErrPayload},
		"too big":       {edit: func(m *webpush.Message) { m.Payload = []byte(`"` + long + `"`) }, want: webpush.ErrTooBig},
		"ttl":           {edit: func(m *webpush.Message) { m.TTLSeconds = &ttl }, want: webpush.ErrTTL},
		"urgency":       {edit: func(m *webpush.Message) { m.Urgency = "urgent" }, want: webpush.ErrUrgency},
		"topic":         {edit: func(m *webpush.Message) { m.Topic = "has space" }, want: webpush.ErrTopic},
	}

	for name, c := range cases {
		m := ok
		c.edit(&m)

		if err := m.Validate(c.hosts); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestSend(t *testing.T) {
	t.Parallel()

	cfg := channel(t)
	b := newBrowser(t)

	var got struct {
		header http.Header
		body   []byte
	}

	status := http.StatusCreated

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.header = r.Header.Clone()
		got.body, _ = io.ReadAll(r.Body)

		switch status {
		case http.StatusCreated:
			w.Header().Set("Location", "https://push.example/m/1")
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "9")
		}

		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	client := webpush.New(cfg, srv.Client())
	ttl := int64(60)
	msg := webpush.Message{
		Subscription: b.subscription(srv.URL + "/push/abc"), Payload: []byte(`{"title":"Привет"}`),
		TTLSeconds: &ttl, Urgency: "high", Topic: "chat-1",
	}

	id, err := client.Send(context.Background(), msg)
	if err != nil || id != "https://push.example/m/1" {
		t.Fatalf("send: %q %v", id, err)
	}

	if plain := b.decrypt(t, got.body); plain != `{"title":"Привет"}` {
		t.Fatalf("payload = %q", plain)
	}

	h := got.header
	if h.Get("Content-Encoding") != "aes128gcm" || h.Get("Ttl") != "60" || h.Get("Urgency") != "high" || h.Get("Topic") != "chat-1" {
		t.Fatalf("headers: %v", h)
	}

	// VAPID: a JWT for the push service's origin, signed by the channel's key.
	auth := h.Get("Authorization")
	token, key, _ := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ", k=")

	if key != cfg.PublicKey {
		t.Fatalf("k = %q", key)
	}

	pub, _ := b64.DecodeString(cfg.PublicKey)

	parsed, err := jwt.Parse(token, func(*jwt.Token) (any, error) {
		return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience(srv.URL))
	if err != nil {
		t.Fatalf("vapid jwt: %v", err)
	}

	if sub, _ := parsed.Claims.GetSubject(); sub != "mailto:ops@example.com" {
		t.Fatalf("sub = %q", sub)
	}

	for _, c := range []struct {
		status    int
		code      string
		permanent bool
		after     time.Duration
	}{
		{http.StatusGone, "gone", true, 0},
		{http.StatusNotFound, "gone", true, 0},
		{http.StatusForbidden, "unauthorized", true, 0},
		{http.StatusRequestEntityTooLarge, "too_big", true, 0},
		{http.StatusBadRequest, "refused", true, 0},
		{http.StatusTooManyRequests, "rate_limited", false, 9 * time.Second},
	} {
		status = c.status

		_, err := client.Send(context.Background(), msg)

		var pe *provider.Error
		if !errors.As(err, &pe) || pe.Code != c.code || pe.Permanent != c.permanent || pe.RetryAfter != c.after {
			t.Errorf("%d: %v", c.status, err)
		}
	}

	status = http.StatusBadGateway

	_, err = client.Send(context.Background(), msg)

	var pe *provider.Error
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("502 is retried by the caller: %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	t.Parallel()

	hit := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(target.Close)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	// The default client, with the test server's roots.
	client := webpush.New(channel(t), nil)
	webpush.UseTransport(client, srv.Client().Transport)

	_, err := client.Send(context.Background(), webpush.Message{
		Subscription: newBrowser(t).subscription(srv.URL), Payload: []byte(`{}`),
	})
	if err == nil || hit {
		t.Fatalf("followed a redirect: %v", err)
	}
}
