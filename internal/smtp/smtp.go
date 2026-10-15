// Package smtp is the built-in mail provider: one SMTP relay per channel,
// a connection per delivery, 5xx replies final.
package smtp

import (
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"strconv"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/config"
	gomail "github.com/wneessen/go-mail"

	"github.com/backplane-labs/backplane-courier/provider"
)

// TLS modes.
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"
)

// ErrConfig is an invalid channel.
var ErrConfig = errors.New("smtp: invalid channel")

// Config of an SMTP channel.
type Config struct {
	Host string `json:"host" schemapb:"description=Relay host"`
	Port int64  `json:"port" schemapb:"default=587;gte=1;lte=65535"`
	// TLS: starttls (default), tls (implicit on connect), none.
	TLS      string        `json:"tls"                schemapb:"default=starttls;description=starttls, tls or none"`
	Username string        `json:"username,omitempty"`
	Password config.Secret `json:"password,omitempty" schemapb:"secret=true"`
	// From is the sender when the message names none.
	From string `json:"from" schemapb:"description=Default sender, e.g. Shop <no-reply@shop.example>"`
}

// Validate checks what the schema cannot.
func (c Config) Validate() error {
	switch {
	case c.Host == "":
		return fmt.Errorf("%w: host is required", ErrConfig)
	case c.TLS != TLSStartTLS && c.TLS != TLSImplicit && c.TLS != TLSNone:
		return fmt.Errorf("%w: tls must be starttls, tls or none", ErrConfig)
	case c.From == "":
		return fmt.Errorf("%w: from is required", ErrConfig)
	}

	return nil
}

// Sender delivers through one relay.
type Sender struct {
	cfg     Config
	timeout time.Duration
}

// New is the sender of cfg; timeout bounds one connection.
func New(cfg Config, timeout time.Duration) *Sender { return &Sender{cfg: cfg, timeout: timeout} }

// From is the channel's default sender.
func (s *Sender) From() string { return s.cfg.From }

func (s *Sender) client() (*gomail.Client, error) {
	opts := []gomail.Option{gomail.WithPort(int(s.cfg.Port)), gomail.WithTimeout(s.timeout)}

	switch s.cfg.TLS {
	case TLSImplicit:
		opts = append(opts, gomail.WithSSL(), gomail.WithTLSPolicy(gomail.TLSMandatory))
	case TLSNone:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}

	if s.cfg.Username != "" {
		auth := gomail.SMTPAuthPlain
		if s.cfg.TLS == TLSNone {
			auth = gomail.SMTPAuthPlainNoEnc
		}

		opts = append(opts, gomail.WithSMTPAuth(auth), gomail.WithUsername(s.cfg.Username),
			gomail.WithPassword(s.cfg.Password.Reveal()))
	}

	c, err := gomail.NewClient(s.cfg.Host, opts...)
	if err != nil {
		return nil, provider.Permanent("CONFIG", err.Error())
	}

	return c, nil
}

// Send delivers msg; a 5xx reply is a permanent *provider.Error.
func (s *Sender) Send(ctx context.Context, msg *gomail.Msg) error {
	c, err := s.client()
	if err != nil {
		return err
	}

	if err := c.DialAndSendWithContext(ctx, msg); err != nil {
		return classify(err)
	}

	return nil
}

// Check connects and authenticates without sending.
func (s *Sender) Check(ctx context.Context) error {
	c, err := s.client()
	if err != nil {
		return err
	}

	if err := c.DialWithContext(ctx); err != nil {
		return classify(err)
	}

	return c.Close() //nolint:wrapcheck // the client's own error
}

const permanentReply = 500

// classify marks a 5xx reply as permanent; the rest may be retried.
func classify(err error) error {
	var sendErr *gomail.SendError
	if errors.As(err, &sendErr) && !sendErr.IsTemp() && sendErr.ErrorCode() >= permanentReply {
		return &provider.Error{Code: strconv.Itoa(sendErr.ErrorCode()), Message: err.Error(), Permanent: true}
	}

	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= permanentReply {
		return &provider.Error{Code: strconv.Itoa(reply.Code), Message: err.Error(), Permanent: true}
	}

	return fmt.Errorf("smtp: %w", err)
}
