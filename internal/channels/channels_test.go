package channels_test

import (
	"errors"
	"testing"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/backplanetest"
	"github.com/backplane-labs/backplane/pkg/backplane/config"

	"github.com/backplane-labs/backplane-courier/internal/channels"
	"github.com/backplane-labs/backplane-courier/internal/smtp"
	"github.com/backplane-labs/backplane-courier/internal/webpush"
)

func TestPickAndValidate(t *testing.T) {
	t.Parallel()

	mailA := channels.Spec{SMTP: &smtp.Config{Host: "a", Port: 25, TLS: smtp.TLSNone, From: "a@a"}, Pool: 1, Timeout: time.Second}
	logMail := channels.Spec{Log: &channels.Log{Kind: "mail"}, Pool: 1, Timeout: time.Second}
	logPush := channels.Spec{Log: &channels.Log{Kind: "push"}, Pool: 1, Timeout: time.Second}

	h := backplanetest.New(t)

	chs, err := channels.New(h.Root(), channels.Config{"a": mailA, "b": logMail, "p": logPush}, channels.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Two mail channels, no default: a message must name one; push has one.
	if _, err := chs.Pick("mail", ""); !errors.Is(err, channels.ErrNoDefault) {
		t.Fatalf("no default: %v", err)
	}

	if ch, err := chs.Pick("push", ""); err != nil || ch.Name != "p" {
		t.Fatalf("only push: %v %v", ch, err)
	}

	if _, err := chs.Pick("mail", "p"); !errors.Is(err, channels.ErrWrongKind) {
		t.Fatalf("wrong kind: %v", err)
	}

	chs, err = channels.New(h.Root(), channels.Config{"a": mailA, "b": logMail}, channels.Options{Defaults: map[string]string{"mail": "b"}})
	if err != nil {
		t.Fatal(err)
	}

	if ch, err := chs.Pick("mail", ""); err != nil || ch.Name != "b" || !chs.IsDefault(ch) {
		t.Fatalf("default: %v %v", ch, err)
	}

	if _, err := channels.New(h.Root(), channels.Config{"a": mailA}, channels.Options{Defaults: map[string]string{"push": "a"}}); err == nil {
		t.Fatal("default of the wrong kind accepted")
	}

	if _, err := channels.New(h.Root(), channels.Config{"x": {Provider: &channels.Provider{Name: "nope"}}}, channels.Options{}); !errors.Is(err, channels.ErrProvider) {
		t.Fatalf("unknown provider: %v", err)
	}

	for name, bad := range map[string]channels.Spec{
		"none": {},
		"two":  {SMTP: mailA.SMTP, Log: &channels.Log{Kind: "mail"}},
		"smtp": {SMTP: &smtp.Config{TLS: smtp.TLSNone}},
		"log":  {Log: &channels.Log{Kind: "Mail!"}},
	} {
		if bad.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// A Web Push channel without a pair needs a store to mint one; with a pair
// it needs none. MintsWebPushKeys tells which.
func TestWebPushKeys(t *testing.T) {
	t.Parallel()

	public, private, err := webpush.GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}

	given := channels.Spec{WebPush: &webpush.Config{
		Subject: "mailto:ops@example.com", PublicKey: public, PrivateKey: config.Secret(private),
	}}
	minted := channels.Spec{WebPush: &webpush.Config{Subject: "mailto:ops@example.com"}}

	tests := []struct {
		name    string
		cfg     channels.Config
		mints   bool
		wantErr error
	}{
		{name: "configured pair", cfg: channels.Config{"w": given}},
		{name: "no pair, no store", cfg: channels.Config{"w": minted}, mints: true, wantErr: channels.ErrNoKeyStore},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.cfg.MintsWebPushKeys(); got != tc.mints {
				t.Fatalf("mints %v, want %v", got, tc.mints)
			}

			chs, err := channels.New(backplanetest.New(t).Root(), tc.cfg, channels.Options{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err %v, want %v", err, tc.wantErr)
			}

			if err == nil {
				if ch, _ := chs.Get("w"); ch == nil || ch.WebPush.PublicKey() != public {
					t.Fatalf("channel %+v", ch)
				}
			}
		})
	}
}
