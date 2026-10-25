// Package webpush is the built-in Web Push provider: the standard protocol
// every browser's push service speaks (RFC 8030), the payload encrypted
// to the subscription (RFC 8291) and the sender identified by its VAPID
// key (RFC 8292). No vendor account: a channel is a VAPID key pair and a
// contact; the subscription a browser hands out names its push service.
package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	wp "github.com/SherClockHolmes/webpush-go"
	"github.com/backplane-labs/backplane/pkg/backplane/config"

	"github.com/backplane-labs/backplane-courier/internal/netguard"
	"github.com/backplane-labs/backplane-courier/provider"
)

// Limits of the protocol.
const (
	// MaxPayloadBytes is what one 4096-byte aes128gcm record holds: the
	// header (86), the tag (16) and the padding delimiter (1) take the rest.
	MaxPayloadBytes = int(wp.MaxRecordSize) - 86 - 16 - 1
	// MaxTTLSeconds: push services keep a message four weeks at most.
	MaxTTLSeconds = 28 * 24 * 3600
	// DefaultTTLSeconds is how long a message waits for an offline browser
	// when it names no ttl.
	DefaultTTLSeconds = 24 * 3600
	maxTopic          = 32
	maxEndpoint       = 2048
	maxAnswer         = 4096
)

// DefaultHosts are the push services of the browsers: Chrome and Edge
// (Google), Firefox (Mozilla), Safari (Apple), legacy Edge (Windows).
//
//nolint:gochecknoglobals // a constant list
var DefaultHosts = []string{
	"fcm.googleapis.com", "*.googleapis.com", "updates.push.services.mozilla.com", "*.push.services.mozilla.com",
	"*.push.apple.com", "*.notify.windows.com",
}

// CodeForbidden is the refusal's code for an endpoint courier does not post
// to: not https, a host the channel does not allow, or an internal address.
const CodeForbidden = "forbidden_destination"

// Errors of the configuration and of Validate; every one is final.
var (
	ErrConfig       = errors.New("webpush: invalid channel")
	ErrSubscription = errors.New("webpush: invalid subscription")
	ErrEndpoint     = errors.New("webpush: endpoint is not a push service this channel allows")
	ErrPayload      = errors.New("webpush: payload is required")
	ErrTooBig       = errors.New("webpush: payload exceeds 3993 bytes")
	ErrTTL          = errors.New("webpush: ttl_seconds is 0 to 2419200 (28 days)")
	ErrUrgency      = errors.New("webpush: urgency is very-low, low, normal or high")
	ErrTopic        = errors.New("webpush: topic is up to 32 characters of A-Z a-z 0-9 - _")
)

var topicPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Config of a Web Push channel.
type Config struct {
	// Subject: a mailto: address or an https URL push services can reach
	// the sender at.
	Subject string `json:"subject" schemapb:"description=mailto: address or https URL of the sender"`
	// PublicKey: the VAPID public key, base64url of the uncompressed P-256
	// point; browsers subscribe with it (applicationServerKey).
	PublicKey string `json:"public_key" schemapb:"description=VAPID public key (base64url) browsers subscribe with"`
	// PrivateKey: the VAPID private key, base64url of the P-256 scalar.
	PrivateKey config.Secret `json:"private_key" schemapb:"secret=true;description=VAPID private key (base64url)"`
	// Hosts the endpoints may point at: exact names or *.suffix; empty:
	// DefaultHosts. Subscriptions come from browsers, so an endpoint is
	// untrusted input: courier posts only to push services listed here,
	// over https.
	Hosts []string `json:"hosts,omitempty" schemapb:"description=Allowed push service hosts, else the browsers own"`
}

// Validate checks the key pair and the subject.
func (c Config) Validate() error {
	switch {
	case !strings.HasPrefix(c.Subject, "mailto:") && !strings.HasPrefix(c.Subject, "https://"):
		return fmt.Errorf("%w: subject is a mailto: address or an https URL", ErrConfig)
	case c.PublicKey == "" || c.PrivateKey == "":
		return fmt.Errorf("%w: public_key and private_key are required (generate a pair in the console)", ErrConfig)
	}

	return checkPair(c.PublicKey, c.PrivateKey.Reveal())
}

