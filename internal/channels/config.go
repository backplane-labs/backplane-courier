// Package channels holds the installation's channels: their configuration
// (exactly one provider each), the runtime of each (pool, rate, timeout)
// and which channel a kind uses by default.
package channels

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/config"

	"github.com/backplane-labs/backplane-courier/internal/fcm"
	"github.com/backplane-labs/backplane-courier/internal/smtp"
	"github.com/backplane-labs/backplane-courier/internal/webpush"
)

// Built-in kinds of delivery.
const (
	KindMail    = "mail"
	KindPush    = "push"
	KindWebPush = "webpush"
)

// Errors of the configuration.
var (
	ErrNoChannels = errors.New("channels: at least one channel is required")
	ErrConfig     = errors.New("channels: invalid channel")
)

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Spec is one channel: exactly one of smtp, fcm, webpush, log or provider.
type Spec struct {
	SMTP    *smtp.Config    `json:"smtp,omitempty"    schemapb:"description=Mail through an SMTP relay"`
	FCM     *fcm.Config     `json:"fcm,omitempty"     schemapb:"description=Push through Firebase Cloud Messaging"`
	WebPush *webpush.Config `json:"webpush,omitempty" schemapb:"description=Web Push to browsers (VAPID)"`
	Log     *Log            `json:"log,omitempty"     schemapb:"description=Deliveries go to the service log"`
	// Provider is a plugin or a compiled-in provider, by name.
	Provider *Provider `json:"provider,omitempty" schemapb:"description=A plugin or a compiled-in provider"`

	// Pool bounds the deliveries in flight on the channel.
	Pool int64 `json:"pool" schemapb:"default=8;gte=1;lte=1024"`
	// RatePerMinute bounds deliveries per minute; 0: unbounded.
	RatePerMinute int64         `json:"rate_per_minute" schemapb:"default=0;gte=0"`
	Timeout       time.Duration `json:"timeout"         schemapb:"default=30s"`
}

// Log stands in for a kind: mail, push or a provider's.
type Log struct {
	Kind string `json:"kind" schemapb:"description=The kind it stands in for: mail, push, webpush or a provider's"`
}

// Provider names an extension provider and its channel settings.
type Provider struct {
	Name string `json:"name" schemapb:"description=The plugin or compiled-in provider"`
	// Settings and Secrets are the channel's settings for the provider,
	// merged; secrets are masked wherever the configuration is shown.
	Settings map[string]string        `json:"settings,omitempty"`
	Secrets  map[string]config.Secret `json:"secrets,omitempty"`
}

// Plugin is a provider running somewhere courier can dial: how and where
// it runs is the deployment's concern. Courier waits for it at startup,
// probes its gRPC health as part of its own readiness, and keeps the
// connection alive.
type Plugin struct {
	// Address: unix:///path or host:port.
	Address string `json:"address" schemapb:"description=unix:///path or host:port of the plugin"`
	// TLS of the connection; off: plaintext (a sidecar, a unix socket).
	TLS config.TLS `json:"tls"`
	// StartupTimeout: how long courier waits at startup for the plugin to
	// answer Describe.
	StartupTimeout time.Duration `json:"startup_timeout" schemapb:"default=30s"`
}

// Validate a plugin.
func (p Plugin) Validate() error {
	if p.Address == "" {
		return fmt.Errorf("%w: a plugin needs an address", ErrConfig)
	}

	return nil
}

// Validate checks exactly one provider and its own rules.
func (c Spec) Validate() error {
	set := 0

	for _, on := range []bool{c.SMTP != nil, c.FCM != nil, c.WebPush != nil, c.Log != nil, c.Provider != nil} {
		if on {
			set++
		}
	}

	if set != 1 {
		return fmt.Errorf("%w: exactly one of smtp, fcm, webpush, log or provider", ErrConfig)
	}

	switch {
	case c.SMTP != nil:
		return c.SMTP.Validate() //nolint:wrapcheck // names itself
	case c.FCM != nil:
		return c.FCM.Validate() //nolint:wrapcheck // names itself
	case c.WebPush != nil:
		return c.WebPush.Validate() //nolint:wrapcheck // names itself
	case c.Log != nil && !kindPattern.MatchString(c.Log.Kind):
		return fmt.Errorf("%w: log.kind is mail, push, webpush or a provider's kind", ErrConfig)
	case c.Provider != nil && c.Provider.Name == "":
		return fmt.Errorf("%w: provider.name is required", ErrConfig)
	}

	return nil
}

// Config is the channels by name.
type Config map[string]Spec

// Validate every channel.
func (c Config) Validate() error {
	if len(c) == 0 {
		return ErrNoChannels
	}

	var errs []error

	for _, name := range c.Names() {
		if err := c[name].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("channel %q: %w", name, err))
		}
	}

	return errors.Join(errs...)
}

// Names of the channels, sorted.
func (c Config) Names() []string {
	out := make([]string, 0, len(c))
	for name := range c {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}
