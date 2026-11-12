// Package apnstest is a fake APNs for tests: HTTP/2 over TLS, the
// provider token checked against the channel's key, every push recorded,
// answers by rules on the device token.
package apnstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/golang-jwt/jwt/v5"

	"github.com/backplane-labs/backplane-courier/internal/apns"
)

// Identity of the fake's key.
const (
	KeyID  = "KEY0000001"
	TeamID = "TEAM000001"
)

// Refusals of a provider token.
var (
	errNoBearer = errors.New("apnstest: no bearer token")
	errKeyID    = errors.New("apnstest: another key id")
)

// Push is one push the fake took.
type Push struct {
	Token   string
	Headers http.Header
	Payload json.RawMessage
	// Issued is the provider token's iat.
	Issued int64
}

// Answer is how the fake responds to a device token.
type Answer struct {
	Status int
	Reason string
	// Times: answer this many times, then accept; 0: always.
	Times int
}

// Server is the fake.
type Server struct {
	*httptest.Server

	key *ecdsa.PrivateKey
	pem string

	mu      sync.Mutex
	pushes  []Push
	answers map[string]*Answer
}

// Start runs a fake for the test with a fresh key.
func Start(t *testing.T) *Server {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{
		key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		answers: map[string]*Answer{},
	}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.EnableHTTP2 = true
	s.StartTLS()
	t.Cleanup(s.Close)

	return s
}

// Config is a channel of the fake with topic.
func (s *Server) Config(topic string) apns.Config {
	return apns.Config{KeyID: KeyID, TeamID: TeamID, Key: config.Secret(s.pem), Topic: topic, Endpoint: s.URL}
}

// Answer sets how token is answered.
func (s *Server) Answer(token string, a Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.answers[token] = &a
}

// Pushes are the pushes the fake accepted.
func (s *Server) Pushes() []Push {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Push(nil), s.pushes...)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, "/3/device/")
	if r.Method != http.MethodPost || !ok || r.ProtoMajor != 2 {
		refuse(w, http.StatusNotFound, "BadPath")

		return
	}

	issued, err := s.verify(r.Header.Get("Authorization"))
	if err != nil {
		refuse(w, http.StatusForbidden, "InvalidProviderToken")

		return
	}

	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	answer := s.answers[token]

	if answer != nil {
		if answer.Times > 0 {
			answer.Times--
			if answer.Times == 0 {
				delete(s.answers, token)
			}
		}

		s.mu.Unlock()
		refuse(w, answer.Status, answer.Reason)

		return
	}

	s.pushes = append(s.pushes, Push{Token: token, Headers: r.Header.Clone(), Payload: body, Issued: issued})
	s.mu.Unlock()

	id := r.Header.Get("Apns-Id")
	if id == "" {
		id = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(s.Pushes()))
	}

	w.Header().Set("Apns-Id", id)
}

// verify checks the provider token as APNs does: ES256 by the key, kid and
// iss of the channel; it returns iat.
func (s *Server) verify(header string) (int64, error) {
	raw, ok := strings.CutPrefix(header, "bearer ")
	if !ok {
		return 0, errNoBearer
	}

	claims := jwt.MapClaims{}

	t, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Header["kid"] != KeyID {
			return nil, errKeyID
		}

		return &s.key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(TeamID), jwt.WithIssuedAt())
	if err != nil || !t.Valid {
		return 0, fmt.Errorf("provider token: %w", err)
	}

	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return 0, fmt.Errorf("iat: %w", err)
	}

	return iat.Unix(), nil
}

func refuse(w http.ResponseWriter, status int, reason string) {
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
