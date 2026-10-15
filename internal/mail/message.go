// Package mail is the service's domain: the message a caller hands to
// courier.SendMail, its checks and limits, address hashing and the MIME it
// becomes. It knows nothing of accounts, queues or the platform.
package mail

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"path/filepath"
	"strings"

	gomail "github.com/wneessen/go-mail"
)

// Errors of Validate; every one is final: no retry changes the message.
var (
	ErrNoRecipient = errors.New("mail: no recipient")
	ErrNoSubject   = errors.New("mail: subject is empty")
	ErrNoBody      = errors.New("mail: text or html body is required")
	ErrAddress     = errors.New("mail: invalid address")
	ErrHeader      = errors.New("mail: header not allowed")
	ErrTooBig      = errors.New("mail: message exceeds the size limit")
	ErrAttachment  = errors.New("mail: invalid attachment")
)

// Limits bound a message.
type Limits struct {
	// MaxBytes bounds the sum of bodies and attachments.
	MaxBytes int64
	// MaxRecipients bounds to + cc + bcc.
	MaxRecipients int
}

// Defaults of the limits.
const (
	DefaultMaxBytes      = 25 << 20
	DefaultMaxRecipients = 100
)

// DefaultLimits of a message: 25 MiB, 100 recipients.
//
//nolint:gochecknoglobals // immutable
var DefaultLimits = Limits{MaxBytes: DefaultMaxBytes, MaxRecipients: DefaultMaxRecipients}

// Attachment is a file in the message; Data is base64 on the wire.
type Attachment struct {
	Name        string `json:"name"                   schemapb:"description=File name"`
	ContentType string `json:"content_type,omitempty" schemapb:"description=MIME type, inferred from the name when empty"`
	Data        []byte `json:"data"                   schemapb:"description=Content, base64"`
}

