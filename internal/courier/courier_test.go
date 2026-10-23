package courier_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/activity"
	"github.com/backplane-labs/backplane/pkg/backplane/backplanetest"
	"github.com/backplane-labs/backplane/pkg/backplane/config"

	sp "github.com/gopherex/schemapb/go/schemapb"

	"github.com/backplane-labs/backplane-courier/internal/channels"
	"github.com/backplane-labs/backplane-courier/internal/courier"
	"github.com/backplane-labs/backplane-courier/internal/fcm"
	"github.com/backplane-labs/backplane-courier/internal/fcm/fcmtest"
	"github.com/backplane-labs/backplane-courier/internal/mail"
	"github.com/backplane-labs/backplane-courier/internal/push"
	"github.com/backplane-labs/backplane-courier/internal/smtp"
	"github.com/backplane-labs/backplane-courier/internal/smtp/smtptest"
	"github.com/backplane-labs/backplane-courier/internal/webpush"
	"github.com/backplane-labs/backplane-courier/plugin"
	"github.com/backplane-labs/backplane-courier/provider"
)

// sms is a provider courier does not know: it records what it sends and
// refuses the number 000 for good, 111 for now.
type sms struct {
	mu   sync.Mutex
	sent []map[string]any
	keys []string
}

type smsMessage struct {
	To   string `json:"to"   schemapb:"description=Phone number"`
	Text string `json:"text"`
}

type smsSettings struct {
	Login    string        `json:"login"`
	Password config.Secret `json:"password" schemapb:"secret=true"`
}

func (s *sms) Describe(context.Context) (provider.Descriptor, error) {
	msg, err := sp.ReflectType[smsMessage](sp.ID(sp.Namespace("sms"), sp.SchemaName("message"), sp.Ver(1, 0, 0)))
	if err != nil {
		return provider.Descriptor{}, err
	}

	settings, err := sp.ReflectType[smsSettings](sp.ID(sp.Namespace("sms"), sp.SchemaName("settings"), sp.Ver(1, 0, 0)))
	if err != nil {
		return provider.Descriptor{}, err
	}

	return provider.Descriptor{Kind: "sms", Description: "sends an SMS", Settings: settings, Message: msg}, nil
}

func (s *sms) Check(_ context.Context, ch provider.Channel) error {
	var set map[string]string

	_ = json.Unmarshal(ch.Settings, &set)

	if set["password"] != "pw" {
		return provider.Permanent("AUTH", "wrong password")
	}

	return nil
}

