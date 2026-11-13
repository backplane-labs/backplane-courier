// Package apns is the built-in Apple push provider: APNs over HTTP/2 per
// channel (a team's auth key) with token-based authentication, and every
// push type APNs takes, VoIP for PushKit and CallKit included. FCM cannot
// deliver a VoIP push; this channel talks to Apple directly.
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/backplane-labs/backplane/pkg/backplane/tracehttp"
	"github.com/golang-jwt/jwt/v5"

	"github.com/backplane-labs/backplane-courier/internal/push"
	"github.com/backplane-labs/backplane-courier/provider"
)

// Endpoints of Apple.
const (
	DefaultEndpoint = "https://api.push.apple.com"
	// SandboxEndpoint takes the tokens of development builds.
	SandboxEndpoint = "https://api.sandbox.push.apple.com"
)

// Limits of APNs.
const (
	MaxPayloadBytes     = 4096
	MaxVoIPPayloadBytes = 5120
	// MaxTTLSeconds: APNs keeps a message for an offline device about 30 days.
	MaxTTLSeconds = 30 * 24 * 3600
	maxCollapseID = 64
	maxAnswer     = 4096
	// tokenLifetime: Apple refuses a provider token older than an hour and
	// one refreshed more often than every 20 minutes.
	tokenLifetime = 40 * time.Minute
)

// Values of apns-priority.
const (
	priorityNow         = 10
	priorityConsiderate = 5
	priorityLow         = 1
)

// Push types APNs takes (the apns-push-type header).
const (
	TypeAlert      = "alert"
	TypeBackground = "background"
	TypeVoIP       = "voip"
)

// checkToken is a device token no device has: a check sends to it, and an
// answer of BadDeviceToken proves the key, the team and the topic.
const checkToken = "0000000000000000000000000000000000000000000000000000000000000000"

// Codes APNs answers with that a check expects for the placeholder token.
const reasonBadDeviceToken = "BadDeviceToken"

// Errors of the configuration and of Validate; every one is final.
var (
	ErrConfig     = errors.New("apns: invalid channel")
	ErrToken      = errors.New("apns: token is the device token in hex")
	ErrTopic      = errors.New("apns: topic is required: the message's or the channel's")
	ErrPushType   = errors.New("apns: unknown push_type")
	ErrPriority   = errors.New("apns: priority is 10, 5 or 1")
	ErrVoIP       = errors.New("apns: a voip push has priority 10 and a .voip topic")
	ErrTTL        = errors.New("apns: ttl_seconds is 0 to 2592000 (30 days)")
	ErrCollapseID = errors.New("apns: collapse_id is up to 64 bytes")
	ErrID         = errors.New("apns: id is a UUID")
	ErrPayload    = errors.New("apns: payload is a JSON object")
	ErrTooBig     = errors.New("apns: payload exceeds 4096 bytes (5120 for voip)")
)

var (
	tokenPattern = regexp.MustCompile(`^[0-9a-fA-F]{8,512}$`)
	idPattern    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	teamPattern  = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	pushTypes    = regexp.MustCompile(
		`^(alert|background|voip|location|complication|fileprovider|mdm|liveactivity|pushtotalk|widgets|controls)$`)
)

// Config of an APNs channel.
type Config struct {
	KeyID  string `json:"key_id"  schemapb:"description=Key ID of the APNs auth key (.p8)"`
	TeamID string `json:"team_id" schemapb:"description=Apple Developer Team ID"`
	// Key is the .p8 file's PEM contents.
	Key config.Secret `json:"key" schemapb:"secret=true;description=Contents of the APNs auth key file (.p8, PEM)"`
	// Topic is used when a message names none: the bundle ID, or
	// <bundle ID>.voip for VoIP.
	Topic string `json:"topic,omitempty" schemapb:"description=Default apns-topic: the bundle ID or <bundle ID>.voip"`
	// Endpoint: DefaultEndpoint when empty; SandboxEndpoint for development
	// builds, or a stand-in on a test stand.
	Endpoint string `json:"endpoint,omitempty" schemapb:"description=APNs endpoint, else api.push.apple.com"`
}

// Validate checks what the schema cannot, the key included.
func (c Config) Validate() error {
	switch {
	case !teamPattern.MatchString(c.KeyID):
		return fmt.Errorf("%w: key_id is 10 characters A-Z 0-9", ErrConfig)
	case !teamPattern.MatchString(c.TeamID):
		return fmt.Errorf("%w: team_id is 10 characters A-Z 0-9", ErrConfig)
	}

	if _, err := jwt.ParseECPrivateKeyFromPEM([]byte(c.Key.Reveal())); err != nil {
		return fmt.Errorf("%w: key is not an EC private key in PEM (.p8): %w", ErrConfig, err)
	}

	return nil
}

