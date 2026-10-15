package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/deps"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"

	"github.com/backplane-labs/backplane-courier/internal/fcm"
	"github.com/backplane-labs/backplane-courier/internal/smtp"
	"github.com/backplane-labs/backplane-courier/provider"
)

// Providers of the built-in kinds.
const (
	ProviderSMTP = "smtp"
	ProviderFCM  = "fcm"
	ProviderLog  = "log"
)

// Errors of picking a channel; final for a delivery.
var (
	ErrUnknown   = errors.New("channels: no such channel")
	ErrWrongKind = errors.New("channels: the channel delivers another kind")
	ErrNoDefault = errors.New("channels: name a channel: the kind has several and no default")
	ErrProvider  = errors.New("channels: no such provider")
)

// Extension is a provider courier loaded (a plugin or a compiled-in one).
type Extension struct {
	Name       string
	Provider   provider.Provider
	Descriptor provider.Descriptor
	// Plugin: it runs as its own process; false: compiled in.
	Plugin bool
}

// Channel at run time.
type Channel struct {
	Name string
	// Kind: mail, push or a provider's kind.
	Kind string
	// Provider: smtp, fcm, log, or the extension's name.
	Provider string
	Spec     Spec

	Mail      *smtp.Sender
	Push      *fcm.Client
	Extension *Extension
	// Settings of an extension channel as JSON (settings and secrets).
	Settings []byte

	pool    chan struct{}
	limiter *rate.Limiter
}

// Acquire a delivery slot within the channel's pool and rate; release it
// when done.
func (c *Channel) Acquire(ctx context.Context) (func(), error) {
	select {
	case c.pool <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err() //nolint:wrapcheck // the caller's context
	}

	release := func() { <-c.pool }

	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			release()

			return nil, fmt.Errorf("channel %s: rate: %w", c.Name, err)
		}
	}

	return release, nil
}

// Timeout of one delivery.
func (c *Channel) Timeout() time.Duration { return c.Spec.Timeout }

// Channels is the component: every channel and the defaults by kind.
type Channels struct {
	deps.Component

	byName   map[string]*Channel
	defaults map[string]string
}

// Options of New.
type Options struct {
	// Extensions by name: plugins and compiled-in providers.
	Extensions map[string]*Extension
	// Defaults: the channel of a kind when a message names none.
	Defaults map[string]string
	// TokenSource authorizes every FCM channel instead of its credentials.
	TokenSource oauth2.TokenSource
}

// New builds the channels of cfg (validated by Open).
func New(parent deps.Scope, cfg Config, opts Options) (*Channels, error) {
	c := &Channels{
		Component: deps.NewComponent(parent, "channels"), byName: map[string]*Channel{}, defaults: map[string]string{},
	}

	for _, name := range cfg.Names() {
		ch, err := build(name, cfg[name], opts)
		if err != nil {
			return nil, fmt.Errorf("channel %q: %w", name, err)
		}

		c.byName[name] = ch
	}

	for kind, name := range opts.Defaults {
		ch, ok := c.byName[name]
		if !ok {
			return nil, fmt.Errorf("default of %s: %w: %q", kind, ErrUnknown, name)
		}

		if ch.Kind != kind {
			return nil, fmt.Errorf("default of %s: %w: %q is %s", kind, ErrWrongKind, name, ch.Kind)
		}

		c.defaults[kind] = name
	}

	return c, nil
}

func build(name string, spec Spec, opts Options) (*Channel, error) {
	ch := &Channel{Name: name, Spec: spec, pool: make(chan struct{}, max(spec.Pool, 1))}
	if spec.RatePerMinute > 0 {
		ch.limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(spec.RatePerMinute)), int(max(spec.Pool, 1)))
	}

	switch {
	case spec.SMTP != nil:
		ch.Kind, ch.Provider, ch.Mail = KindMail, ProviderSMTP, smtp.New(*spec.SMTP, spec.Timeout)
	case spec.FCM != nil:
		client, err := fcm.New(*spec.FCM, opts.TokenSource)
		if err != nil {
			return nil, err //nolint:wrapcheck // names itself
		}

		ch.Kind, ch.Provider, ch.Push = KindPush, ProviderFCM, client
	case spec.Log != nil:
		ch.Kind, ch.Provider = spec.Log.Kind, ProviderLog
	case spec.Provider != nil:
		ext, ok := opts.Extensions[spec.Provider.Name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrProvider, spec.Provider.Name)
		}

		settings := map[string]string{}
		for k, v := range spec.Provider.Settings {
			settings[k] = v
		}

		for k, v := range spec.Provider.Secrets {
			settings[k] = v.Reveal()
		}

		raw, err := json.Marshal(settings)
		if err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}

		ch.Kind, ch.Provider, ch.Extension, ch.Settings = ext.Descriptor.Kind, ext.Name, ext, raw
	}

	return ch, nil
}

// Pick is the channel a delivery of kind uses: the one named, else the
// kind's default, else the kind's only channel.
func (c *Channels) Pick(kind, name string) (*Channel, error) {
	if name == "" {
		name = c.defaults[kind]
	}

	if name == "" {
		for _, ch := range c.byName {
			if ch.Kind != kind {
				continue
			}

			if name != "" {
				return nil, fmt.Errorf("%w: %s", ErrNoDefault, kind)
			}

			name = ch.Name
		}
	}

	ch, ok := c.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q (%s)", ErrUnknown, name, kind)
	}

	if ch.Kind != kind {
		return nil, fmt.Errorf("%w: %q is %s, not %s", ErrWrongKind, name, ch.Kind, kind)
	}

	return ch, nil
}

// Get a channel by name.
func (c *Channels) Get(name string) (*Channel, bool) { ch, ok := c.byName[name]; return ch, ok }

// All channels by name.
func (c *Channels) All() map[string]*Channel { return c.byName }

// IsDefault reports whether ch is its kind's default (named or the only
// one).
func (c *Channels) IsDefault(ch *Channel) bool {
	picked, err := c.Pick(ch.Kind, "")

	return err == nil && picked == ch
}
