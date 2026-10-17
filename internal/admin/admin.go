// Package admin is courier's internal API: the console module's channel
// list and check, served on the platform port behind the internal secret.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane"
	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/backplane-labs/backplane/pkg/backplane/deps"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/gopherex/xlog"

	"github.com/backplane-labs/backplane-courier/internal/channels"
	"github.com/backplane-labs/backplane-courier/internal/courier"
	"github.com/backplane-labs/backplane-courier/internal/webpush"
	courierconsolev1 "github.com/backplane-labs/backplane-courier/proto/courier/console/v1"
	"github.com/backplane-labs/backplane-courier/provider"
)

// healthTimeout bounds asking a plugin how it is, per channel listed.
const healthTimeout = 2 * time.Second

// Admin serves AdminService.
type Admin struct {
	courierconsolev1.UnimplementedAdminServiceServer
	deps.Component

	channels *channels.Channels
	dryRun   config.Live[bool]
}

// New creates the admin component under parent.
func New(parent deps.Scope, chs *channels.Channels, dryRun config.Live[bool]) *Admin {
	return &Admin{Component: deps.NewComponent(parent, "admin"), channels: chs, dryRun: dryRun}
}

// ListChannels implements AdminService.
func (a *Admin) ListChannels(
	ctx context.Context, _ *courierconsolev1.ListChannelsRequest,
) (*courierconsolev1.ListChannelsResponse, error) {
	out := &courierconsolev1.ListChannelsResponse{DryRun: a.dryRun.Get()}

	for _, ch := range a.channels.All() {
		item := &courierconsolev1.Channel{
			Name: ch.Name, Kind: ch.Kind, Activity: activityOf(ch.Kind), Details: details(ch),
			Pool:          int32(min(ch.Spec.Pool, math.MaxInt32)),          //nolint:gosec // bounded
			RatePerMinute: int32(min(ch.Spec.RatePerMinute, math.MaxInt32)), //nolint:gosec // bounded
			Timeout:       durationpb.New(ch.Spec.Timeout), Default: a.channels.IsDefault(ch),
		}

		if ch.Extension != nil && ch.Extension.Plugin {
			item.Plugin = ch.Provider

			if ch.Extension.Health != nil {
				hctx, cancel := context.WithTimeout(ctx, healthTimeout)
				if err := ch.Extension.Health(hctx); err != nil {
					item.Problem = err.Error()
				}

				cancel()
			}
		}

		out.Channels = append(out.Channels, item)
	}

	list := out.GetChannels()
	sort.Slice(list, func(i, j int) bool { return list[i].GetName() < list[j].GetName() })

	return out, nil
}

func activityOf(kind string) string {
	switch kind {
	case channels.KindMail:
		return "SendMail"
	case channels.KindPush:
		return "SendPush"
	case channels.KindWebPush:
		return "SendWebPush"
	default:
		return courier.ActivityName(kind)
	}
}

// details are what the console may show: never a secret.
func details(ch *channels.Channel) map[string]string {
	s := ch.Spec

	switch {
	case s.SMTP != nil:
		auth := "none"
		if s.SMTP.Username != "" {
			auth = s.SMTP.Username
		}

		return map[string]string{
			"host": fmt.Sprintf("%s:%d", s.SMTP.Host, s.SMTP.Port), "tls": s.SMTP.TLS,
			"auth": auth, "from": s.SMTP.From,
		}
	case s.FCM != nil:
		d := map[string]string{"project": s.FCM.Project()}
		if s.FCM.Endpoint != "" {
			d["endpoint"] = s.FCM.Endpoint
		}

		return d
	case s.WebPush != nil:
		hosts := "the browsers' push services"
		if len(s.WebPush.Hosts) > 0 {
			hosts = strings.Join(s.WebPush.Hosts, ", ")
		}

		return map[string]string{"subject": s.WebPush.Subject, "public_key": s.WebPush.PublicKey, "hosts": hosts}
	case s.Log != nil:
		return map[string]string{"log": "deliveries go to the service log"}
	case s.Provider != nil:
		d := map[string]string{"provider": s.Provider.Name}
		for k, v := range s.Provider.Settings {
			d[k] = v
		}

		for k := range s.Provider.Secrets {
			d[k] = "***"
		}

		return d
	}

	return nil
}

