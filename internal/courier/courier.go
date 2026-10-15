// Package courier is the service's work: an activity per kind of
// delivery — SendMail, SendPush (with Subscribe and Unsubscribe for FCM
// topics) and Send<Kind> for every extension provider — each delivering
// at once through the channel the message names or the kind's default.
// It keeps nothing: a temporary failure goes back to the caller, whose
// retry policy decides; a final one is NonRetryable; a provider's
// Retry-After becomes activity.RetryAfter. Every outcome is an audit
// record.
package courier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/backplane-labs/backplane/pkg/backplane"
	"github.com/backplane-labs/backplane/pkg/backplane/activity"
	"github.com/backplane-labs/backplane/pkg/backplane/config"
	"github.com/backplane-labs/backplane/pkg/backplane/deps"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"

	sp "github.com/gopherex/schemapb/go/schemapb"
	"github.com/gopherex/xlog"

	"github.com/backplane-labs/backplane-courier/internal/channels"
	"github.com/backplane-labs/backplane-courier/internal/fcm"
	"github.com/backplane-labs/backplane-courier/internal/mail"
	"github.com/backplane-labs/backplane-courier/internal/push"
	"github.com/backplane-labs/backplane-courier/provider"
)

const sendTimeout = time.Minute

// logFrom is the sender of a mail a log channel takes without one.
const logFrom = "courier <no-reply@localhost>"

// Errors of a subscription call; final.
var (
	ErrTooManyTokens = errors.New("courier: at most 1000 tokens per call")
	ErrTopicRequest  = errors.New("courier: a topic and tokens are required")
	// ErrDuplicateKind: two providers in use deliver the same kind.
	ErrDuplicateKind = errors.New("courier: two providers of one kind")
)

// Settings of the courier.
type Settings struct {
	Limits mail.Limits
	// Live: accept and record, deliver nothing.
	DryRun config.Live[bool]
}

// Payloads of the activities.
type (
	// MailResult is SendMail's answer.
	MailResult struct {
		Channel   string `json:"channel"`
		MessageID string `json:"message_id"        schemapb:"description=The Message-ID header of the mail"`
		DryRun    bool   `json:"dry_run,omitempty"`
	}
	// PushResult is SendPush's answer.
	PushResult struct {
		Channel string `json:"channel"`
		Name    string `json:"name"              schemapb:"description=FCM's message name"`
		DryRun  bool   `json:"dry_run,omitempty"`
	}
	// TopicRequest changes a topic's subscriptions.
	TopicRequest struct {
		Channel string   `json:"channel,omitempty" schemapb:"description=Push channel, else the default"`
		Topic   string   `json:"topic"`
		Tokens  []string `json:"tokens"            schemapb:"description=Device tokens, at most 1000"`
	}
	// TopicResult is every token's outcome.
	TopicResult struct {
		Channel string            `json:"channel"`
		Results []fcm.TokenResult `json:"results"`
	}
)

// Courier is the component.
type Courier struct {
	deps.Component

	set      Settings
	channels *channels.Channels

	deliveries metric.Int64Counter
	duration   metric.Float64Histogram
}