func (s *sms) Send(_ context.Context, d provider.Delivery) ([]byte, error) {
	var msg map[string]any
	if err := json.Unmarshal(d.Message, &msg); err != nil {
		return nil, err
	}

	switch msg["to"] {
	case "000":
		return nil, provider.Permanent("BAD_NUMBER", "no such number")
	case "111":
		return nil, provider.Later("BUSY", "try later", time.Second)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.sent = append(s.sent, msg)
	s.keys = append(s.keys, d.IdempotencyKey)

	return []byte(`{"id":"sms-1"}`), nil
}

type fixture struct {
	h       *backplanetest.Harness
	courier *courier.Courier
	smtp    *smtptest.Server
	fcm     *fcmtest.Server
	push    *pushService
	sms     *sms
	remote  *sms
}

func start(t *testing.T) *fixture {
	t.Helper()

	f := &fixture{smtp: smtptest.Start(t), fcm: fcmtest.Start(t), push: startPushService(t), sms: &sms{}, remote: &sms{}}
	f.h = backplanetest.New(t, backplanetest.Name("courier"))

	// The remote provider is a plugin on a unix socket.
	socket := filepath.Join(t.TempDir(), "sms.sock")

	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = plugin.ServeListener(ctx, lis, f.remote) }()

	client, err := plugin.Dial("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	exts := map[string]*channels.Extension{}

	for name, p := range map[string]provider.Provider{"sms": f.sms, "remote": client} {
		d, err := p.Describe(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if name == "remote" {
			d.Kind = "pager" // the same provider as another kind
		}

		exts[name] = &channels.Extension{Name: name, Provider: p, Descriptor: d, Plugin: name == "remote"}
	}

	cfg := channels.Config{
		"mail": {
			SMTP: &smtp.Config{Host: f.smtp.Host(), Port: f.smtp.Port(), TLS: smtp.TLSNone, From: "Shop <no-reply@shop.example>"},
			Pool: 2, Timeout: 5 * time.Second,
		},
		"push":    {FCM: &fcm.Config{Credentials: "{}", ProjectID: "p", Endpoint: f.fcm.URL, IIDEndpoint: f.fcm.URL}, Pool: 2, Timeout: 5 * time.Second},
		"devmail": {Log: &channels.Log{Kind: "mail"}, Pool: 1, Timeout: time.Second},
		"web":     {WebPush: f.push.config(t), Pool: 1, Timeout: 5 * time.Second},
		"devweb":  {Log: &channels.Log{Kind: "webpush"}, Pool: 1, Timeout: time.Second},
		"sms": {Provider: &channels.Provider{
			Name: "sms", Settings: map[string]string{"login": "shop"},
			Secrets: map[string]config.Secret{"password": "pw"},
		}, Pool: 1, Timeout: time.Second},
		"pager": {Provider: &channels.Provider{Name: "remote", Secrets: map[string]config.Secret{"password": "pw"}}, Pool: 1, Timeout: time.Second},
	}

	chs, err := channels.New(f.h.Root(), cfg, channels.Options{
		Extensions: exts, Defaults: map[string]string{"mail": "mail", "webpush": "web"}, TokenSource: fcmtest.TokenSource(),
		WebPushClient: f.push.client,
	})
	if err != nil {
		t.Fatal(err)
	}

	live := backplanetest.Config[struct {
		DryRun config.Live[bool] `json:"dry_run" schemapb:"default=false"`
	}](t, nil)

	f.courier, err = courier.New(f.h.Root(), chs, courier.Settings{Limits: mail.DefaultLimits, DryRun: live.DryRun}, exts)
	if err != nil {
		t.Fatal(err)
	}

	f.h.Start()

	return f
}

func call[In, Out any](t *testing.T, f *fixture, name string, in In) (Out, error) {
	t.Helper()

	return backplanetest.Activity[In, Out](t.Context(), f.h, name, in)
}

func TestMail(t *testing.T) {
	t.Parallel()

	f := start(t)

	out, err := call[mail.Message, courier.MailResult](t, f, "SendMail", mail.Message{To: []string{"ann@example.com"}, Subject: "Hi", Text: "hello"})
	if err != nil || out.Channel != "mail" || out.MessageID == "" || len(f.smtp.Received()) != 1 ||
		!strings.Contains(f.smtp.Received()[0].Data, "Subject: Hi") {
		t.Fatalf("mail: %+v %v", out, err)
	}

	// The log channel, named; a refusal for good is final.
	if logged, err := call[mail.Message, courier.MailResult](t, f, "SendMail",
		mail.Message{Channel: "devmail", To: []string{"ann@example.com"}, Subject: "x", Text: "y"}); err != nil || logged.Channel != "devmail" {
		t.Fatalf("log: %+v %v", logged, err)
	}

	f.smtp.Reject("gone@example.com", 550)

	if _, err := call[mail.Message, courier.MailResult](t, f, "SendMail",
		mail.Message{To: []string{"gone@example.com"}, Subject: "x", Text: "y"}); !isFinal(err) {
		t.Fatalf("rejected: %v", err)
	}

	// A push channel cannot carry mail; an unknown one does not exist.
	for _, ch := range []string{"push", "nope"} {
		if _, err := call[mail.Message, courier.MailResult](t, f, "SendMail",
			mail.Message{Channel: ch, To: []string{"a@b.c"}, Subject: "x", Text: "y"}); !isFinal(err) {
			t.Fatalf("channel %s: %v", ch, err)
		}
	}
}

func TestScrub(t *testing.T) {
	t.Parallel()

	got := courier.Scrubbed("550 5.1.1 <gone@example.com>: no such user", []string{"gone@example.com", "x"})
	if strings.Contains(got, "gone@example.com") || !strings.Contains(got, "no such user") {
		t.Fatalf("scrubbed %q", got)
	}
}

func TestPush(t *testing.T) {
	t.Parallel()

	f := start(t)

	out, err := call[push.Message, courier.PushResult](t, f, "SendPush", push.Message{
		Token: "tok", Title: "Hi", APNS: map[string]any{"payload": map[string]any{"aps": map[string]any{"mutable-content": 1}}},
	})
	if err != nil || out.Channel != "push" || out.Name == "" || len(f.fcm.Received()) != 1 {
		t.Fatalf("push: %+v %v", out, err)
	}

	f.fcm.Answer("gone", fcmtest.Answer{Status: 404, Code: "UNREGISTERED", Message: "gone"})
	f.fcm.Answer("busy", fcmtest.Answer{Status: 429, Code: "QUOTA_EXCEEDED", Message: "slow", RetryAfter: "7"})

	if _, err := call[push.Message, courier.PushResult](t, f, "SendPush", push.Message{Token: "gone", Title: "x"}); !isFinal(err) {
		t.Fatalf("dead token: %v", err)
	}

	refused := backplanetest.Events(f.h, f.courier.Refused())
	if len(refused) != 1 || refused[0].GetCode() != "UNREGISTERED" || refused[0].GetKind() != "push" ||
		refused[0].GetChannel() != "push" || !slices.Equal(refused[0].GetAddresses(), []string{"gone"}) {
		t.Fatalf("refused: %v", refused)
	}

	if _, err := call[push.Message, courier.PushResult](t, f, "SendPush", push.Message{Token: "busy", Title: "x"}); retryAfter(err) != 7*time.Second {
		t.Fatalf("quota: %v", err)
	}

	sub, err := call[courier.TopicRequest, courier.TopicResult](t, f, "Subscribe", courier.TopicRequest{Topic: "news", Tokens: []string{"a", "gone"}})
	if err != nil || len(sub.Results) != 2 || sub.Results[0].Error != "" || sub.Results[1].Error == "" ||
		len(f.fcm.Subscribed("news")) != 1 {
		t.Fatalf("subscribe: %+v %v", sub, err)
	}

	if _, err := call[courier.TopicRequest, courier.TopicResult](t, f, "Unsubscribe", courier.TopicRequest{Topic: "news", Tokens: []string{"a"}}); err != nil ||
		len(f.fcm.Subscribed("news")) != 0 {
		t.Fatalf("unsubscribe: %v", err)
	}
}

// pushService is a browser vendor's push service at push.example.com.
type pushService struct {
	srv    *httptest.Server
	client *http.Client
	mu     sync.Mutex
	got    int
	status int
}

func startPushService(t *testing.T) *pushService {
	t.Helper()

	p := &pushService{status: http.StatusCreated}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()

		_, _ = io.Copy(io.Discard, r.Body)
		p.got++

		w.Header().Set("Location", "https://push.example.com/m/1")
		w.WriteHeader(p.status)
	}))
	t.Cleanup(p.srv.Close)

	// push.example.com resolves to the test server, whose certificate
	// names example.com.
	transport, _ := p.srv.Client().Transport.(*http.Transport)
	transport = transport.Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, p.srv.Listener.Addr().String())
	}
	p.client = &http.Client{Transport: transport}

	return p
}

