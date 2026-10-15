package mail_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/backplane-labs/backplane-courier/internal/mail"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	good := mail.Message{To: []string{"Ann <ann@example.com>"}, Subject: "Hi", Text: "hello"}
	if err := good.Validate(mail.DefaultLimits); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		msg  mail.Message
		want error
	}{
		"no recipient": {mail.Message{Subject: "x", Text: "y"}, mail.ErrNoRecipient},
		"no subject":   {mail.Message{To: []string{"a@b.c"}, Text: "y"}, mail.ErrNoSubject},
		"no body":      {mail.Message{To: []string{"a@b.c"}, Subject: "x"}, mail.ErrNoBody},
		"bad address":  {mail.Message{To: []string{"not an address"}, Subject: "x", Text: "y"}, mail.ErrAddress},
		"bad header":   {mail.Message{To: []string{"a@b.c"}, Subject: "x", Text: "y", Headers: map[string]string{"From": "z"}}, mail.ErrHeader},
		"attachment":   {mail.Message{To: []string{"a@b.c"}, Subject: "x", Text: "y", Attachments: []mail.Attachment{{Name: "f"}}}, mail.ErrAttachment},
		"too big":      {mail.Message{To: []string{"a@b.c"}, Subject: "x", Text: strings.Repeat("y", 2000)}, mail.ErrTooBig},
		"too many":     {mail.Message{To: []string{"a@b.c", "d@e.f"}, Subject: "x", Text: "y"}, mail.ErrNoRecipient},
		"crlf header":  {mail.Message{To: []string{"a@b.c"}, Subject: "x", Text: "y", Headers: map[string]string{"X-Id": "1\r\nBcc: z@b.c"}}, mail.ErrHeader},
		"crlf subject": {mail.Message{To: []string{"a@b.c"}, Subject: "x\nBcc: z@b.c", Text: "y"}, mail.ErrHeader},
		"crlf address": {mail.Message{To: []string{"a@b.c\r\nX: y"}, Subject: "x", Text: "y"}, mail.ErrAddress},
		"path name":    {mail.Message{To: []string{"a@b.c"}, Subject: "x", Text: "y", Attachments: []mail.Attachment{{Name: "../x", Data: []byte("1")}}}, mail.ErrAttachment},
	} {
		err := c.msg.Validate(mail.Limits{MaxBytes: 1000, MaxRecipients: 1})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

func TestBuildAndHash(t *testing.T) {
	t.Parallel()

	msg, err := mail.Build(mail.Message{
		To: []string{"ann@example.com"}, Cc: []string{"Bob <bob@example.com>"}, Subject: "Order 7", Text: "plain", HTML: "<b>html</b>",
		Headers:     map[string]string{"X-Order": "7", "List-Unsubscribe": "<mailto:u@example.com>"},
		Attachments: []mail.Attachment{{Name: "invoice.txt", Data: []byte("total 1")}, {Name: "README", Data: []byte("no extension")}},
	}, "Shop <no-reply@shop.example>", "id-1@smtp")
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if _, err := msg.WriteTo(&b); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"From: \"Shop\" <no-reply@shop.example>", "To: <ann@example.com>", "Cc: \"Bob\" <bob@example.com>",
		"Subject: Order 7", "Message-ID: <id-1@smtp>", "X-Order: 7", "multipart/mixed", "multipart/alternative", "text/html", "invoice.txt",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}

	if _, err := mail.Build(mail.Message{To: []string{"ann@example.com"}, Subject: "x", Text: "y"}, "", "id"); !errors.Is(err, mail.ErrAddress) {
		t.Fatal("no sender", err)
	}

	if mail.Hash("Ann <ANN@Example.com>") != mail.Hash("ann@example.com") || mail.Hash("a@b.c") == mail.Hash("b@b.c") || len(mail.Hash("x")) != 64 {
		t.Fatal("hash normalizes the address and nothing else")
	}

	m := mail.Message{To: []string{"a"}, Cc: []string{"b"}, Bcc: []string{"c"}}
	if got := m.Recipients(); strings.Join(got, ",") != "a,b,c" {
		t.Fatal(got)
	}
}
