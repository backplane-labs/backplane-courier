// Package courier is the courier service as a library: Main runs it, with
// the providers a custom build compiles in.
//
//	func main() { courier.Main(courier.WithProvider("smsc", smsc.New())) }
//
// The stock binary (cmd/courier) is Main with nothing added. A provider in
// another language runs as a plugin (package plugin) the configuration
// names by address.
package courier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane"
	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/backplane-labs/backplane/pkg/backplane/deps"
	"github.com/backplane-labs/backplane/pkg/backplane/drivers/standard"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/backplane-labs/backplane-courier/internal/admin"
	"github.com/backplane-labs/backplane-courier/internal/channels"
	core "github.com/backplane-labs/backplane-courier/internal/courier"
	"github.com/backplane-labs/backplane-courier/internal/mail"
	"github.com/backplane-labs/backplane-courier/internal/ui"
	"github.com/backplane-labs/backplane-courier/plugin"
	courierconsolev1 "github.com/backplane-labs/backplane-courier/proto/courier/console/v1"
	"github.com/backplane-labs/backplane-courier/provider"
)

// describeTimeout bounds asking a plugin what it delivers at startup.
const describeTimeout = 10 * time.Second

// ErrProviderName: a provider name is used twice.
var ErrProviderName = errors.New("courier: provider name used twice")

// Config is the whole configuration: file (BACKPLANE_CONFIG_FILE) < env
// COURIER_* (BACKPLANE_* for the SDK block) < Consul KV for Live fields.
type Config struct {
	config.Backplane `json:"backplane"`

	// Channels by name; each has exactly one provider.
	Channels channels.Config `json:"channels"`
	// Plugins by name: providers courier dials.
	Plugins map[string]channels.Plugin `json:"plugins,omitempty"`
	// Defaults: the channel of a kind (mail, push, a provider's) when a
	// message names none; a kind with one channel needs no default.
	Defaults map[string]string `json:"defaults,omitempty"`
	// DryRun records deliveries without sending them; live.
	DryRun config.Live[bool] `json:"dry_run" schemapb:"default=false;description=Record deliveries, send nothing"`
	// MaxMessageBytes bounds a mail's bodies and attachments.
	MaxMessageBytes int64 `json:"max_message_bytes" schemapb:"default=26214400;gte=4096"`
	// MaxRecipients bounds a mail's to + cc + bcc.
	MaxRecipients int64 `json:"max_recipients" schemapb:"default=100;gte=1;lte=1000"`
}

// Validate what the schema cannot.
func (c *Config) Validate() error {
	var errs []error

	for name, p := range c.Plugins {
		if err := p.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
		}
	}

	return errors.Join(errs...)
}

// Option configures Main.
type Option func(*options)

type options struct {
	providers map[string]provider.Provider
	tokens    oauth2.TokenSource
}

// WithProvider compiles p in under name: channels use it as
// {provider: {name: <name>}}.
func WithProvider(name string, p provider.Provider) Option {
	return func(o *options) { o.providers[name] = p }
}

// WithTokenSource authorizes every FCM channel with ts instead of its
// credentials (tests, a sidecar issuing tokens).
func WithTokenSource(ts oauth2.TokenSource) Option { return func(o *options) { o.tokens = ts } }

// State is the service's tree.
type State struct {
	Channels *channels.Channels
	Courier  *core.Courier
	Admin    *admin.Admin
	plugins  []*plugin.Client
}

