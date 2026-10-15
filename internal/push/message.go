// Package push is the push domain: the message a caller hands to
// courier.SendPush, its checks, token hashing and the FCM HTTP v1 message
// it becomes. Everything FCM takes is reachable: the common fields are
// modeled, and the raw android, apns, webpush and fcm_options blocks are
// merged over them for whatever is not.
package push

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Errors of Validate; every one is final.
var (
	ErrTarget    = errors.New("push: exactly one of token, topic or condition is required")
	ErrToken     = errors.New("push: invalid device token")
	ErrTopic     = errors.New("push: invalid topic")
	ErrCondition = errors.New("push: invalid condition")
	ErrDataKey   = errors.New("push: reserved data key")
	ErrPriority  = errors.New("push: priority is high or normal")
	ErrTTL       = errors.New("push: ttl_seconds is 0 to 2419200 (28 days)")
	ErrTooBig    = errors.New("push: message exceeds 4096 bytes of payload")
)

// Limits of FCM.
const (
	MaxPayloadBytes = 4096
	MaxTTLSeconds   = 28 * 24 * 3600
	maxTokenLen     = 4096
	maxCondition    = 1024
	envelopeBytes   = 1024
)

var topicPattern = regexp.MustCompile(`^[a-zA-Z0-9\-_.~%]{1,900}$`)

// Message is one push: to a device token, a topic or a condition.
type Message struct {
	Channel string `json:"channel,omitempty" schemapb:"description=Push channel of the installation, else the default"`

	Token     string `json:"token,omitempty"     schemapb:"description=Device registration token"`
	Topic     string `json:"topic,omitempty"     schemapb:"description=Topic, instead of a token"`
	Condition string `json:"condition,omitempty" schemapb:"description=Topic condition, instead of a token"`

	Title string            `json:"title,omitempty"`
	Body  string            `json:"body,omitempty"`
	Image string            `json:"image,omitempty" schemapb:"description=Image URL"`
	Data  map[string]string `json:"data,omitempty"  schemapb:"description=Key-value data for the app"`

	Priority    string `json:"priority,omitempty"     schemapb:"description=high or normal"`
	TTLSeconds  int64  `json:"ttl_seconds,omitempty"  schemapb:"description=How long FCM keeps it for an offline device"`
	CollapseKey string `json:"collapse_key,omitempty" schemapb:"description=A newer message with the key replaces it"`

	// Raw FCM blocks, merged over what the fields above produce: any
	// field FCM takes (mutable-content, content-available, apns-push-type,
	// direct_boot_ok, notification options...). See the FCM HTTP v1
	// Message reference.
	Android    map[string]any `json:"android,omitempty"     schemapb:"description=Raw AndroidConfig"`
	APNS       map[string]any `json:"apns,omitempty"        schemapb:"description=Raw ApnsConfig"`
	Webpush    map[string]any `json:"webpush,omitempty"     schemapb:"description=Raw WebpushConfig"`
	FCMOptions map[string]any `json:"fcm_options,omitempty" schemapb:"description=Raw FcmOptions"`

	// ValidateOnly: FCM checks the message and delivers nothing.
	ValidateOnly bool `json:"validate_only,omitempty" schemapb:"description=Check the message with FCM, deliver nothing"`
}

// Validate checks what FCM would refuse before it is sent.
func (m Message) Validate() error {
	if err := m.checkTarget(); err != nil {
		return err
	}

	for k := range m.Data {
		if reservedKey(k) {
			return fmt.Errorf("%w: %q", ErrDataKey, k)
		}
	}

	if m.Priority != "" && m.Priority != "high" && m.Priority != "normal" {
		return ErrPriority
	}

	if m.TTLSeconds < 0 || m.TTLSeconds > MaxTTLSeconds {
		return ErrTTL
	}

	body, err := Build(m)
	if err != nil {
		return err
	}

	if len(body) > MaxPayloadBytes+envelopeBytes {
		return ErrTooBig
	}

	return nil
}

