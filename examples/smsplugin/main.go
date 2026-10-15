// smsplugin is an example courier plugin: an SMS "gateway" that logs what
// it would send. A real one calls its provider's API in Send. Run it next
// to courier and name it in courier's configuration:
//
//	plugins:  {sms: {address: "unix:///run/courier/sms.sock"}}
//	channels: {sms: {provider: {name: sms, settings: {sender: Shop}, secrets: {token: ${SMS_TOKEN}}}}}
//
// Courier then offers courier.SendSms with this plugin's message schema.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	sp "github.com/gopherex/schemapb/go/schemapb"

	"github.com/backplane-labs/backplane-courier/plugin"
	"github.com/backplane-labs/backplane-courier/provider"
)

// Message is what courier.SendSms takes.
type Message struct {
	To   string `json:"to"   schemapb:"description=Phone number, E.164"`
	Text string `json:"text" schemapb:"description=Text, at most 1600 characters"`
}

// Settings of an SMS channel.
type Settings struct {
	Sender string `json:"sender" schemapb:"description=Sender name or number"`
	Token  string `json:"token"  schemapb:"secret=true;description=API token"`
}

// Result of a send.
type Result struct {
	ID string `json:"id"`
}

type sms struct{ log *slog.Logger }

func (s sms) Describe(context.Context) (provider.Descriptor, error) {
	id := func(name string) *sp.SchemaIdentity {
		return sp.ID(sp.Namespace("sms"), sp.SchemaName(name), sp.Ver(1, 0, 0))
	}

	msg, err := sp.ReflectType[Message](id("message"))
	if err != nil {
		return provider.Descriptor{}, fmt.Errorf("message schema: %w", err)
	}

	settings, err := sp.ReflectType[Settings](id("settings"))
	if err != nil {
		return provider.Descriptor{}, fmt.Errorf("settings schema: %w", err)
	}

	result, err := sp.ReflectType[Result](id("result"))
	if err != nil {
		return provider.Descriptor{}, fmt.Errorf("result schema: %w", err)
	}

	return provider.Descriptor{
		Kind: "sms", Description: "sends an SMS through the example gateway",
		Settings: settings, Message: msg, Result: result,
	}, nil
}

func (s sms) Check(_ context.Context, ch provider.Channel) error {
	var set Settings
	if err := json.Unmarshal(ch.Settings, &set); err != nil || set.Token == "" {
		return provider.Permanent("CONFIG", "token is required")
	}

	return nil
}

func (s sms) Send(ctx context.Context, d provider.Delivery) ([]byte, error) {
	var msg Message
	if err := json.Unmarshal(d.Message, &msg); err != nil {
		return nil, provider.Permanent("INVALID", err.Error())
	}

	if !strings.HasPrefix(msg.To, "+") {
		return nil, provider.Permanent("BAD_NUMBER", "a number starts with +")
	}

	s.log.InfoContext(ctx, "sms", "channel", d.Channel.Name, "to_len", len(msg.To), "text_len", len(msg.Text),
		"key", d.IdempotencyKey, "attempt", d.Attempt)

	return json.Marshal(Result{ID: d.IdempotencyKey}) //nolint:wrapcheck // a plain struct
}

func main() {
	address := os.Getenv("SMS_PLUGIN_ADDRESS")
	if address == "" {
		address = "unix:///tmp/courier-sms.sock"
	}

	if err := plugin.Serve(address, sms{log: slog.Default()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
