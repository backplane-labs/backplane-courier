// Package plugin connects courier and a provider running as its own
// service over courier.plugin.v1.ProviderService. A plugin written in Go
// implements provider.Provider and calls Serve; one in another language
// implements the service from proto/courier/plugin/v1. Courier dials a
// plugin with Dial; where and how the plugin runs (a sidecar, another
// container, a process the image starts) is the deployment's concern.
package plugin

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/durationpb"

	pluginv1 "github.com/backplane-labs/backplane-courier/proto/courier/plugin/v1"
	"github.com/backplane-labs/backplane-courier/provider"
)

// ServeOption configures Serve and ServeListener.
type ServeOption func(*serveOptions)

type serveOptions struct {
	grpc []grpc.ServerOption
}

// WithServerTLS serves over TLS (mTLS when cfg requires client
// certificates); courier dials with its plugin's tls settings.
func WithServerTLS(cfg *tls.Config) ServeOption {
	return func(o *serveOptions) { o.grpc = append(o.grpc, grpc.Creds(credentials.NewTLS(cfg))) }
}

// WithServerOptions adds gRPC server options (interceptors, limits).
func WithServerOptions(opts ...grpc.ServerOption) ServeOption {
	return func(o *serveOptions) { o.grpc = append(o.grpc, opts...) }
}

// Serve runs p as a plugin on address (unix:///path or host:port) until
// SIGINT or SIGTERM.
func Serve(address string, p provider.Provider, opts ...ServeOption) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	network := "tcp"
	if path, ok := strings.CutPrefix(address, "unix://"); ok {
		network, address = "unix", path
	}

	lis, err := (&net.ListenConfig{}).Listen(ctx, network, address)
	if err != nil {
		return fmt.Errorf("plugin: listen: %w", err)
	}

	return ServeListener(ctx, lis, p, opts...)
}

// ServeListener serves p on lis until ctx ends, with the standard gRPC
// health service: SERVING while it runs, NOT_SERVING once it stops.
func ServeListener(ctx context.Context, lis net.Listener, p provider.Provider, opts ...ServeOption) error {
	var o serveOptions
	for _, opt := range opts {
		opt(&o)
	}

	o.grpc = append(o.grpc, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime: keepaliveTime / 2, PermitWithoutStream: true, //nolint:mnd // tolerate courier's pings
	}))

	srv := grpc.NewServer(o.grpc...)
	pluginv1.RegisterProviderServiceServer(srv, &server{p: p})

	status := health.NewServer()
	status.SetServingStatus(ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, status)

	go func() {
		<-ctx.Done()
		status.Shutdown()
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("plugin: serve: %w", err)
	}

	return nil
}

// server adapts a Provider to the gRPC service.
type server struct {
	pluginv1.UnimplementedProviderServiceServer

	p provider.Provider
}

func (s *server) Describe(ctx context.Context, _ *pluginv1.DescribeRequest) (*pluginv1.DescribeResponse, error) {
	d, err := s.p.Describe(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // the provider's own error, as a status
	}

	return &pluginv1.DescribeResponse{
		Kind: d.Kind, Description: d.Description, Settings: d.Settings, Message: d.Message, Result: d.Result,
	}, nil
}

func (s *server) Check(ctx context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckResponse, error) {
	if err := s.p.Check(ctx, channelOf(req.GetChannel())); err != nil {
		return &pluginv1.CheckResponse{Failure: failureOf(err)}, nil
	}

	return &pluginv1.CheckResponse{}, nil
}

func (s *server) Send(ctx context.Context, req *pluginv1.SendRequest) (*pluginv1.SendResponse, error) {
	out, err := s.p.Send(ctx, provider.Delivery{
		Channel: channelOf(req.GetChannel()), Message: req.GetMessage(),
		IdempotencyKey: req.GetIdempotencyKey(), Attempt: int(req.GetAttempt()),
	})
	if err != nil {
		return &pluginv1.SendResponse{Outcome: &pluginv1.SendResponse_Failure{Failure: failureOf(err)}}, nil
	}

	return &pluginv1.SendResponse{Outcome: &pluginv1.SendResponse_Result{Result: out}}, nil
}

func channelOf(c *pluginv1.Channel) provider.Channel {
	return provider.Channel{Name: c.GetName(), Settings: c.GetSettings()}
}

func failureOf(err error) *pluginv1.Failure {
	var pe *provider.Error
	if errors.As(err, &pe) {
		f := &pluginv1.Failure{Message: pe.Message, Code: pe.Code, Permanent: pe.Permanent}
		if pe.RetryAfter > 0 {
			f.RetryAfter = durationpb.New(pe.RetryAfter)
		}

		return f
	}

	return &pluginv1.Failure{Message: err.Error()}
}

// ErrNotServing: the plugin's health is not SERVING.
var ErrNotServing = errors.New("not serving")

// ServiceName is the plugin service's name in the gRPC health protocol.
const ServiceName = "courier.plugin.v1.ProviderService"

// keepaliveTime: courier pings an idle connection this often, so a plugin
// gone away is noticed before a delivery needs it.
const (
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 10 * time.Second
)