// New declares the activities under parent: the built-in ones, and
// Send<Kind> for every extension kind among the channels.
func New(
	parent deps.Scope, chs *channels.Channels, set Settings, extensions map[string]*channels.Extension,
) (*Courier, error) {
	c := &Courier{Component: deps.NewComponent(parent, "courier"), set: set, channels: chs}

	activity.Handle(c, "SendMail", c.SendMail, activity.StartToClose(sendTimeout),
		activity.Describe("delivers a rendered mail through a mail channel of the installation"))
	activity.Handle(c, "SendPush", c.SendPush, activity.StartToClose(sendTimeout),
		activity.Describe("delivers a push to a device, a topic or a condition through a push channel"))
	activity.Handle(c, "Subscribe", c.Subscribe, activity.StartToClose(sendTimeout),
		activity.Describe("subscribes device tokens to an FCM topic"))
	activity.Handle(c, "Unsubscribe", c.Unsubscribe, activity.StartToClose(sendTimeout),
		activity.Describe("unsubscribes device tokens from an FCM topic"))

	declared := map[string]bool{}

	for _, ext := range extensions {
		kind := ext.Descriptor.Kind
		if declared[kind] {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKind, kind)
		}

		declared[kind] = true

		in, err := withChannel(ext.Descriptor.Message)
		if err != nil {
			return nil, fmt.Errorf("courier: provider %s: %w", ext.Name, err)
		}

		description := ext.Descriptor.Description
		if description == "" {
			description = "delivers a " + kind + " message through a " + kind + " channel"
		}

		activity.HandleSchema(c, ActivityName(kind), in, ext.Descriptor.Result, c.extension(kind),
			activity.StartToClose(sendTimeout), activity.Describe(description))
	}

	var err error

	c.deliveries, err = c.Meter().Int64Counter("courier.deliveries",
		metric.WithDescription("Deliveries by channel, kind and outcome"))
	if err != nil {
		return nil, fmt.Errorf("courier metrics: %w", err)
	}

	c.duration, err = c.Meter().Float64Histogram("courier.delivery.duration", metric.WithUnit("s"),
		metric.WithDescription("Time a provider took to take a delivery"))
	if err != nil {
		return nil, fmt.Errorf("courier metrics: %w", err)
	}

	return c, nil
}

// ActivityName of an extension kind: Send<Kind> in CamelCase.
func ActivityName(kind string) string {
	var b strings.Builder

	b.WriteString("Send")

	for _, part := range strings.Split(kind, "_") {
		if part == "" {
			continue
		}

		r := []rune(part)
		b.WriteRune(unicode.ToUpper(r[0]))
		b.WriteString(string(r[1:]))
	}

	return b.String()
}

// withChannel is the message schema with an optional channel field.
func withChannel(message *sp.Schema) (*sp.Schema, error) {
	type pick struct {
		Channel string `json:"channel,omitempty" schemapb:"description=Channel of the installation, else the default"`
	}

	field, err := sp.ReflectType[pick](sp.ID(sp.Namespace("courier"), sp.SchemaName("pick"), sp.Ver(1, 0, 0)))
	if err != nil {
		return nil, fmt.Errorf("channel field: %w", err)
	}

	if message == nil {
		return nil, nil //nolint:nilnil // no schema: plain JSON
	}

	for _, f := range message.GetFields() {
		if f.GetName() == "channel" {
			return nil, errors.New("the message schema has its own channel field") //nolint:err113 // one message
		}
	}

	out, _ := proto.Clone(message).(*sp.Schema)
	out.Fields = append(out.Fields, field.GetFields()...)

	return out, nil
}

// SendMail is the courier.SendMail activity.
func (c *Courier) SendMail(ctx context.Context, msg mail.Message) (MailResult, error) {
	if err := msg.Validate(c.set.Limits); err != nil {
		return MailResult{}, activity.NonRetryable(err) //nolint:wrapcheck // final
	}

	ch, err := c.pick(channels.KindMail, msg.Channel)
	if err != nil {
		return MailResult{}, err
	}

	id := uuid.Must(uuid.NewV7()).String()
	target := "to:" + strings.Join(mail.Hashes(msg.Recipients()), ",")

	from := logFrom
	if ch.Mail != nil {
		from = ch.Mail.From()
	}

	built, err := mail.Build(msg, from, id)
	if err != nil {
		return MailResult{}, activity.NonRetryable(err) //nolint:wrapcheck // final
	}

	out := MailResult{Channel: ch.Name, MessageID: strings.Trim(built.GetMessageID(), "<>"), DryRun: c.set.DryRun.Get()}

	err = c.deliver(ctx, ch, target, func(ctx context.Context) (string, error) {
		if ch.Mail == nil || out.DryRun {
			c.logged(ctx, ch, target, "subject", msg.Subject)

			return out.MessageID, nil
		}

		return out.MessageID, ch.Mail.Send(ctx, built)
	})

	return out, err
}

