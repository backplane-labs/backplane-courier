// Package fcm is the built-in push provider: FCM HTTP v1 per channel
// (a Firebase project) with OAuth2 from a service account, and topic
// subscriptions through the Instance ID API.
package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/backplane-labs/backplane-courier/provider"
)

// Endpoints of Google.
const (
	DefaultEndpoint    = "https://fcm.googleapis.com"
	DefaultIIDEndpoint = "https://iid.googleapis.com"
	// MaxBatch is the most tokens one subscription call takes.
	MaxBatch = 1000
)

const scope = "https://www.googleapis.com/auth/firebase.messaging"

// checkBody validates credentials and project, delivering nothing.
const checkBody = `{"validate_only":true,"message":{"topic":"courier-check","notification":{"title":"check"}}}`

// ErrConfig is an invalid channel.
var ErrConfig = errors.New("fcm: invalid channel")

// Config of an FCM channel.
type Config struct {
	// Credentials: the service account key JSON (Firebase Cloud Messaging
	// API Admin).
	Credentials config.Secret `json:"credentials" schemapb:"secret=true;description=Service account key JSON"`
	// ProjectID; empty: the credentials' project_id.
	ProjectID string `json:"project_id,omitempty" schemapb:"description=Firebase project, else the credentials' project"`
	// Endpoint and IIDEndpoint: a test or a proxy may set others.
	Endpoint    string `json:"endpoint,omitempty"     schemapb:"description=FCM endpoint, else fcm.googleapis.com"`
	IIDEndpoint string `json:"iid_endpoint,omitempty" schemapb:"description=Instance ID endpoint, else iid.googleapis.com"`
}

// Project is ProjectID, else the credentials' project_id.
func (c Config) Project() string {
	if c.ProjectID != "" {
		return c.ProjectID
	}

	var key struct {
		ProjectID string `json:"project_id"`
	}

	_ = json.Unmarshal([]byte(c.Credentials), &key)

	return key.ProjectID
}

// Validate checks what the schema cannot.
func (c Config) Validate() error {
	switch {
	case c.Credentials == "":
		return fmt.Errorf("%w: credentials are required", ErrConfig)
	case c.Project() == "":
		return fmt.Errorf("%w: project_id is required (not in the credentials)", ErrConfig)
	}

	return nil
}

// Client of one channel.
type Client struct {
	send, iid string
	http      *http.Client
}

// New is the client of cfg; ts, when set, authorizes instead of the
// credentials (tests, a sidecar issuing tokens).
func New(cfg Config, ts oauth2.TokenSource) (*Client, error) {
	if ts == nil {
		creds, err := google.CredentialsFromJSON(context.Background(), []byte(cfg.Credentials), scope) //nolint:staticcheck,lll // a key the operator configured
		if err != nil {
			return nil, fmt.Errorf("fcm: credentials: %w", err)
		}

		ts = creds.TokenSource
	}

	endpoint, iid := cfg.Endpoint, cfg.IIDEndpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	if iid == "" {
		iid = DefaultIIDEndpoint
	}

	return &Client{
		send: strings.TrimSuffix(endpoint, "/") + "/v1/projects/" + cfg.Project() + "/messages:send",
		iid:  strings.TrimSuffix(iid, "/") + "/iid/v1:",
		http: oauth2.NewClient(context.Background(), oauth2.ReuseTokenSource(nil, ts)),
	}, nil
}

// Send posts an FCM v1 body and returns the message name; a refusal is a
// *provider.Error.
func (c *Client) Send(ctx context.Context, body []byte) (string, error) {
	raw, err := c.post(ctx, c.send, body, nil)
	if err != nil {
		return "", err
	}

	var ok struct {
		Name string `json:"name"`
	}

	_ = json.Unmarshal(raw, &ok)

	return ok.Name, nil
}

// Check sends a validate-only message to a topic: credentials and project
// are checked, nothing is delivered.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.post(ctx, c.send, []byte(checkBody), nil)

	return err
}

// TokenResult is one token's outcome of a subscription call: empty Error
// when it worked.
type TokenResult struct {
	Token string `json:"token"`
	Error string `json:"error,omitempty"`
}

// Subscribe adds tokens (at most MaxBatch) to topic.
func (c *Client) Subscribe(ctx context.Context, topic string, tokens []string) ([]TokenResult, error) {
	return c.batch(ctx, "batchAdd", topic, tokens)
}

// Unsubscribe removes tokens (at most MaxBatch) from topic.
func (c *Client) Unsubscribe(ctx context.Context, topic string, tokens []string) ([]TokenResult, error) {
	return c.batch(ctx, "batchRemove", topic, tokens)
}

func (c *Client) batch(ctx context.Context, method, topic string, tokens []string) ([]TokenResult, error) {
	to := "/topics/" + strings.TrimPrefix(topic, "/topics/")

	body, err := json.Marshal(map[string]any{"to": to, "registration_tokens": tokens})
	if err != nil {
		return nil, fmt.Errorf("fcm: encode: %w", err)
	}

	raw, err := c.post(ctx, c.iid+method, body, map[string]string{"access_token_auth": "true"})
	if err != nil {
		return nil, err
	}

	var res struct {
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}

	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("fcm: %s answer: %w", method, err)
	}

	out := make([]TokenResult, len(tokens))
	for i, t := range tokens {
		out[i].Token = t
		if i < len(res.Results) {
			out[i].Error = res.Results[i].Error
		}
	}

	return out, nil
}

func (c *Client) post(ctx context.Context, url string, body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, provider.Permanent("INVALID_REQUEST", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fcm: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:mnd // an answer is small

	if resp.StatusCode/100 == 2 { //nolint:mnd // 2xx
		return raw, nil
	}

	return nil, parseError(resp, raw)
}

// parseError reads FCM's error body: the FcmError code when there is one,
// else the status. Unregistered and invalid tokens, a token of another
// project, refused credentials and a malformed message are final; quota
// and outages are retried after Retry-After.
func parseError(resp *http.Response, raw []byte) *provider.Error {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Type      string `json:"@type"`
				ErrorCode string `json:"errorCode"` //nolint:tagliatelle // FCM's name
			} `json:"details"`
		} `json:"error"`
	}

	_ = json.Unmarshal(raw, &body)

	e := &provider.Error{Code: body.Error.Status, Message: body.Error.Message}

	for _, d := range body.Error.Details {
		if strings.HasSuffix(d.Type, "FcmError") && d.ErrorCode != "" {
			e.Code = d.ErrorCode
		}
	}

	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}

	if e.Code == "" {
		e.Code = strconv.Itoa(resp.StatusCode)
	}

	switch e.Code {
	case "UNREGISTERED", "SENDER_ID_MISMATCH", "INVALID_ARGUMENT",
		"THIRD_PARTY_AUTH_ERROR", "PERMISSION_DENIED", "NOT_FOUND":
		e.Permanent = true
	}

	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	return e
}