// CheckChannel implements AdminService.
func (a *Admin) CheckChannel(
	ctx context.Context, req *courierconsolev1.CheckChannelRequest,
) (*courierconsolev1.CheckChannelResponse, error) {
	ch, ok := a.channels.Get(req.GetName())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "channel %q is not configured", req.GetName())
	}

	ctx, cancel := context.WithTimeout(ctx, ch.Timeout())
	defer cancel()

	started := time.Now()

	var err error

	switch {
	case ch.Mail != nil:
		err = ch.Mail.Check(ctx)
	case ch.Push != nil:
		err = ch.Push.Check(ctx)
	case ch.WebPush != nil:
		err = ch.WebPush.Check(ctx)
	case ch.Extension != nil:
		err = ch.Extension.Provider.Check(ctx, provider.Channel{Name: ch.Name, Settings: ch.Settings})
	}

	out := &courierconsolev1.CheckChannelResponse{Ok: err == nil, Took: durationpb.New(time.Since(started))}
	if err != nil {
		out.Error = err.Error()
	}

	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}

	a.Log().Ctx().Info(ctx, "courier.channel.checked",
		backplane.Audit(), xlog.String("event.name", "courier.channel.checked"),
		xlog.String(backplane.AuditID, uuid.NewString()), xlog.String(backplane.AuditSubject, "channel/"+ch.Name),
		xlog.String(backplane.AuditActor, "console"), xlog.String(backplane.AuditOutcome, outcome),
		xlog.String("error", out.GetError()))

	return out, nil
}

// GenerateWebPushKeys implements AdminService.
func (a *Admin) GenerateWebPushKeys(
	context.Context, *courierconsolev1.GenerateWebPushKeysRequest,
) (*courierconsolev1.GenerateWebPushKeysResponse, error) {
	public, private, err := webpush.GenerateKeys()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error()) //nolint:wrapcheck // a gRPC status
	}

	return &courierconsolev1.GenerateWebPushKeysResponse{PublicKey: public, PrivateKey: private}, nil
}

// TestWebPush implements AdminService.
func (a *Admin) TestWebPush(
	ctx context.Context, req *courierconsolev1.TestWebPushRequest,
) (*courierconsolev1.TestWebPushResponse, error) {
	ch, ok := a.channels.Get(req.GetChannel())
	if !ok || ch.Kind != channels.KindWebPush {
		return nil, status.Errorf(codes.NotFound, "webpush channel %q is not configured", req.GetChannel())
	}

	msg := webpush.Message{Payload: json.RawMessage(req.GetPayload())}
	if err := json.Unmarshal([]byte(req.GetSubscription()), &msg.Subscription); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "subscription: %v", err)
	}

	var hosts []string
	if ch.WebPush != nil {
		hosts = ch.WebPush.Hosts()
	}

	if err := msg.Validate(hosts); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error()) //nolint:wrapcheck // a gRPC status
	}

	ctx, cancel := context.WithTimeout(ctx, ch.Timeout())
	defer cancel()

	started := time.Now()

	var (
		id  string
		err error
	)

	if ch.WebPush == nil {
		id = "logged"
	} else {
		id, err = ch.WebPush.Send(ctx, msg)
	}

	out := &courierconsolev1.TestWebPushResponse{Ok: err == nil, Id: id, Took: durationpb.New(time.Since(started))}

	outcome := "succeeded"
	if err != nil {
		outcome, out.Error = "failed", err.Error()

		var pe *provider.Error
		if errors.As(err, &pe) {
			out.Code = pe.Code
		}
	}

	a.Log().Ctx().Info(ctx, "courier.webpush.tested",
		backplane.Audit(), xlog.String("event.name", "courier.webpush.tested"),
		xlog.String(backplane.AuditID, uuid.NewString()), xlog.String(backplane.AuditSubject, "channel/"+ch.Name),
		xlog.String(backplane.AuditActor, "console"), xlog.String(backplane.AuditOutcome, outcome),
		xlog.String("target", msg.Target()), xlog.String("code", out.GetCode()))

	return out, nil
}

var _ courierconsolev1.AdminServiceServer = (*Admin)(nil)
