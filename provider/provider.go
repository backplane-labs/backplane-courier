// Package provider is what a courier provider implements: one way to
// deliver messages courier does not know. A Go provider is compiled into a
// custom courier binary (courier.Main(courier.WithProvider(p))); one in any
// language runs as a plugin serving courier.plugin.v1.ProviderService
// (package plugin serves a Go Provider that way).
package provider

import (
	"context"
	"fmt"
	"time"

	sp "github.com/gopherex/schemapb/go/schemapb"
)

// Descriptor is what a provider delivers and the shapes it takes.
type Descriptor struct {
	// Kind: [a-z][a-z0-9_]*; the activity is Send<Kind> (SendSmsc).
	Kind        string
	Description string
	// Settings of a channel; fields marked secret are masked by courier.
	Settings *sp.Schema
	// Message: the activity's input (courier adds an optional channel).
	Message *sp.Schema
	// Result: the activity's output; nil: {id}.
	Result *sp.Schema
}

// Channel is one configured channel of the provider's kind.
type Channel struct {
	Name string
	// Settings as JSON, valid against Descriptor.Settings.
	Settings []byte
}

// Delivery is one message to send.
type Delivery struct {
	Channel Channel
	// Message as JSON, valid against Descriptor.Message.
	Message []byte
	// IdempotencyKey is the same for every attempt of one delivery.
	IdempotencyKey string
	// Attempt is 1 for the first.
	Attempt int
}

// Provider delivers messages of one kind.
type Provider interface {
	Describe(ctx context.Context) (Descriptor, error)
	// Check a channel against the provider without delivering.
	Check(ctx context.Context, ch Channel) error
	// Send one message; the result is JSON (valid against
	// Descriptor.Result); a failure should be an *Error.
	Send(ctx context.Context, d Delivery) ([]byte, error)
}

// Error is why a delivery or a check failed: final or not, and how long
// to wait before retrying when the provider said so. Any other error is
// retried by the binding's policy.
type Error struct {
	Message    string
	Code       string
	Permanent  bool
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}

	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Permanent is a final failure.
func Permanent(code, message string) *Error {
	return &Error{Code: code, Message: message, Permanent: true}
}

// Later is a failure to retry no sooner than after.
func Later(code, message string, after time.Duration) *Error {
	return &Error{Code: code, Message: message, RetryAfter: after}
}