// Message is what a caller sends: a rendered mail. Addresses are RFC 5322
// ("Name <user@host>" or "user@host").
type Message struct {
	// Channel names the installation's mail channel; empty: the default.
	Channel string `json:"channel,omitempty" schemapb:"description=Mail channel of the installation, else the default"`
	// From overrides the channel's default sender.
	From    string   `json:"from,omitempty"     schemapb:"description=Sender, else the channel's default"`
	To      []string `json:"to"                 schemapb:"description=Recipients"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	ReplyTo string   `json:"reply_to,omitempty"`
	Subject string   `json:"subject"`
	Text    string   `json:"text,omitempty"     schemapb:"description=Plain text body"`
	HTML    string   `json:"html,omitempty"     schemapb:"description=HTML body, an alternative of text when both are set"`
	// Headers: X-*, List-*, In-Reply-To, References, Precedence and the like.
	Headers     map[string]string `json:"headers,omitempty"     schemapb:"description=Extra headers: X-*, List-*"`
	Attachments []Attachment      `json:"attachments,omitempty"`
}

// Recipients are to, cc and bcc in order, as given.
func (m Message) Recipients() []string {
	out := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	out = append(out, m.To...)
	out = append(out, m.Cc...)
	out = append(out, m.Bcc...)

	return out
}

// Size is what the bodies and attachments take.
func (m Message) Size() int64 {
	n := int64(len(m.Text) + len(m.HTML) + len(m.Subject))
	for _, a := range m.Attachments {
		n += int64(len(a.Data))
	}

	return n
}

// Validate checks the message against the limits; its error is final.
func (m Message) Validate(limits Limits) error {
	return errors.Join(m.checkEnvelope(limits), m.checkHeaders(), m.checkAttachments(), m.checkSize(limits))
}

func (m Message) checkEnvelope(limits Limits) error {
	var errs []error

	if len(m.To) == 0 {
		errs = append(errs, ErrNoRecipient)
	}

	if recipients := m.Recipients(); len(recipients) > limits.MaxRecipients {
		errs = append(errs, fmt.Errorf("%w: %d recipients, at most %d", ErrNoRecipient, len(recipients),
			limits.MaxRecipients))
	}

	for _, a := range append(m.Recipients(), m.From, m.ReplyTo) {
		if a == "" {
			continue
		}

		if _, err := mail.ParseAddress(a); err != nil || strings.ContainsAny(a, "\r\n") {
			errs = append(errs, fmt.Errorf("%w: %q", ErrAddress, a))
		}
	}

	return errors.Join(errs...)
}

// checkHeaders: the subject and the extra headers stay on their lines.
func (m Message) checkHeaders() error {
	var errs []error

	if strings.TrimSpace(m.Subject) == "" {
		errs = append(errs, ErrNoSubject)
	}

	if strings.ContainsAny(m.Subject, "\r\n") {
		errs = append(errs, fmt.Errorf("%w: subject spans lines", ErrHeader))
	}

	if m.Text == "" && m.HTML == "" {
		errs = append(errs, ErrNoBody)
	}

	for name, value := range m.Headers {
		if !headerAllowed(name) || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Errorf("%w: %s", ErrHeader, name))
		}
	}

	return errors.Join(errs...)
}

func (m Message) checkAttachments() error {
	var errs []error

	for _, a := range m.Attachments {
		if a.Name == "" || len(a.Data) == 0 || strings.ContainsAny(a.Name, "\r\n\"/\\") {
			errs = append(errs, fmt.Errorf("%w: a plain file name and content are required", ErrAttachment))
		}
	}

	return errors.Join(errs...)
}

func (m Message) checkSize(limits Limits) error {
	if m.Size() > limits.MaxBytes {
		return fmt.Errorf("%w: %d bytes, at most %d", ErrTooBig, m.Size(), limits.MaxBytes)
	}

	return nil
}

// headerAllowed: custom and threading headers only; the message's own
// (From, To, Subject, Content-*) come from its fields.
func headerAllowed(name string) bool {
	n := strings.ToLower(name)

	return strings.HasPrefix(n, "x-") || strings.HasPrefix(n, "list-") || n == "in-reply-to" || n == "references" ||
		n == "precedence" || n == "auto-submitted" || n == "importance" || n == "priority"
}

// Hash identifies an address without keeping it: sha256 of the lower-cased
// address part, hex.
func Hash(address string) string {
	a := address
	if parsed, err := mail.ParseAddress(address); err == nil {
		a = parsed.Address
	}

	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(a))))

	return hex.EncodeToString(sum[:])
}

// Hashes of every address, in order.
func Hashes(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	for _, a := range addresses {
		out = append(out, Hash(a))
	}

	return out
}

// Build is the message as MIME, with from as sender when the message names
// none and id as its Message-ID.
func Build(m Message, from, id string) (*gomail.Msg, error) {
	msg := gomail.NewMsg()

	sender := m.From
	if sender == "" {
		sender = from
	}

	if err := msg.From(sender); err != nil {
		return nil, fmt.Errorf("%w: from %q", ErrAddress, sender)
	}

	if err := errors.Join(msg.To(m.To...), msg.Cc(m.Cc...), msg.Bcc(m.Bcc...)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAddress, err)
	}

	if m.ReplyTo != "" {
		if err := msg.ReplyTo(m.ReplyTo); err != nil {
			return nil, fmt.Errorf("%w: reply-to %q", ErrAddress, m.ReplyTo)
		}
	}

	msg.Subject(m.Subject)
	msg.SetMessageIDWithValue(id)
	msg.SetDate()

	switch {
	case m.Text != "" && m.HTML != "":
		msg.SetBodyString(gomail.TypeTextPlain, m.Text)
		msg.AddAlternativeString(gomail.TypeTextHTML, m.HTML)
	case m.HTML != "":
		msg.SetBodyString(gomail.TypeTextHTML, m.HTML)
	default:
		msg.SetBodyString(gomail.TypeTextPlain, m.Text)
	}

	for name, value := range m.Headers {
		msg.SetGenHeader(gomail.Header(name), value)
	}

	for _, a := range m.Attachments {
		contentType := a.ContentType
		if contentType == "" {
			contentType = mime.TypeByExtension(filepath.Ext(a.Name))
		}

		opts := []gomail.FileOption{}
		if contentType != "" {
			opts = append(opts, gomail.WithFileContentType(gomail.ContentType(contentType)))
		}

		if err := msg.AttachReader(a.Name, strings.NewReader(string(a.Data)), opts...); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrAttachment, a.Name, err)
		}
	}

	return msg, nil
}