// checkPair: the private key is a P-256 scalar whose public key is pub.
func checkPair(pub, priv string) error {
	d, err := decode(priv)
	if err != nil {
		return fmt.Errorf("%w: private_key: %w", ErrConfig, err)
	}

	key, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return fmt.Errorf("%w: private_key: %w", ErrConfig, err)
	}

	p, err := decode(pub)
	if err != nil {
		return fmt.Errorf("%w: public_key: %w", ErrConfig, err)
	}

	if !bytes.Equal(key.PublicKey().Bytes(), p) {
		return fmt.Errorf("%w: public_key is not the private key's", ErrConfig)
	}

	return nil
}

// decode reads base64url with or without padding (and standard base64).
func decode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}

	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base64url: %w", err)
	}

	return b, nil
}

// GenerateKeys is a fresh VAPID pair.
func GenerateKeys() (string, string, error) {
	private, public, err := wp.GenerateVAPIDKeys()
	if err != nil {
		return "", "", fmt.Errorf("webpush: generate keys: %w", err)
	}

	return public, private, nil
}

// Subscription is what PushManager.subscribe() gives the browser, as its
// toJSON() writes it.
type Subscription struct {
	Endpoint string `json:"endpoint" schemapb:"description=The push service URL of the subscription"`
	Keys     Keys   `json:"keys"`
}

// Keys of a subscription.
type Keys struct {
	P256dh string `json:"p256dh" schemapb:"description=The browser's P-256 public key (base64url)"`
	Auth   string `json:"auth"   schemapb:"description=The authentication secret (base64url)"`
}

// Message is one Web Push to one subscription.
type Message struct {
	Channel string `json:"channel,omitempty" schemapb:"description=Web Push channel of the installation, else the default"`

	Subscription Subscription `json:"subscription"`
	// Payload is what the service worker's push event gets as
	// event.data.json(): any JSON — the app's own shape, or the
	// declarative {"web_push": 8030, "notification": {...}} Safari shows
	// without a service worker.
	Payload json.RawMessage `json:"payload" schemapb:"description=JSON the service worker gets as event.data.json()"`

	TTLSeconds *int64 `json:"ttl_seconds,omitempty" schemapb:"description=Seconds kept for an offline browser, else 86400"`
	Urgency    string `json:"urgency,omitempty"     schemapb:"description=very-low, low, normal or high"`
	Topic      string `json:"topic,omitempty"       schemapb:"description=A newer undelivered one replaces it"`
}

// Validate checks what a push service would refuse, and the endpoint
// against hosts (empty: DefaultHosts).
func (m Message) Validate(hosts []string) error {
	if err := m.Subscription.validate(hosts); err != nil {
		return err
	}

	if err := m.validatePayload(); err != nil {
		return err
	}

	switch {
	case m.TTLSeconds != nil && (*m.TTLSeconds < 0 || *m.TTLSeconds > MaxTTLSeconds):
		return ErrTTL
	case m.Urgency != "" && m.Urgency != "very-low" && m.Urgency != "low" && m.Urgency != "normal" && m.Urgency != "high":
		return ErrUrgency
	case m.Topic != "" && !topicPattern.MatchString(m.Topic):
		return ErrTopic
	}

	return nil
}

func (m Message) validatePayload() error {
	payload := strings.TrimSpace(string(m.Payload))

	switch {
	case payload == "" || payload == "null":
		return ErrPayload
	case !json.Valid(m.Payload):
		return fmt.Errorf("%w: not JSON", ErrPayload)
	case len(m.Payload) > MaxPayloadBytes:
		return fmt.Errorf("%w (%d)", ErrTooBig, len(m.Payload))
	}

	return nil
}

// TTL of the message.
func (m Message) TTL() int {
	if m.TTLSeconds == nil {
		return DefaultTTLSeconds
	}

	return int(*m.TTLSeconds)
}