func (p *pushService) config(t *testing.T) *webpush.Config {
	t.Helper()

	public, private, err := webpush.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}

	return &webpush.Config{Subject: "mailto:ops@shop.example", PublicKey: public, PrivateKey: config.Secret(private), Hosts: []string{"push.example.com"}}
}

func (p *pushService) answer(status int) {
	p.mu.Lock()
	p.status = status
	p.mu.Unlock()
}

func subscription(t *testing.T, endpoint string) webpush.Subscription {
	t.Helper()

	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	return webpush.Subscription{Endpoint: endpoint, Keys: webpush.Keys{
		P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
	}}
}

func TestWebPush(t *testing.T) {
	t.Parallel()

	f := start(t)
	sub := subscription(t, "https://push.example.com/s/1")

	out, err := call[webpush.Message, courier.WebPushResult](t, f, "SendWebPush", webpush.Message{Subscription: sub, Payload: []byte(`{"title":"Hi"}`)})
	if err != nil || out.Channel != "web" || out.ID != "https://push.example.com/m/1" || f.push.got != 1 {
		t.Fatalf("webpush: %+v %v", out, err)
	}

	key, err := call[courier.WebPushKeyRequest, courier.WebPushKeyResult](t, f, "WebPushKey", courier.WebPushKeyRequest{})
	if err != nil || key.Channel != "web" || key.PublicKey == "" {
		t.Fatalf("key: %+v %v", key, err)
	}

	// An endpoint outside the channel's hosts never leaves courier.
	if _, err := call[webpush.Message, courier.WebPushResult](t, f, "SendWebPush", webpush.Message{
		Subscription: subscription(t, "https://billing.internal/admin"), Payload: []byte(`{}`),
	}); !isFinal(err) || f.push.got != 1 {
		t.Fatalf("internal endpoint: %v", err)
	}

	// The log channel, named: nothing posted.
	if logged, err := call[webpush.Message, courier.WebPushResult](t, f, "SendWebPush", webpush.Message{
		Channel: "devweb", Subscription: subscription(t, "https://fcm.googleapis.com/fcm/send/x"), Payload: []byte(`{}`),
	}); err != nil || logged.Channel != "devweb" || f.push.got != 1 {
		t.Fatalf("log: %+v %v", logged, err)
	}

	f.push.answer(http.StatusGone)

	if _, err := call[webpush.Message, courier.WebPushResult](t, f, "SendWebPush", webpush.Message{Subscription: sub, Payload: []byte(`{}`)}); !isFinal(err) {
		t.Fatalf("gone: %v", err)
	}

	// Each refusal is an event: a rule drops the subscription. The endpoint
	// outside the hosts first, refused by courier itself.
	refused := backplanetest.Events(f.h, f.courier.Refused())
	if len(refused) != 2 || refused[0].GetCode() != "forbidden_destination" ||
		!slices.Equal(refused[0].GetAddresses(), []string{"https://billing.internal/admin"}) ||
		refused[1].GetCode() != "gone" || refused[1].GetKind() != "webpush" ||
		!slices.Equal(refused[1].GetAddresses(), []string{sub.Endpoint}) {
		t.Fatalf("refused: %v", refused)
	}

	f.push.answer(http.StatusTooManyRequests)

	if _, err := call[webpush.Message, courier.WebPushResult](t, f, "SendWebPush", webpush.Message{Subscription: sub, Payload: []byte(`{}`)}); retryAfter(err) != time.Minute {
		t.Fatalf("rate limited: %v", err)
	}
}

