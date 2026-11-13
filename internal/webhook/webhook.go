// Package webhook is the built-in webhook provider: a signed HTTP POST of
// JSON to a URL the product names on every call, signed per Standard
// Webhooks (webhook-id, webhook-timestamp, webhook-signature: HMAC-SHA256
// with the secret the product holds for that receiver). Retries are the
// platform's (the activity's); the receiver deduplicates by webhook-id,
// the same on every attempt.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/netguard"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/backplane-labs/backplane-courier/provider"
)

// Limits of a message.
const (
	MaxBodyBytes = 1 << 20
	maxURL       = 2048
	maxID        = 256
	maxHeaders   = 32
	maxSecrets   = 8
	maxAnswer    = 4096
)

// Codes of the refusals.
const (
	// CodeForbidden: a URL courier does not post to — not https, a host the
	// channel does not allow, or an internal address.
	CodeForbidden = "forbidden_destination"
	// CodeGone: the receiver answered 410, it takes no more.
	CodeGone = "gone"
	// CodeRefused: the receiver answered another 3xx or 4xx.
	CodeRefused = "refused"
)

// Signature headers (Standard Webhooks: webhook-id, webhook-timestamp,
// webhook-signature), canonical.
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"
)

// secretPrefix marks a base64 secret (Standard Webhooks "whsec_").
const secretPrefix = "whsec_"

// Errors of Validate; every one is final.
var (
	ErrURL         = errors.New("webhook: url is an https URL")
	ErrDestination = errors.New("webhook: url is not a host this channel allows")
	ErrSecret      = errors.New("webhook: secrets are required (each raw, or whsec_ and base64)")
	ErrBody        = errors.New("webhook: body is a JSON value")
	ErrTooBig      = errors.New("webhook: body exceeds 1 MiB")
	ErrID          = errors.New("webhook: id is up to 256 printable characters")
	ErrHeader      = errors.New("webhook: header")
)

// reserved headers are courier's: the request's own and the signature.
//
//nolint:gochecknoglobals // a constant set
var reserved = map[string]bool{
	"Host": true, "Content-Length": true, "Content-Type": true, "Transfer-Encoding": true, "Connection": true,
	"Webhook-Id": true, "Webhook-Timestamp": true, "Webhook-Signature": true,
}

// Config of a webhook channel.
type Config struct {
	// Hosts the channel posts to (exact or *.suffix); empty: any public
	// host.
	Hosts []string `json:"hosts,omitempty" schemapb:"description=Hosts webhooks may go to, else any public host"`
	// Insecure lets the channel post over http and to internal addresses:
	// a receiver in the installation's own network, or development.
	Insecure bool `json:"insecure,omitempty" schemapb:"description=Allow http and internal addresses"`
}

// Message is courier.SendWebhook's input.
type Message struct {
	Channel string `json:"channel,omitempty" schemapb:"description=Webhook channel of the installation, else the default"`
	URL     string `json:"url"               schemapb:"description=Receiver URL"`
	// Secrets sign the message, each raw or whsec_ and base64: every one
	// adds its signature to webhook-signature, in order, so a receiver
	// that knows any of them verifies it. A product rotating a receiver's
	// secret signs with the new one and, while the receiver may still hold
	// it, the old one.
	Secrets []string `json:"secrets" schemapb:"secret=true;description=Signing secrets: the current one, then older ones"`
	// ID is webhook-id, the receiver deduplicates by it; empty: the
	// step's, the same on every attempt.
	ID      string            `json:"id,omitempty"      schemapb:"description=webhook-id, else the step's"`
	Body    json.RawMessage   `json:"body"              schemapb:"description=JSON posted as the body"`
	Headers map[string]string `json:"headers,omitempty" schemapb:"description=Extra request headers"`
}

// Validate m for a channel of cfg.
func (m Message) Validate(cfg Config) error {
	if err := m.validateURL(cfg); err != nil {
		return err
	}

	if err := m.validateSecrets(); err != nil {
		return err
	}

	body := strings.TrimSpace(string(m.Body))

	switch {
	case body == "" || !json.Valid(m.Body):
		return ErrBody
	case len(m.Body) > MaxBodyBytes:
		return fmt.Errorf("%w (%d)", ErrTooBig, len(m.Body))
	case len(m.ID) > maxID || strings.ContainsFunc(m.ID, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return ErrID
	}

	return m.validateHeaders()
}

func (m Message) validateURL(cfg Config) error {
	if len(m.URL) > maxURL {
		return fmt.Errorf("%w: too long", ErrURL)
	}

	target, err := url.Parse(m.URL)
	if err != nil || target.Host == "" || target.User != nil || (target.Scheme != "https" && target.Scheme != "http") {
		return ErrURL
	}

	if target.Scheme == "http" && !cfg.Insecure {
		return fmt.Errorf("%w: %w: not https", ErrURL, ErrDestination)
	}

	if len(cfg.Hosts) > 0 && !netguard.Allowed(target.Hostname(), cfg.Hosts) {
		return fmt.Errorf("%w: %s", ErrDestination, target.Hostname())
	}

	return nil
}

func (m Message) validateSecrets() error {
	if len(m.Secrets) == 0 || len(m.Secrets) > maxSecrets {
		return fmt.Errorf("%w: 1 to %d", ErrSecret, maxSecrets)
	}

	for i, secret := range m.Secrets {
		switch {
		case secret == "":
			return fmt.Errorf("%w: secret %d is empty", ErrSecret, i+1)
		case strings.HasPrefix(secret, secretPrefix) && decodeSecret(secret) == nil:
			return fmt.Errorf("%w: whsec_ secret %d is not base64", ErrSecret, i+1)
		}
	}

	return nil
}

func (m Message) validateHeaders() error {
	if len(m.Headers) > maxHeaders {
		return fmt.Errorf("%w: at most %d", ErrHeader, maxHeaders)
	}

	for name, value := range m.Headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)

		switch {
		case name == "" || strings.ContainsAny(name, " \t\r\n:") || strings.ContainsAny(value, "\r\n"):
			return fmt.Errorf("%w %q: not a header", ErrHeader, name)
		case reserved[canonical]:
			return fmt.Errorf("%w %q: set by courier", ErrHeader, name)
		}
	}

	return nil
}

