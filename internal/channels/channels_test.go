package channels_test

import (
	"errors"
	"testing"
	"time"

	"github.com/backplane-labs/backplane/pkg/backplane/backplanetest"

	"github.com/backplane-labs/backplane-courier/internal/channels"
	"github.com/backplane-labs/backplane-courier/internal/smtp"
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
