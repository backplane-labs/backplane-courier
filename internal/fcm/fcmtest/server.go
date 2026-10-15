// Package fcmtest is a fake FCM endpoint for tests (HTTP v1 sends and
// Instance ID subscriptions): it records what it receives and answers by
// rules on the target.
package fcmtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// Answer is how the server responds to a token or topic.
type Answer struct {
	Status     int
	Code       string
	Message    string
	RetryAfter string
	// Times: answer this many times, then accept; 0: always.
	Times int
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	received []map[string]any
	auth     []string
	answers  map[string]*Answer
	topics   map[string]map[string]bool
}

// Start runs a fake for the test.
func Start(t *testing.T) *Server {
	t.Helper()

	s := &Server{answers: map[string]*Answer{}, topics: map[string]map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)

	return s
}

// Answer sets how target (a token, or "topic:<name>") is answered.
func (s *Server) Answer(target string, a Answer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.answers[target] = &a
}

// Received are the accepted messages' "message" objects.
func (s *Server) Received() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]map[string]any(nil), s.received...)
}

// Auth are the Authorization headers seen.
func (s *Server) Auth() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.auth...)
}

// TokenSource is a static token for the fake.
func TokenSource() oauth2.TokenSource { //nolint:ireturn // what accounts takes
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"})
}

// Subscribed are the tokens subscribed to topic.
func (s *Server) Subscribed(topic string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.topics[topic]))
	for t := range s.topics[topic] {
		out = append(out, t)
	}

	return out
}

// subscriptions answers the Instance ID batch calls: a token with an
// Answer set is refused (INVALID_ARGUMENT), the rest change the topic.
func (s *Server) subscriptions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To     string   `json:"to"`
		Tokens []string `json:"registration_tokens"`
	}

	auth := r.Header.Get("access_token_auth")
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || auth != "true" {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	topic := strings.TrimPrefix(body.To, "/topics/")
	add := strings.HasSuffix(r.URL.Path, ":batchAdd")

	s.mu.Lock()
	if s.topics[topic] == nil {
		s.topics[topic] = map[string]bool{}
	}

	results := make([]map[string]string, len(body.Tokens))
	for i, t := range body.Tokens {
		results[i] = map[string]string{}

		switch {
		case s.answers[t] != nil:
			results[i]["error"] = "INVALID_ARGUMENT"
		case add:
			s.topics[topic][t] = true
		default:
			delete(s.topics[topic], t)
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(map[string][]map[string]string{"results": results}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/iid/v1:") {
		s.subscriptions(w, r)

		return
	}

	if !strings.HasSuffix(r.URL.Path, "/messages:send") || r.Method != http.MethodPost {
		http.NotFound(w, r)

		return
	}

	raw, _ := io.ReadAll(r.Body)

	var body struct {
		ValidateOnly bool           `json:"validate_only"`
		Message      map[string]any `json:"message"`
	}

	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	key, _ := body.Message["token"].(string)
	if topic, ok := body.Message["topic"].(string); ok {
		key = "topic:" + topic
	}

	s.mu.Lock()
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	a := s.answers[key]

	if a != nil && a.Times > 0 {
		a.Times--
		if a.Times == 0 {
			delete(s.answers, key)
		}
	}

	if a == nil && !body.ValidateOnly {
		s.received = append(s.received, body.Message)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if a == nil {
		_, _ = fmt.Fprintf(w, `{"name":"projects/test/messages/%d"}`, len(s.Received()))

		return
	}

	if a.RetryAfter != "" {
		w.Header().Set("Retry-After", a.RetryAfter)
	}

	w.WriteHeader(a.Status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"status":"x","details":[{"@type":%q,"errorCode":%q}]}}`,
		a.Status, a.Message, "type.googleapis.com/google.firebase.fcm.v1.FcmError", a.Code)
}