// SendPush is the courier.SendPush activity.
func (c *Courier) SendPush(ctx context.Context, msg push.Message) (PushResult, error) {
	if err := msg.Validate(); err != nil {
		return PushResult{}, activity.NonRetryable(err) //nolint:wrapcheck // final
	}

	ch, err := c.pick(channels.KindPush, msg.Channel)
	if err != nil {
		return PushResult{}, err
	}

	body, err := push.Build(msg)
	if err != nil {
		return PushResult{}, activity.NonRetryable(err) //nolint:wrapcheck // final
	}

	out := PushResult{Channel: ch.Name, DryRun: c.set.DryRun.Get()}

	err = c.deliver(ctx, ch, msg.Target(), func(ctx context.Context) (string, error) {
		if ch.Push == nil || out.DryRun {
			c.logged(ctx, ch, msg.Target(), "title", msg.Title)

			out.Name = "logged/" + uuid.NewString()

			return out.Name, nil
		}

		name, err := ch.Push.Send(ctx, body)
		out.Name = name

		return name, err //nolint:wrapcheck // classified by deliver
	})

	return out, err
}

// Subscribe is the courier.Subscribe activity.
func (c *Courier) Subscribe(ctx context.Context, req TopicRequest) (TopicResult, error) {
	return c.topic(ctx, req, "subscribe", func(ctx context.Context, cl *fcm.Client) ([]fcm.TokenResult, error) {
		return cl.Subscribe(ctx, req.Topic, req.Tokens)
	})
}

// Unsubscribe is the courier.Unsubscribe activity.
func (c *Courier) Unsubscribe(ctx context.Context, req TopicRequest) (TopicResult, error) {
	return c.topic(ctx, req, "unsubscribe", func(ctx context.Context, cl *fcm.Client) ([]fcm.TokenResult, error) {
		return cl.Unsubscribe(ctx, req.Topic, req.Tokens)
	})
}

func (c *Courier) topic(
	ctx context.Context, req TopicRequest, operation string,
	call func(context.Context, *fcm.Client) ([]fcm.TokenResult, error),
) (TopicResult, error) {
	switch {
	case len(req.Tokens) > fcm.MaxBatch:
		return TopicResult{}, activity.NonRetryable(ErrTooManyTokens) //nolint:wrapcheck // final
	case len(req.Tokens) == 0 || req.Topic == "":
		return TopicResult{}, activity.NonRetryable(ErrTopicRequest) //nolint:wrapcheck // final
	}

	if err := (push.Message{Topic: req.Topic}).Validate(); err != nil {
		return TopicResult{}, activity.NonRetryable(err) //nolint:wrapcheck // final
	}

	ch, err := c.pick(channels.KindPush, req.Channel)
	if err != nil {
		return TopicResult{}, err
	}

	out := TopicResult{Channel: ch.Name}

	err = c.deliver(ctx, ch, "topic:"+req.Topic, func(ctx context.Context) (string, error) {
		if ch.Push == nil || c.set.DryRun.Get() {
			c.logged(ctx, ch, "topic:"+req.Topic, operation, strconv.Itoa(len(req.Tokens)))

			for _, t := range req.Tokens {
				out.Results = append(out.Results, fcm.TokenResult{Token: t})
			}

			return operation, nil
		}

		results, err := call(ctx, ch.Push)
		out.Results = results

		return operation, err
	})

	return out, err
}

// extension is the handler of Send<Kind>: the input minus channel goes to
// the provider of the channel picked.
func (c *Courier) extension(kind string) func(ctx context.Context, in []byte) ([]byte, error) {
	return func(ctx context.Context, in []byte) ([]byte, error) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(in, &fields); err != nil {
			return nil, activity.NonRetryable(fmt.Errorf("courier: message: %w", err))
		}

		var name string
		if raw, ok := fields["channel"]; ok {
			_ = json.Unmarshal(raw, &name)

			delete(fields, "channel")
		}

		ch, err := c.pick(kind, name)
		if err != nil {
			return nil, err
		}

		message, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("courier: message: %w", err)
		}

		info, _ := activity.InfoOf(ctx)

		var result []byte

		err = c.deliver(ctx, ch, kind, func(ctx context.Context) (string, error) {
			if ch.Extension == nil || c.set.DryRun.Get() {
				c.logged(ctx, ch, kind, "bytes", strconv.Itoa(len(message)))

				result = []byte(`{"id":"logged"}`)

				return "logged", nil
			}

			out, err := ch.Extension.Provider.Send(ctx, provider.Delivery{
				Channel: provider.Channel{Name: ch.Name, Settings: ch.Settings}, Message: message,
				IdempotencyKey: info.Key, Attempt: int(info.Attempt),
			})
			result = out

			return "", err //nolint:wrapcheck // classified by deliver
		})
		if err != nil {
			return nil, err
		}

		if len(result) == 0 {
			result = []byte("{}")
		}

		return result, nil
	}
}