func (m Message) checkTarget() error {
	kinds := 0

	for _, v := range []string{m.Token, m.Topic, m.Condition} {
		if v != "" {
			kinds++
		}
	}

	switch {
	case kinds != 1:
		return ErrTarget
	case m.Token != "" && (len(m.Token) > maxTokenLen || strings.ContainsAny(m.Token, " \t\r\n")):
		return ErrToken
	case m.Topic != "" && !topicPattern.MatchString(strings.TrimPrefix(m.Topic, "/topics/")):
		return ErrTopic
	case m.Condition != "" && (len(m.Condition) > maxCondition || !strings.Contains(m.Condition, "in topics")):
		return ErrCondition
	}

	return nil
}

func reservedKey(k string) bool {
	switch k {
	case "", "from", "notification", "message_type", "collapse_key":
		return true
	}

	return strings.HasPrefix(k, "google") || strings.HasPrefix(k, "gcm")
}

// Build is the FCM HTTP v1 request body of m.
func Build(m Message) ([]byte, error) {
	msg := map[string]any{}

	switch {
	case m.Token != "":
		msg["token"] = m.Token
	case m.Topic != "":
		msg["topic"] = strings.TrimPrefix(m.Topic, "/topics/")
	default:
		msg["condition"] = m.Condition
	}

	if n := omitEmpty(map[string]any{"title": m.Title, "body": m.Body, "image": m.Image}); len(n) > 0 {
		msg["notification"] = n
	}

	if len(m.Data) > 0 {
		msg["data"] = m.Data
	}

	modeled(msg, m)

	raws := map[string]map[string]any{
		"android": m.Android, "apns": m.APNS, "webpush": m.Webpush, "fcm_options": m.FCMOptions,
	}
	for key, raw := range raws {
		if len(raw) > 0 {
			base, _ := msg[key].(map[string]any)
			msg[key] = merge(base, raw)
		}
	}

	envelope := map[string]any{"message": msg}
	if m.ValidateOnly {
		envelope["validate_only"] = true
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("push: encode: %w", err)
	}

	return body, nil
}

// modeled adds the platform blocks the modeled fields produce.
func modeled(msg map[string]any, m Message) {
	android := omitEmpty(map[string]any{"collapse_key": m.CollapseKey})
	headers := map[string]any{}

	if m.Priority != "" {
		android["priority"] = strings.ToUpper(m.Priority)
		headers["apns-priority"] = map[string]string{"high": "10", "normal": "5"}[m.Priority]
	}

	if m.TTLSeconds > 0 {
		android["ttl"] = strconv.FormatInt(m.TTLSeconds, 10) + "s"
	}

	if m.CollapseKey != "" {
		headers["apns-collapse-id"] = m.CollapseKey
	}

	if len(android) > 0 {
		msg["android"] = android
	}

	if len(headers) > 0 {
		msg["apns"] = map[string]any{"headers": headers}
	}
}

// merge is base with over laid on top, recursively for objects.
func merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}

	for k, v := range over {
		if sub, ok := v.(map[string]any); ok {
			if prev, isMap := out[k].(map[string]any); isMap {
				out[k] = merge(prev, sub)

				continue
			}
		}

		out[k] = v
	}

	return out
}

func omitEmpty(in map[string]any) map[string]any {
	for k, v := range in {
		if s, ok := v.(string); ok && s == "" {
			delete(in, k)
		}
	}

	return in
}

// Hash is how a token is kept in audit records: sha256, hex.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// Target names m's destination for audit: a token hashed, else the topic
// or condition as is.
func (m Message) Target() string {
	switch {
	case m.Token != "":
		return "token:" + Hash(m.Token)
	case m.Topic != "":
		return "topic:" + m.Topic
	default:
		return "condition:" + m.Condition
	}
}