// Main runs courier until a signal stops it, exiting the process on error.
func Main(opts ...Option) {
	if err := Run(context.Background(), opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Run runs courier until ctx ends or a signal stops it.
func Run(ctx context.Context, opts ...Option) error {
	o := options{providers: map[string]provider.Provider{}}
	for _, opt := range opts {
		opt(&o)
	}

	svc, err := backplane.Open(ctx, func(root backplane.Root[Config]) (*State, error) { return newState(ctx, root, o) },
		standard.Drivers())
	if err != nil {
		return fmt.Errorf("courier: %w", err)
	}

	svc.Internal(func(r grpc.ServiceRegistrar) { courierconsolev1.RegisterAdminServiceServer(r, svc.State().Admin) })

	if bundle := ui.Bundle(); bundle != nil {
		svc.UI(bundle)
	}

	if err := svc.Run(ctx); err != nil {
		return fmt.Errorf("courier: %w", err)
	}

	return nil
}

func newState(ctx context.Context, root backplane.Root[Config], o options) (*State, error) {
	cfg := root.Config()
	st := &State{}

	extensions, err := st.extensions(ctx, root, cfg, o)
	if err != nil {
		return nil, err
	}

	st.Channels, err = channels.New(root, cfg.Channels, channels.Options{ //nolint:contextcheck // builds, does not call
		Extensions: extensions, Defaults: cfg.Defaults, TokenSource: o.tokens,
	})
	if err != nil {
		return nil, fmt.Errorf("courier: %w", err)
	}

	set := core.Settings{
		Limits: mail.Limits{MaxBytes: cfg.MaxMessageBytes, MaxRecipients: int(cfg.MaxRecipients)}, DryRun: cfg.DryRun,
	}

	if st.Courier, err = core.New(root, st.Channels, set, used(cfg.Channels, extensions)); err != nil {
		return nil, fmt.Errorf("courier: %w", err)
	}

	st.Admin = admin.New(root, st.Channels, cfg.DryRun)

	return st, nil
}

// extensions are the compiled-in providers and the plugins, described.
func (st *State) extensions(
	ctx context.Context, root backplane.Root[Config], cfg Config, o options,
) (map[string]*channels.Extension, error) {
	out := map[string]*channels.Extension{}

	add := func(name string, p provider.Provider, isPlugin bool) error {
		if _, dup := out[name]; dup {
			return fmt.Errorf("%w: %q", ErrProviderName, name)
		}

		dctx, cancel := context.WithTimeout(ctx, describeTimeout)
		defer cancel()

		d, err := p.Describe(dctx)
		if err != nil {
			return fmt.Errorf("courier: provider %q: %w", name, err)
		}

		out[name] = &channels.Extension{Name: name, Provider: p, Descriptor: d, Plugin: isPlugin}

		return nil
	}

	for name, p := range o.providers {
		if err := add(name, p, false); err != nil {
			return nil, err
		}
	}

	for name, spec := range cfg.Plugins {
		client, err := st.dial(root, name, spec)
		if err != nil {
			return nil, err
		}

		dctx, cancel := context.WithTimeout(ctx, spec.StartupTimeout)
		d, err := client.WaitDescribe(dctx)

		cancel()

		if err != nil {
			return nil, fmt.Errorf("courier: plugin %q did not answer within %s: %w", name, spec.StartupTimeout, err)
		}

		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: %q", ErrProviderName, name)
		}

		dep := pluginDependency{name: name, client: client, want: d}
		deps.NewDependency[*plugin.Client](root, dep)

		out[name] = &channels.Extension{Name: name, Provider: client, Descriptor: d, Plugin: true, Health: dep.check}
	}

	return out, nil
}

// dial connects a plugin and makes it a required dependency: its gRPC
// health is part of courier's readiness, its connection closed at stop.
func (st *State) dial(_ backplane.Root[Config], name string, spec channels.Plugin) (*plugin.Client, error) {
	var opts []plugin.DialOption

	if spec.TLS.Enabled {
		tlsCfg, err := spec.TLS.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("courier: plugin %q: tls: %w", name, err)
		}

		opts = append(opts, plugin.WithTLS(tlsCfg))
	}

	client, err := plugin.Dial(spec.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("courier: plugin %q: %w", name, err)
	}

	st.plugins = append(st.plugins, client)

	return client, nil
}

// pluginDependency is a dialed plugin as a dependency of courier: ready
// while its gRPC health serves and it still describes the contract
// courier declared its activity with.
type pluginDependency struct {
	name   string
	client *plugin.Client
	want   provider.Descriptor
}

// ErrContractChanged: the plugin now describes another kind or other
// schemas than courier declared at startup.
var ErrContractChanged = errors.New("changed its contract since courier started: restart courier to take it")

// check is the plugin's health and contract.
func (p pluginDependency) check(ctx context.Context) error {
	if err := p.client.Health(ctx); err != nil {
		return err //nolint:wrapcheck // names the plugin
	}

	got, err := p.client.Describe(ctx)
	if err != nil {
		return err //nolint:wrapcheck // names the plugin
	}

	if got.Kind != p.want.Kind || !proto.Equal(got.Message, p.want.Message) || !proto.Equal(got.Result, p.want.Result) ||
		!proto.Equal(got.Settings, p.want.Settings) {
		return fmt.Errorf("plugin %q (%s) %w", p.name, p.client.Target(), ErrContractChanged)
	}

	return nil
}

func (p pluginDependency) Name() string { return "plugin-" + p.name }

func (p pluginDependency) Provide(context.Context, deps.Scope) (*plugin.Client, error) {
	return p.client, nil
}

func (p pluginDependency) Probe(ctx context.Context, _ *plugin.Client) error { return p.check(ctx) }

func (p pluginDependency) Close(context.Context, *plugin.Client) error {
	return p.client.Close() //nolint:wrapcheck // gRPC's own error
}

// used are the extensions some channel uses: an activity is declared for
// a kind the installation delivers, not for every provider compiled in.
func used(cfg channels.Config, all map[string]*channels.Extension) map[string]*channels.Extension {
	out := map[string]*channels.Extension{}

	for _, spec := range cfg {
		if spec.Provider != nil {
			if ext, ok := all[spec.Provider.Name]; ok {
				out[ext.Name] = ext
			}
		}
	}

	return out
}