// Target names the message in logs and audit: the URL's host.
func (m Message) Target() string {
	if u, err := url.Parse(m.URL); err == nil {
		return u.Host
	}

	return ""
}

// Client posts the webhooks of one channel.
type Client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time
}

// New is the client of a channel. A nil client: one that does not follow
// redirects and, unless the channel is insecure, dials only public
// addresses, directly (netguard.ErrForbiddenAddress); every request is a
// client span carrying the caller's trace.
func New(cfg Config, client *http.Client) *Client {
	switch {
	case client != nil:
	case cfg.Insecure:
		transport, _ := http.DefaultTransport.(*http.Transport)
		client = &http.Client{
			Transport:     otelhttp.NewTransport(transport.Clone()),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	default:
		client = netguard.Client()
	}

	return &Client{cfg: cfg, http: client, now: time.Now}
}

// Config of the channel.
func (c *Client) Config() Config { return c.cfg }

// Check: nothing to try before a receiver is named.
func (c *Client) Check(context.Context) error { return nil }

// Send signs and posts m (validated) with webhook-id id and returns it,
// or a *provider.Error: final for a refusal, later for a rate limit or an
// unavailable receiver that says when to come back; any other failure is
// retried.
func (c *Client) Send(ctx context.Context, m Message, id string) (string, error) {
	timestamp := strconv.FormatInt(c.now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(m.Body))
	if err != nil {
		return "", provider.Permanent(CodeForbidden, err.Error())
	}

	for name, value := range m.Headers {
		req.Header.Set(name, value)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "backplane-courier")
	req.Header.Set(HeaderID, id)
	req.Header.Set(HeaderTimestamp, timestamp)

	signatures := make([]string, 0, len(m.Secrets))
	for _, secret := range m.Secrets {
		signatures = append(signatures, Sign(secret, id, timestamp, m.Body))
	}

	req.Header.Set(HeaderSignature, strings.Join(signatures, " "))

	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, netguard.ErrForbiddenAddress) {
			return "", provider.Permanent(CodeForbidden, err.Error())
		}

		return "", fmt.Errorf("webhook: %w", err)
	}
	defer res.Body.Close()

	answer, _ := io.ReadAll(io.LimitReader(res.Body, maxAnswer))

	return id, refusal(res, strings.TrimSpace(string(answer)))
}

// Sign is the webhook-signature of a message: "v1," and the base64
// HMAC-SHA256 of "<id>.<timestamp>.<body>" with the secret.
func Sign(secret, id, timestamp string, body []byte) string {
	key := []byte(secret)
	if decoded := decodeSecret(secret); decoded != nil {
		key = decoded
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)

	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// decodeSecret is a whsec_ secret's key; nil for a raw or bad one.
func decodeSecret(secret string) []byte {
	encoded, ok := strings.CutPrefix(secret, secretPrefix)
	if !ok {
		return nil
	}

	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) == 0 {
		return nil
	}

	return key
}

// refusal classifies the receiver's answer.
func refusal(res *http.Response, body string) error {
	text := "receiver answered " + res.Status
	if body != "" {
		text += ": " + body
	}

	switch code := res.StatusCode; {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusGone:
		return provider.Permanent(CodeGone, text)
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		if after, ok := retryAfter(res.Header.Get("Retry-After")); ok {
			return provider.Later("unavailable", text, after)
		}

		return fmt.Errorf("webhook: %s", text) //nolint:err113 // the receiver's own answer
	case code == http.StatusRequestTimeout || code >= 500:
		return fmt.Errorf("webhook: %s", text) //nolint:err113 // the receiver's own answer
	default:
		return provider.Permanent(CodeRefused, text)
	}
}

// retryAfter reads Retry-After: seconds or a date.
func retryAfter(v string) (time.Duration, bool) {
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second, true
	}

	if t, err := http.ParseTime(v); err == nil && time.Until(t) > 0 {
		return time.Until(t), true
	}

	return 0, false
}