// Message is one push to one device.
type Message struct {
	Channel string `json:"channel,omitempty" schemapb:"description=APNs channel of the installation, else the default"`

	// Token is the device token: the PushKit token for voip, else the app's
	// remote notification token.
	Token string `json:"token" schemapb:"description=Device token in hex (the PushKit token for voip)"`
	// Topic: the bundle ID, <bundle ID>.voip for voip; else the channel's.
	Topic string `json:"topic,omitempty" schemapb:"description=apns-topic, else the channel's"`

	// PushType: alert (default), background, voip, location, complication,
	// fileprovider, mdm, liveactivity, pushtotalk, widgets or controls.
	PushType string `json:"push_type,omitempty" schemapb:"description=apns-push-type, else alert"`
	// Priority: 10 now, 5 power-considerate, 1 low; 0: APNs' default.
	Priority int64 `json:"priority,omitempty" schemapb:"description=apns-priority: 10, 5 or 1"`
	// TTLSeconds sets apns-expiration; 0: deliver now or never; absent: APNs'
	// default.
	TTLSeconds *int64 `json:"ttl_seconds,omitempty" schemapb:"description=Seconds kept for an offline device"`
	// CollapseID: a newer push with the same one replaces it on the device.
	CollapseID string `json:"collapse_id,omitempty" schemapb:"description=apns-collapse-id, up to 64 bytes"`
	ID         string `json:"id,omitempty"          schemapb:"description=apns-id (a UUID), else APNs makes one"`

	// Payload is the whole APNs payload: aps and the app's own keys.
	Payload json.RawMessage `json:"payload" schemapb:"description=The APNs JSON payload: aps and the app's own keys"`
}

// Validate checks what APNs would refuse, with the channel cfg's default
// topic.
func (m Message) Validate(cfg Config) error {
	if err := m.checkAddress(cfg); err != nil {
		return err
	}

	if err := m.checkHeaders(); err != nil {
		return err
	}

	var payload map[string]json.RawMessage

	switch {
	case json.Unmarshal(m.Payload, &payload) != nil || payload == nil:
		return ErrPayload
	case len(m.Payload) > m.maxPayload():
		return ErrTooBig
	}

	return nil
}

// checkAddress: the token, the topic, and a voip push's topic.
func (m Message) checkAddress(cfg Config) error {
	switch {
	case !tokenPattern.MatchString(m.Token) || len(m.Token)%2 != 0:
		return ErrToken
	case m.topic(cfg) == "":
		return ErrTopic
	case m.pushType() == TypeVoIP && !strings.HasSuffix(m.topic(cfg), ".voip"):
		return ErrVoIP
	}

	return nil
}

// checkHeaders: the values of the apns-* headers.
func (m Message) checkHeaders() error {
	switch {
	case !pushTypes.MatchString(m.pushType()):
		return ErrPushType
	case m.Priority != 0 && m.Priority != priorityNow && m.Priority != priorityConsiderate && m.Priority != priorityLow:
		return ErrPriority
	case m.pushType() == TypeVoIP && m.Priority != 0 && m.Priority != priorityNow:
		return ErrVoIP
	case m.TTLSeconds != nil && (*m.TTLSeconds < 0 || *m.TTLSeconds > MaxTTLSeconds):
		return ErrTTL
	case len(m.CollapseID) > maxCollapseID:
		return ErrCollapseID
	case m.ID != "" && !idPattern.MatchString(m.ID):
		return ErrID
	}

	return nil
}

func (m Message) topic(cfg Config) string {
	if m.Topic != "" {
		return m.Topic
	}

	return cfg.Topic
}

func (m Message) pushType() string {
	if m.PushType == "" {
		return TypeAlert
	}

	return m.PushType
}

func (m Message) maxPayload() int {
	if m.pushType() == TypeVoIP {
		return MaxVoIPPayloadBytes
	}

	return MaxPayloadBytes
}

// Target names the destination for audit: the token hashed, as push does.
func (m Message) Target() string { return "token:" + push.Hash(m.Token) }

// Client of one channel.
type Client struct {
	cfg      Config
	endpoint string
	key      *ecdsa.PrivateKey
	http     *http.Client
	now      func() time.Time

	mu     sync.Mutex
	bearer string
	issued time.Time
}