func (s Subscription) validate(hosts []string) error {
	if len(s.Endpoint) > maxEndpoint {
		return fmt.Errorf("%w: endpoint too long", ErrSubscription)
	}

	endpoint, err := url.Parse(s.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil {
		return fmt.Errorf("%w: endpoint is an https URL", ErrSubscription)
	}

	if endpoint.Scheme != "https" {
		return fmt.Errorf("%w: %w: endpoint is not https", ErrSubscription, ErrEndpoint)
	}

	if !Allowed(endpoint.Hostname(), hosts) {
		return fmt.Errorf("%w: %s", ErrEndpoint, endpoint.Hostname())
	}

	p, err := decode(s.Keys.P256dh)
	if err != nil {
		return fmt.Errorf("%w: keys.p256dh: %w", ErrSubscription, err)
	}

	if _, err := ecdh.P256().NewPublicKey(p); err != nil {
		return fmt.Errorf("%w: keys.p256dh is not a P-256 point", ErrSubscription)
	}

	if a, err := decode(s.Keys.Auth); err != nil || len(a) != 16 {
		return fmt.Errorf("%w: keys.auth is 16 bytes of base64url", ErrSubscription)
	}

	return nil
}

// Allowed reports whether host matches hosts (empty: DefaultHosts): an
// exact name or *.suffix; never an IP address.
func Allowed(host string, hosts []string) bool {
	if len(hosts) == 0 {
		hosts = DefaultHosts
	}

	return netguard.Allowed(host, hosts)
}

// Target is the subscription as audited: its push service and a hash,
// never the endpoint, which identifies a browser.
func (m Message) Target() string {
	host := ""
	if u, err := url.Parse(m.Subscription.Endpoint); err == nil {
		host = u.Hostname()
	}

	sum := sha256.Sum256([]byte(m.Subscription.Endpoint))

	return "webpush:" + host + "/" + hex.EncodeToString(sum[:])
}

// Client of one channel.
type Client struct {
	cfg  Config
	http *http.Client
}

// New is the client of cfg (validated); client nil: the default, which
// follows no redirect (an endpoint must not bounce courier elsewhere) and
// dials only public addresses, directly (netguard.ErrForbiddenAddress).
func New(cfg Config, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{
			Transport:     netguard.Transport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}

	return &Client{cfg: cfg, http: client}
}

// Hosts the channel's endpoints may name.
func (c *Client) Hosts() []string { return c.cfg.Hosts }

// PublicKey of the channel: browsers subscribe with it.
func (c *Client) PublicKey() string { return c.cfg.PublicKey }

// Check verifies the key pair; no push service has a call to try.
func (c *Client) Check(context.Context) error { return c.cfg.Validate() }

// Send encrypts and posts m (validated) and returns the message's id —
// the push service's Location — or a *provider.Error: final for a gone or
// refused subscription, later for a rate limit.
func (c *Client) Send(ctx context.Context, m Message) (string, error) {
	subscriber := strings.TrimPrefix(c.cfg.Subject, "mailto:")

	res, err := wp.SendNotificationWithContext(ctx, m.Payload, &wp.Subscription{
		Endpoint: m.Subscription.Endpoint, Keys: wp.Keys{P256dh: m.Subscription.Keys.P256dh, Auth: m.Subscription.Keys.Auth},
	}, &wp.Options{
		HTTPClient: c.http, Subscriber: subscriber, VAPIDPublicKey: c.cfg.PublicKey,
		VAPIDPrivateKey: c.cfg.PrivateKey.Reveal(), TTL: m.TTL(), Urgency: wp.Urgency(m.Urgency), Topic: m.Topic,
	})
	if err != nil {
		if errors.Is(err, wp.ErrMaxPadExceeded) {
			return "", provider.Permanent("too_big", err.Error())
		}

		if errors.Is(err, netguard.ErrForbiddenAddress) {
			return "", provider.Permanent(CodeForbidden, err.Error())
		}

		return "", fmt.Errorf("webpush: %w", err)
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(res.Body, maxAnswer))

	return refusal(res, strings.TrimSpace(string(body)))
}

// refusal classifies the push service's answer.
func refusal(res *http.Response, body string) (string, error) {
	text := "push service answered " + res.Status
	if body != "" {
		text += ": " + body
	}

	switch code := res.StatusCode; {
	case code >= 200 && code < 300:
		return res.Header.Get("Location"), nil
	case code == http.StatusNotFound || code == http.StatusGone:
		// The browser unsubscribed or the subscription expired: the
		// product deletes it.
		return "", provider.Permanent("gone", text)
	case code == http.StatusRequestEntityTooLarge:
		return "", provider.Permanent("too_big", text)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		// The subscription was made with another applicationServerKey.
		return "", provider.Permanent("unauthorized", text)
	case code == http.StatusTooManyRequests:
		return "", provider.Later("rate_limited", text, retryAfter(res.Header.Get("Retry-After")))
	case code >= 400 && code < 500:
		return "", provider.Permanent("refused", text)
	default:
		return "", fmt.Errorf("webpush: %s", text) //nolint:err113 // the push service's own answer
	}
}

// retryAfter reads Retry-After (seconds or a date); default a minute.
func retryAfter(v string) time.Duration {
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}

	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}

	return time.Minute
}