// A provider courier does not know, compiled in or a plugin over gRPC:
// its own activity, its schema, its failures.
func TestExtensions(t *testing.T) {
	t.Parallel()

	f := start(t)

	in := f.h.Manifest().GetActivities()
	names := map[string]bool{}

	for _, a := range in {
		names[a.GetName()] = true

		if a.GetName() == "SendSms" {
			fields := map[string]bool{}
			for _, fl := range a.GetInput().GetFields() {
				fields[fl.GetName()] = true
			}

			if !fields["to"] || !fields["text"] || !fields["channel"] {
				t.Fatalf("SendSms schema: %v", a.GetInput())
			}
		}
	}

	if !names["SendSms"] || !names["SendPager"] || !names["SendMail"] || !names["SendPush"] || !names["SendWebPush"] || !names["WebPushKey"] {
		t.Fatalf("activities %v", names)
	}

	type msg struct {
		Channel string `json:"channel,omitempty"`
		To      string `json:"to"`
		Text    string `json:"text"`
	}

	out, err := call[msg, map[string]any](t, f, "SendSms", msg{To: "+100", Text: "hi"})
	if err != nil || out["id"] != "sms-1" || len(f.sms.sent) != 1 || f.sms.sent[0]["channel"] != nil {
		t.Fatalf("sms: %v %v %v", out, err, f.sms.sent)
	}

	if _, err := call[msg, map[string]any](t, f, "SendSms", msg{To: "000"}); !isFinal(err) {
		t.Fatalf("bad number: %v", err)
	}

	if _, err := call[msg, map[string]any](t, f, "SendSms", msg{To: "111"}); retryAfter(err) != time.Second {
		t.Fatalf("busy: %v", err)
	}

	// The plugin over gRPC behaves the same.
	if paged, err := call[msg, map[string]any](t, f, "SendPager", msg{To: "+200", Text: "hi"}); err != nil || paged["id"] != "sms-1" ||
		len(f.remote.sent) != 1 {
		t.Fatalf("pager: %v %v", paged, err)
	}

	if _, err := call[msg, map[string]any](t, f, "SendPager", msg{To: "000"}); !isFinal(err) {
		t.Fatalf("pager bad number: %v", err)
	}

	if _, err := call[msg, map[string]any](t, f, "SendPager", msg{To: "111"}); retryAfter(err) != time.Second {
		t.Fatalf("pager busy: %v", err)
	}
}

func isFinal(err error) bool { return activity.IsNonRetryable(err) }

func retryAfter(err error) time.Duration {
	d, _ := activity.RetryAfterOf(err)

	return d
}
