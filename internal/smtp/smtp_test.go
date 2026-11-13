package smtp_test

import (
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	gomail "github.com/wneessen/go-mail"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/backplane-labs/backplane-courier/internal/smtp"
	"github.com/backplane-labs/backplane-courier/internal/smtp/smtptest"
	"github.com/backplane-labs/backplane-courier/provider"
)

// spans records the sender's spans: the SDK's global provider stands in,
// set once for the package.
var spans = tracetest.NewSpanRecorder()

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	os.Exit(m.Run())
}

func message(t *testing.T, to string) *gomail.Msg {
	t.Helper()

	msg := gomail.NewMsg()
	if err := msg.From("shop@example.com"); err != nil {
		t.Fatal(err)
	}

	if err := msg.To(to); err != nil {
		t.Fatal(err)
	}

	msg.Subject("hi")
	msg.SetBodyString(gomail.TypeTextPlain, "hello")

	return msg
}

// A send is delivered, or refused permanently on a 5xx; either way it is a
// client span of the caller's trace naming the relay, an error when refused.
func TestSend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		to        string
		permanent bool
	}{
		{name: "delivered", to: "ann@example.com"},
		{name: "a 5xx is permanent", to: "gone@example.com", permanent: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := smtptest.Start(t)
			srv.Reject("gone@example.com", 550)

			ctx, caller := otel.Tracer("test").Start(t.Context(), "caller")

			sender := smtp.New(smtp.Config{Host: srv.Host(), Port: srv.Port(), TLS: smtp.TLSNone}, 5*time.Second)
			err := sender.Send(ctx, message(t, tc.to))

			caller.End()

			var refused *provider.Error

			switch {
			case tc.permanent && (!errors.As(err, &refused) || !refused.Permanent || refused.Code != "550"):
				t.Fatalf("refusal: %v", err)
			case !tc.permanent && (err != nil || len(srv.Received()) != 1):
				t.Fatalf("delivery: %v, received %d", err, len(srv.Received()))
			}

			var send sdktrace.ReadOnlySpan

			for _, s := range spans.Ended() {
				if s.Name() == "smtp send" && s.Parent().SpanID() == caller.SpanContext().SpanID() {
					send = s
				}
			}

			if send == nil {
				t.Fatal("no smtp send span under the caller")
			}

			attrs := map[string]string{}
			for _, a := range send.Attributes() {
				attrs[string(a.Key)] = a.Value.String()
			}

			if attrs["server.address"] != srv.Host() || attrs["server.port"] != strconv.FormatInt(srv.Port(), 10) ||
				(send.Status().Code == codes.Error) != tc.permanent || tc.permanent && attrs["error.type"] != "550" {
				t.Fatalf("span attributes %v, status %v", attrs, send.Status())
			}

			// No recipient reaches the span: no recorded error event.
			if len(send.Events()) != 0 {
				t.Fatalf("span events %v", send.Events())
			}
		})
	}
}