// pick is the channel of kind, or a final error.
func (c *Courier) pick(kind, name string) (*channels.Channel, error) {
	ch, err := c.channels.Pick(kind, name)
	if err != nil {
		return nil, activity.NonRetryable(err) //nolint:wrapcheck // final
	}

	return ch, nil
}

// deliver runs send within the channel's pool, rate and timeout, and
// turns its error into the activity's: final, later, or retried.
func (c *Courier) deliver(
	ctx context.Context, ch *channels.Channel, target string, send func(context.Context) (string, error),
) error {
	release, err := ch.Acquire(ctx)
	if err != nil {
		return err //nolint:wrapcheck // the context's or the rate's
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, ch.Timeout())
	defer cancel()

	start := time.Now()
	id, err := send(ctx)
	took := time.Since(start)

	outcome := "delivered"
	if err != nil {
		outcome = "failed"
	}

	c.record(ctx, ch, target, id, outcome, took, err)

	return classify(err)
}

// classify maps a provider error to the activity's.
func classify(err error) error {
	if err == nil {
		return nil
	}

	var refusal *provider.Error
	if !errors.As(err, &refusal) {
		return err
	}

	switch {
	case refusal.Permanent:
		return activity.NonRetryable(err) //nolint:wrapcheck // final
	case refusal.RetryAfter > 0:
		return activity.RetryAfter(err, refusal.RetryAfter) //nolint:wrapcheck // later
	default:
		return err
	}
}

func (c *Courier) logged(ctx context.Context, ch *channels.Channel, target, key, value string) {
	c.Log().Ctx().Info(ctx, "delivery logged, not sent", xlog.String("channel", ch.Name), xlog.String("kind", ch.Kind),
		xlog.String("target", target), xlog.String(key, value), xlog.Bool("dry_run", c.set.DryRun.Get()))
}

// record is the delivery's metrics and audit record: facts only, never a
// payload; recipients and tokens are hashed.
func (c *Courier) record(
	ctx context.Context, ch *channels.Channel, target, id, outcome string, took time.Duration, err error,
) {
	attrs := metric.WithAttributes(attribute.String("channel", ch.Name), attribute.String("kind", ch.Kind),
		attribute.String("outcome", outcome))
	c.deliveries.Add(ctx, 1, attrs)
	c.duration.Record(ctx, took.Seconds(), attrs)

	info, _ := activity.InfoOf(ctx)

	caller := info.Binding
	if caller == "" {
		caller = "courier"
	}

	key := info.Key
	if key == "" {
		key = uuid.NewString()
	}

	fields := []xlog.Field{
		backplane.Audit(), xlog.String("event.name", "courier."+outcome),
		xlog.String(backplane.AuditID, key+"/"+strconv.Itoa(int(info.Attempt))),
		xlog.String(backplane.AuditSubject, ch.Kind+"/"+ch.Name), xlog.String(backplane.AuditActor, caller),
		xlog.String(backplane.AuditOutcome, map[string]string{"delivered": "succeeded", "failed": "failed"}[outcome]),
		xlog.String("channel", ch.Name), xlog.String("kind", ch.Kind), xlog.String("provider", ch.Provider),
		xlog.String("target", target), xlog.String("id", id), xlog.Int("attempt", int(info.Attempt)),
	}

	if err != nil {
		fields = append(fields, xlog.String("error", err.Error()))

		var pe *provider.Error
		if errors.As(err, &pe) {
			fields = append(fields, xlog.String("code", pe.Code), xlog.Bool("permanent", pe.Permanent))
		}
	}

	c.Log().Ctx().Info(ctx, "courier."+outcome, fields...)
}
