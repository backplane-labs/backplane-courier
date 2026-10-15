// Package smtptest is an SMTP server for tests: it keeps what it receives
// and refuses the recipients it is told to.
package smtptest

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/emersion/go-smtp"
)

// Received is one accepted mail.
type Received struct {
	From string
	To   []string
	Data string
}

// Server records mails; Reject maps recipients to the SMTP code they get.
type Server struct {
	Addr string

	mu       sync.Mutex
	received []Received
	reject   map[string]int
	server   *smtp.Server
}

// Start listens on a loopback port and stops at test cleanup.
func Start(t *testing.T) *Server {
	t.Helper()

	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{Addr: l.Addr().String(), reject: map[string]int{}}
	s.server = smtp.NewServer(backend{s})
	s.server.Domain = "smtptest"
	s.server.AllowInsecureAuth = true

	go func() { _ = s.server.Serve(l) }()

	t.Cleanup(func() { _ = s.server.Close() })

	return s
}

// Reject makes every RCPT of address fail with code (a 5xx is permanent).
func (s *Server) Reject(address string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reject[address] = code
}

// Received so far.
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Received(nil), s.received...)
}

// Host and Port of the listener.
func (s *Server) Host() string { h, _, _ := net.SplitHostPort(s.Addr); return h }

func (s *Server) Port() int64 {
	_, p, _ := net.SplitHostPort(s.Addr)
	n, _ := strconv.ParseInt(p, 10, 64)

	return n
}

type backend struct{ s *Server }

func (b backend) NewSession(*smtp.Conn) (smtp.Session, error) { //nolint:ireturn // go-smtp's interface
	return &session{s: b.s}, nil
}

type session struct {
	s    *Server
	from string
	to   []string
}

func (*session) Reset() {}

func (*session) Logout() error { return nil }

func (ss *session) Mail(from string, _ *smtp.MailOptions) error {
	ss.from, ss.to = from, nil

	return nil
}

func (ss *session) Rcpt(recipient string, _ *smtp.RcptOptions) error {
	ss.s.mu.Lock()
	code, rejected := ss.s.reject[recipient]
	ss.s.mu.Unlock()

	if rejected {
		return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "mailbox unavailable"}
	}

	ss.to = append(ss.to, recipient)

	return nil
}

func (ss *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return errors.New("read: " + err.Error()) //nolint:err113 // a test server
	}

	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()

	ss.s.received = append(ss.s.received, Received{From: ss.from, To: append([]string(nil), ss.to...), Data: string(data)})

	return nil
}