// New is the client of cfg (validated); client posts instead of an HTTP/2
// one (tests).
func New(cfg Config, client *http.Client) (*Client, error) {
	key, err := jwt.ParseECPrivateKeyFromPEM([]byte(cfg.Key.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("%w: key: %w", ErrConfig, err)
	}

	if client == nil {
		transport, _ := http.DefaultTransport.(*http.Transport)
		transport = transport.Clone()
		transport.ForceAttemptHTTP2 = true
		client = &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}

	// Every push is a client span carrying the caller's trace, whichever
	// client sends it.
	traced := *client
	traced.Transport = tracehttp.NewTransport(orDefault(traced.Transport))
	client = &traced

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	return &Client{cfg: cfg, endpoint: strings.TrimSuffix(endpoint, "/"), key: key, http: client, now: time.Now}, nil
}

// Config of the channel.
func (c *Client) Config() Config { return c.cfg }

// Send posts m and returns the apns-id APNs answered with; a refusal is a
// *provider.Error whose code is APNs' reason (Unregistered, BadDeviceToken,
// ...).
func (c *Client) Send(ctx context.Context, m Message) (string, error) {
	headers := map[string]string{"apns-topic": m.topic(c.cfg), "apns-push-type": m.pushType()}

	if m.Priority != 0 {
		headers["apns-priority"] = strconv.FormatInt(m.Priority, 10)
	}

	if m.TTLSeconds != nil {
		expiration := int64(0)
		if *m.TTLSeconds > 0 {
			expiration = c.now().Add(time.Duration(*m.TTLSeconds) * time.Second).Unix()
		}

		headers["apns-expiration"] = strconv.FormatInt(expiration, 10)
	}

	if m.CollapseID != "" {
		headers["apns-collapse-id"] = m.CollapseID
	}

	if m.ID != "" {
		headers["apns-id"] = m.ID
	}

	return c.post(ctx, m.Token, m.Payload, headers)
}

// Check sends to a token no device has: an answer of BadDeviceToken means
// APNs took the key, the team and the channel's topic; nothing is delivered.
func (c *Client) Check(ctx context.Context) error {
	if c.cfg.Topic == "" {
		return fmt.Errorf("%w: set topic to check the channel", ErrConfig)
	}

	headers := map[string]string{"apns-topic": c.cfg.Topic, "apns-push-type": TypeAlert}

	_, err := c.post(ctx, checkToken, []byte(`{"aps":{}}`), headers)

	var refusal *provider.Error
	if errors.As(err, &refusal) && refusal.Code == reasonBadDeviceToken {
		return nil
	}

	if err == nil {
		return fmt.Errorf("%w: APNs took a push to a placeholder token", ErrConfig)
	}

	return err
}

func (c *Client) post(ctx context.Context, token string, body []byte, headers map[string]string) (string, error) {
	bearer, err := c.token()
	if err != nil {
		return "", provider.Permanent("InvalidProviderKey", err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/3/device/"+token, bytes.NewReader(body))
	if err != nil {
		return "", provider.Permanent("INVALID_REQUEST", err.Error())
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+bearer)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("apns: %w", err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxAnswer))

	if res.StatusCode == http.StatusOK {
		return res.Header.Get("Apns-Id"), nil
	}

	refusal := parseError(res, raw)
	if refusal.Code == "ExpiredProviderToken" {
		c.expire(bearer)
	}

	return "", refusal
}

// token is the provider token, signed again once it is tokenLifetime old.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.bearer != "" && now.Sub(c.issued) < tokenLifetime {
		return c.bearer, nil
	}

	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": c.cfg.TeamID, "iat": now.Unix()})
	t.Header["kid"] = c.cfg.KeyID

	signed, err := t.SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("apns: sign provider token: %w", err)
	}

	c.bearer, c.issued = signed, now

	return signed, nil
}

// expire drops bearer when it is still the current token: APNs refused it
// as too old, the next send signs a new one.
func (c *Client) expire(bearer string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.bearer == bearer {
		c.bearer = ""
	}
}

// parseError reads APNs' {"reason": ...}. A dead token (Unregistered,
// ExpiredToken, BadDeviceToken, DeviceTokenNotForTopic), a malformed push
// and a refused key or topic are final; an expired provider token, an
// idle connection, throttling and outages are retried.
func parseError(res *http.Response, raw []byte) *provider.Error {
	var body struct {
		Reason string `json:"reason"`
	}

	_ = json.Unmarshal(raw, &body)

	e := &provider.Error{Code: body.Reason, Message: http.StatusText(res.StatusCode)}
	if e.Code == "" {
		e.Code = strconv.Itoa(res.StatusCode)
	}

	switch {
	case e.Code == "ExpiredProviderToken" || e.Code == "IdleTimeout" || e.Code == "TooManyProviderTokenUpdates":
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError:
	case res.StatusCode >= http.StatusBadRequest:
		e.Permanent = true
	}

	if s := res.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	return e
}

// orDefault is rt, or http.DefaultTransport when nil (as http.Client reads it).
func orDefault(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		return http.DefaultTransport
	}

	return rt
}