// Client is a plugin as a provider.Provider.
type Client struct {
	target string
	conn   *grpc.ClientConn
	api    pluginv1.ProviderServiceClient
	health healthpb.HealthClient
}

// DialOption configures Dial.
type DialOption func(*[]grpc.DialOption)

// WithTLS dials over TLS (mTLS when cfg carries a client certificate).
func WithTLS(cfg *tls.Config) DialOption {
	return func(o *[]grpc.DialOption) { *o = append(*o, grpc.WithTransportCredentials(credentials.NewTLS(cfg))) }
}

// WithDialOptions adds gRPC dial options (interceptors, limits).
func WithDialOptions(opts ...grpc.DialOption) DialOption {
	return func(o *[]grpc.DialOption) { *o = append(*o, opts...) }
}

// Dial prepares a connection to a plugin: target is "unix:///path" or
// "host:port". Nothing connects until the first call; gRPC reconnects with
// backoff whenever the plugin goes away, and keepalive pings notice a dead
// peer. Without WithTLS the connection is plaintext: keep such a plugin
// next to courier (a sidecar, a unix socket on a shared volume).
func Dial(target string, opts ...DialOption) (*Client, error) {
	if !strings.Contains(target, "://") && strings.HasPrefix(target, "/") {
		target = "unix://" + target
	}

	dial := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: keepaliveTime, Timeout: keepaliveTimeout, PermitWithoutStream: true,
		}),
	}
	for _, opt := range opts {
		opt(&dial)
	}

	conn, err := grpc.NewClient(target, dial...)
	if err != nil {
		return nil, fmt.Errorf("plugin: dial %s: %w", target, err)
	}

	return &Client{
		target: target, conn: conn, api: pluginv1.NewProviderServiceClient(conn), health: healthpb.NewHealthClient(conn),
	}, nil
}

// Target is the address dialed.
func (c *Client) Target() string { return c.target }

// Health is the plugin's gRPC health: nil while it serves.
func (c *Client) Health(ctx context.Context) error {
	res, err := c.health.Check(ctx, &healthpb.HealthCheckRequest{Service: ServiceName})
	if err != nil {
		return fmt.Errorf("plugin %s: health: %w", c.target, err)
	}

	if res.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("plugin %s: %w: %s", c.target, ErrNotServing, res.GetStatus())
	}

	return nil
}

// WaitDescribe is Describe that waits for the plugin to come up, until
// ctx ends: courier's startup, when the plugin may start after it.
func (c *Client) WaitDescribe(ctx context.Context) (provider.Descriptor, error) {
	d, err := c.api.Describe(ctx, &pluginv1.DescribeRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return provider.Descriptor{}, fmt.Errorf("plugin %s: describe: %w", c.target, err)
	}

	return descriptor(d), nil
}

// Close the connection.
func (c *Client) Close() error { return c.conn.Close() } //nolint:wrapcheck // gRPC's own error

// Describe implements provider.Provider.
func (c *Client) Describe(ctx context.Context) (provider.Descriptor, error) {
	d, err := c.api.Describe(ctx, &pluginv1.DescribeRequest{})
	if err != nil {
		return provider.Descriptor{}, fmt.Errorf("plugin: describe: %w", err)
	}

	return descriptor(d), nil
}

func descriptor(d *pluginv1.DescribeResponse) provider.Descriptor {
	return provider.Descriptor{
		Kind: d.GetKind(), Description: d.GetDescription(), Settings: d.GetSettings(), Message: d.GetMessage(),
		Result: d.GetResult(),
	}
}

// Check implements provider.Provider.
func (c *Client) Check(ctx context.Context, ch provider.Channel) error {
	res, err := c.api.Check(ctx, &pluginv1.CheckRequest{Channel: &pluginv1.Channel{Name: ch.Name, Settings: ch.Settings}})
	if err != nil {
		return fmt.Errorf("plugin: check: %w", err)
	}

	return errorOf(res.GetFailure())
}

// Send implements provider.Provider.
func (c *Client) Send(ctx context.Context, d provider.Delivery) ([]byte, error) {
	res, err := c.api.Send(ctx, &pluginv1.SendRequest{
		Channel: &pluginv1.Channel{Name: d.Channel.Name, Settings: d.Channel.Settings}, Message: d.Message,
		IdempotencyKey: d.IdempotencyKey, Attempt: int32(min(d.Attempt, math.MaxInt32)), //nolint:gosec // bounded
	})
	if err != nil {
		return nil, fmt.Errorf("plugin: send: %w", err)
	}

	if f := res.GetFailure(); f != nil {
		return nil, errorOf(f)
	}

	return res.GetResult(), nil
}

func errorOf(f *pluginv1.Failure) error {
	if f == nil {
		return nil
	}

	return &provider.Error{
		Message: f.GetMessage(), Code: f.GetCode(), Permanent: f.GetPermanent(), RetryAfter: f.GetRetryAfter().AsDuration(),
	}
}

var _ provider.Provider = (*Client)(nil)
