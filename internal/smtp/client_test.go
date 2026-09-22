package smtp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// scriptedServer runs a canned SMTP conversation on a listener. Each step is
// either a "send" (write to the client) or an "expect" (read + prefix-match).
type scriptStep struct {
	send   string // written to client verbatim
	expect string // expected prefix from client (case-sensitive)
	action func(net.Conn)
}

type scriptedServer struct {
	t       *testing.T
	ln      net.Listener
	got     []string
	mu      sync.Mutex
	closed  chan struct{}
	handler func(*testing.T, net.Conn, *scriptedServer)
}

func (s *scriptedServer) address() string { return s.ln.Addr().String() }

func (s *scriptedServer) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func (s *scriptedServer) stop() { _ = s.ln.Close(); <-s.closed }

func startScripted(t *testing.T, handler func(*testing.T, net.Conn, *scriptedServer)) *scriptedServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &scriptedServer{t: t, ln: ln, closed: make(chan struct{}), handler: handler}
	go func() {
		defer close(s.closed)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				handler(t, c, s)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func writeLine(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := c.Write([]byte(line)); err != nil {
		t.Logf("scripted write: %v", err)
	}
}

func readLine(t *testing.T, r *bufio.Reader, s *scriptedServer) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		return ""
	}
	s.mu.Lock()
	s.got = append(s.got, strings.TrimRight(line, "\r\n"))
	s.mu.Unlock()
	return line
}

// normalHandler runs a happy-path conversation and records what the client sent.
func normalHandler(_ *testing.T, c net.Conn, s *scriptedServer) {
	r := bufio.NewReader(c)
	writeLine(s.t, c, "220 example.test ready\r\n")
	for {
		line := readLine(s.t, r, s)
		if line == "" {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			writeLine(s.t, c, "250-example.test\r\n")
			writeLine(s.t, c, "250-SIZE 10485760\r\n")
			writeLine(s.t, c, "250-ENHANCEDSTATUSCODES\r\n")
			writeLine(s.t, c, "250 8BITMIME\r\n")
		case strings.HasPrefix(upper, "MAIL FROM"):
			writeLine(s.t, c, "250 2.1.0 Sender OK\r\n")
		case strings.HasPrefix(upper, "RCPT TO"):
			writeLine(s.t, c, "250 2.1.5 Recipient OK\r\n")
		case strings.HasPrefix(upper, "DATA"):
			writeLine(s.t, c, "354 End data with <CR><LF>.<CR><LF>\r\n")
			// Read the message until \r\n.\r\n
			for {
				l := readLine(s.t, r, s)
				if l == "" {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
			}
			writeLine(s.t, c, "250 2.6.0 Accepted\r\n")
		case strings.HasPrefix(upper, "QUIT"):
			writeLine(s.t, c, "221 2.0.0 Bye\r\n")
			return
		default:
			writeLine(s.t, c, "500 5.5.2 unknown\r\n")
		}
	}
}

func testClientConfig() ClientConfig {
	return ClientConfig{Identity: "mailx-test", DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
}

func TestClientHappyPath(t *testing.T) {
	srv := startScripted(t, normalHandler)
	c, err := NewClient(testClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(),
		Envelope: mail.Envelope{
			MailFrom:   "<alice@example.com>",
			Recipients: []string{"<bob@example.com>", "<carol@example.com>"},
		},
		Raw: "From: <alice@example.com>\r\nTo: <bob@example.com>\r\nSubject: hi\r\n\r\nbody line\r\n",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !res.Accepted || res.FinalCode != 250 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.QuitError != nil {
		t.Fatalf("unexpected quit error: %v", res.QuitError)
	}
	got := srv.received()
	want := []string{
		"EHLO mailx-test",
		"MAIL FROM:<alice@example.com>",
		"RCPT TO:<bob@example.com>",
		"RCPT TO:<carol@example.com>",
		"DATA",
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing command %q in %v", w, got)
		}
	}
}

func TestClientRejectsInvalidInputs(t *testing.T) {
	c, _ := NewClient(testClientConfig())
	cases := []struct {
		name string
		req  DeliveryRequest
	}{
		{"empty address", DeliveryRequest{Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}}},
		{"no recipients", DeliveryRequest{Address: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "<a@b>"}}},
		{"CRLF injection sender", DeliveryRequest{Address: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "<a@b>\r\nRCPT TO:<x@y>", Recipients: []string{"<c@d>"}}}},
		{"CRLF injection recipient", DeliveryRequest{Address: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>\r\nDATA"}}}},
		{"null recipient", DeliveryRequest{Address: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<>"}}}},
		{"unbracketed sender", DeliveryRequest{Address: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "a@b", Recipients: []string{"<c@d>"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Send(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected error")
			}
			var d *DeliveryError
			if !asDelivery(err, &d) || d.Stage != StageInvalidInput {
				t.Fatalf("expected invalid_input error, got %v", err)
			}
		})
	}
}

func TestClientNullReverseAllowed(t *testing.T) {
	srv := startScripted(t, normalHandler)
	c, _ := NewClient(testClientConfig())
	res, err := c.Send(context.Background(), DeliveryRequest{
		Address:  srv.address(),
		Envelope: mail.Envelope{MailFrom: "<>", Recipients: []string{"<bob@example.com>"}},
		Raw:      "Subject: bounce\r\n\r\nbody\r\n",
	})
	if err != nil || !res.Accepted {
		t.Fatalf("null reverse rejected: %v %+v", err, res)
	}
	found := false
	for _, g := range srv.received() {
		if g == "MAIL FROM:<>" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected MAIL FROM:<> in %v", srv.received())
	}
}

func TestClientGreetingFailure(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, _ *scriptedServer) {
		writeLine(t, c, "554 5.7.1 no service\r\n")
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address:  srv.address(),
		Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
		Raw:      "Subject: t\r\n\r\nx\r\n",
	})
	var d *DeliveryError
	if !asDelivery(err, &d) || d.Stage != StageGreeting || d.Code != 554 || d.Temporary {
		t.Fatalf("bad error: %v", err)
	}
}

func TestClientGreetingTemp(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, _ *scriptedServer) {
		writeLine(t, c, "421 4.3.0 not ready\r\n")
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if !IsTemporary(err) {
		t.Fatalf("expected temporary, got %v", err)
	}
}

func TestClientHELOFallback(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		for {
			line := readLine(t, r, s)
			if line == "" {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"):
				writeLine(t, c, "502 5.5.1 EHLO unsupported\r\n")
			case strings.HasPrefix(u, "HELO"):
				writeLine(t, c, "250 hello\r\n")
			case strings.HasPrefix(u, "MAIL"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "RCPT"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "DATA"):
				writeLine(t, c, "354 go\r\n")
				for {
					l := readLine(t, r, s)
					if l == "" || strings.TrimRight(l, "\r\n") == "." {
						break
					}
				}
				writeLine(t, c, "250 accepted\r\n")
			case strings.HasPrefix(u, "QUIT"):
				writeLine(t, c, "221 bye\r\n")
				return
			}
		}
	})
	c, _ := NewClient(testClientConfig())
	res, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err != nil || !res.Accepted {
		t.Fatalf("HELO fallback failed: %v %+v", err, res)
	}
	sawHELO := false
	for _, g := range srv.received() {
		if strings.HasPrefix(g, "HELO ") {
			sawHELO = true
		}
	}
	if !sawHELO {
		t.Fatalf("expected HELO fallback, got %v", srv.received())
	}
}

func TestClientEHLO4xxNoFallback(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		for {
			line := readLine(t, r, s)
			if line == "" {
				return
			}
			if strings.HasPrefix(strings.ToUpper(line), "EHLO") {
				writeLine(t, c, "421 4.3.0 try later\r\n")
				return
			}
		}
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	var d *DeliveryError
	if !asDelivery(err, &d) || d.Stage != StageEHLO || !d.Temporary {
		t.Fatalf("expected EHLO temp error, got %v", err)
	}
}

func TestClientRcptFailure(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		for {
			line := readLine(t, r, s)
			if line == "" {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "MAIL"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "RCPT TO:<BAD"), strings.HasPrefix(u, "RCPT TO:<BAD@"):
				writeLine(t, c, "550 5.1.1 unknown user\r\n")
			case strings.HasPrefix(u, "RCPT"):
				writeLine(t, c, "250 ok\r\n")
			}
		}
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address:  srv.address(),
		Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<ok@example.com>", "<bad@example.com>"}},
		Raw:      "x\r\n",
	})
	var d *DeliveryError
	if !asDelivery(err, &d) || d.Stage != StageRcptTo || d.Code != 550 || d.Recipient != "<bad@example.com>" || d.Temporary {
		t.Fatalf("bad rcpt error: %+v", d)
	}
}

func TestClientDATA354Rejected(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		for {
			line := readLine(t, r, s)
			if line == "" {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"), strings.HasPrefix(u, "RCPT"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "DATA"):
				writeLine(t, c, "554 5.5.1 no data now\r\n")
			}
		}
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	var d *DeliveryError
	if !asDelivery(err, &d) || d.Stage != StageData || d.Code != 554 {
		t.Fatalf("bad DATA error: %v", err)
	}
	// The message body must NOT have been sent.
	for _, g := range srv.received() {
		if g == "." || strings.HasPrefix(g, "Subject:") {
			t.Fatalf("body leaked before 354: %v", srv.received())
		}
	}
}

func TestClientFinalDATAFailure(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		for {
			line := readLine(t, r, s)
			if line == "" {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"), strings.HasPrefix(u, "RCPT"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "DATA"):
				writeLine(t, c, "354 go\r\n")
				for {
					l := readLine(t, r, s)
					if l == "" || strings.TrimRight(l, "\r\n") == "." {
						break
					}
				}
				writeLine(t, c, "552 5.3.4 too big\r\n")
				return
			}
		}
	})
	c, _ := NewClient(testClientConfig())
	res, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if res.Accepted {
		t.Fatalf("must not report accepted on 552, got %+v", res)
	}
	var d *DeliveryError
	if !asDelivery(err, &d) || d.Stage != StageDataResponse || d.Code != 552 {
		t.Fatalf("bad final DATA error: %v", err)
	}
}

func TestClientQuitFailureAfterAcceptance(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		for {
			line := readLine(t, r, s)
			if line == "" {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"), strings.HasPrefix(u, "RCPT"):
				writeLine(t, c, "250 ok\r\n")
			case strings.HasPrefix(u, "DATA"):
				writeLine(t, c, "354 go\r\n")
				for {
					l := readLine(t, r, s)
					if l == "" || strings.TrimRight(l, "\r\n") == "." {
						break
					}
				}
				writeLine(t, c, "250 accepted\r\n")
				// Server drops connection instead of replying to QUIT.
				return
			}
		}
	})
	c, _ := NewClient(testClientConfig())
	res, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err != nil {
		t.Fatalf("acceptance must not be reported as failure: %v", err)
	}
	if !res.Accepted || res.QuitError == nil {
		t.Fatalf("expected accepted with QuitError, got %+v", res)
	}
}

func TestClientMalformedReplies(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"too short", "22\r\n"},
		{"non-numeric", "abc ok\r\n"},
		{"bad separator", "250/ok\r\n"},
		{"lf only", "220 ok\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startScripted(t, func(_ *testing.T, c net.Conn, _ *scriptedServer) {
				writeLine(t, c, tc.reply)
			})
			c, _ := NewClient(testClientConfig())
			_, err := c.Send(context.Background(), DeliveryRequest{
				Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
			})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestClientMismatchedMultilineCodes(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		line := readLine(t, r, s)
		_ = line
		writeLine(t, c, "250-example\r\n")
		writeLine(t, c, "550 broken\r\n")
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestClientMultilineBounded(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, s *scriptedServer) {
		r := bufio.NewReader(c)
		writeLine(t, c, "220 ok\r\n")
		_ = readLine(t, r, s)
		// Endless multiline. Client must bound.
		for i := 0; i < 10000; i++ {
			if _, err := c.Write([]byte("250-x\r\n")); err != nil {
				return
			}
		}
	})
	cfg := testClientConfig()
	cfg.MaxReplyLines = 8
	c, _ := NewClient(cfg)
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected line count bound, got %v", err)
	}
}

func TestClientReplyLineBounded(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, _ *scriptedServer) {
		// Write 4KB with no CRLF.
		_, _ = c.Write([]byte("220 " + strings.Repeat("x", 4096)))
	})
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err == nil {
		t.Fatal("expected line-limit error")
	}
}

func TestClientContextCancellation(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, _ *scriptedServer) {
		// Never send greeting.
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
	})
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := NewClient(testClientConfig())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.Send(ctx, DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("cancellation took too long: %v", time.Since(start))
	}
}

func TestClientReadTimeout(t *testing.T) {
	srv := startScripted(t, func(_ *testing.T, c net.Conn, _ *scriptedServer) {
		buf := make([]byte, 1)
		_, _ = c.Read(buf) // block forever
	})
	cfg := testClientConfig()
	cfg.ReadTimeout = 100 * time.Millisecond
	c, _ := NewClient(cfg)
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(), Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestClientDialFailure(t *testing.T) {
	c, _ := NewClient(testClientConfig())
	_, err := c.Send(context.Background(), DeliveryRequest{
		Address: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}, Raw: "x\r\n",
	})
	var d *DeliveryError
	if !asDelivery(err, &d) || d.Stage != StageDial {
		t.Fatalf("expected dial stage, got %v", err)
	}
}

func TestSerializeForDATA(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ".\r\n"},
		{"plain", "hello\r\n", "hello\r\n.\r\n"},
		{"missing crlf", "hello", "hello\r\n.\r\n"},
		{"leading dot", ".hello\r\n", "..hello\r\n.\r\n"},
		{"only dot line", ".\r\n", "..\r\n.\r\n"},
		{"double dot", "..hello\r\n", "...hello\r\n.\r\n"},
		{"lf only normalized", "hello\n", "hello\r\n.\r\n"},
		{"mid-line dot untouched", "abc.def\r\n", "abc.def\r\n.\r\n"},
		{"multi lines dot-prefixed", ".a\r\n.b\r\n", "..a\r\n..b\r\n.\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serializeForDATA(tc.in)
			if got != tc.want {
				t.Fatalf("serialize %q:\ngot  %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseReplyLine(t *testing.T) {
	if c, cont, txt, err := parseReplyLine("250-ok"); err != nil || c != 250 || !cont || txt != "ok" {
		t.Fatalf("continuation parse: %d %v %q %v", c, cont, txt, err)
	}
	if c, cont, txt, err := parseReplyLine("221 bye"); err != nil || c != 221 || cont || txt != "bye" {
		t.Fatalf("final parse: %d %v %q %v", c, cont, txt, err)
	}
	if _, _, _, err := parseReplyLine("22"); err == nil {
		t.Fatal("short must fail")
	}
	if _, _, _, err := parseReplyLine("2X0 x"); err == nil {
		t.Fatal("non-numeric must fail")
	}
	if _, _, _, err := parseReplyLine("999 x"); err == nil {
		t.Fatal("out-of-range must fail")
	}
}

func TestSplitEnhanced(t *testing.T) {
	if e, r := splitEnhanced(550, "5.1.1 User unknown"); e != "5.1.1" || r != "User unknown" {
		t.Fatalf("got %q %q", e, r)
	}
	if e, r := splitEnhanced(250, "OK"); e != "" || r != "OK" {
		t.Fatalf("no enhanced: %q %q", e, r)
	}
	if e, r := splitEnhanced(550, "6.1.1 mismatched class"); e != "" || r != "6.1.1 mismatched class" {
		t.Fatalf("class mismatch must not strip: %q %q", e, r)
	}
}

func TestClientConcurrentSends(t *testing.T) {
	srv := startScripted(t, normalHandler)
	c, _ := NewClient(testClientConfig())
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := c.Send(context.Background(), DeliveryRequest{
				Address:  srv.address(),
				Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{fmt.Sprintf("<r%d@x>", n)}},
				Raw:      fmt.Sprintf("Subject: n%d\r\n\r\nbody %d\r\n", n, n),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent send: %v", err)
		}
	}
}

// Integration: outbound MailX -> inbound MailX -> storage round trip.
func TestClientToMailXInboundIntegration(t *testing.T) {
	dir := t.TempDir()
	// Run the inbound MailX server in-process.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(DefaultConfig(), func(sess Session, m mail.Message) error {
		rec, err := storage.NewMessageRecord(sess.Envelope, m)
		if err != nil {
			return err
		}
		return store.Save(rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); <-serveDone })

	client, _ := NewClient(testClientConfig())

	// Realistic multipart/mixed with text/plain, text/html, base64 attachment,
	// plus a dot-prefixed body line to exercise dot-stuffing.
	raw := "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: MailX round trip\r\n" +
		"Message-ID: <round-trip-1@mailx.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n" +
		"\r\n" +
		"hello world\r\n" +
		".leading dot\r\n" +
		"--B\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<b>hi</b>\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n" +
		"\r\n" +
		"aGVsbG8gYXR0YWNobWVudA==\r\n" +
		"--B--\r\n"

	res, err := client.Send(context.Background(), DeliveryRequest{
		Address: ln.Addr().String(),
		Envelope: mail.Envelope{
			MailFrom:   "<bounce@mailx.test>",
			Recipients: []string{"<bob@example.com>", "<hidden-bcc@example.com>"},
		},
		Raw: raw,
	})
	if err != nil || !res.Accepted {
		t.Fatalf("delivery failed: %v %+v", err, res)
	}

	// Wait for sink completion — Save runs synchronously in the sink.
	deadline := time.Now().Add(2 * time.Second)
	var ids []string
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(filepath.Join(dir, "messages"))
		if err == nil && len(entries) == 1 {
			for _, e := range entries {
				ids = append(ids, e.Name())
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 stored message, got %v", ids)
	}

	loaded, err := store.Load(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.Envelope.MailFrom != "<bounce@mailx.test>" {
		t.Fatalf("envelope MAIL FROM lost: %+v", loaded.Metadata.Envelope)
	}
	if len(loaded.Metadata.Envelope.RcptTo) != 2 || loaded.Metadata.Envelope.RcptTo[1] != "<hidden-bcc@example.com>" {
		t.Fatalf("envelope recipients not preserved: %v", loaded.Metadata.Envelope.RcptTo)
	}
	if loaded.Metadata.Message.From != "Alice <alice@example.com>" {
		t.Fatalf("From header lost: %q", loaded.Metadata.Message.From)
	}
	if len(loaded.Metadata.Attachments) != 1 || loaded.Metadata.Attachments[0].Filename != "note.txt" {
		t.Fatalf("attachment metadata lost: %+v", loaded.Metadata.Attachments)
	}
	// message.eml should contain the unstuffed dot line, proving dot-stuffing was reversed on the receiver.
	eml, err := os.ReadFile(filepath.Join(dir, "messages", ids[0], "message.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(eml), "\r\n.leading dot\r\n") {
		t.Fatalf("expected unstuffed '.leading dot' line in stored eml; got:\n%s", string(eml))
	}
	// The attachment bytes should decode to "hello attachment".
	att, err := os.ReadFile(filepath.Join(dir, "messages", ids[0], "attachments", "0001.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(att) != "hello attachment" {
		t.Fatalf("attachment content wrong: %q", string(att))
	}
}

func FuzzReplyParser(f *testing.F) {
	seeds := []string{
		"220 ok",
		"250-example",
		"250 ok",
		"550 5.1.1 User unknown",
		"421 4.3.0 try later",
		"",
		"2",
		"25",
		"999 out of range",
		"250/bad separator",
		"250-\r\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _, _, _ = parseReplyLine(s)
		if len(s) < 3 {
			return
		}
		if code, _, _, err := parseReplyLine(s); err == nil {
			_, _ = splitEnhanced(code, "")
		}
	})
}

// asDelivery is a small typed unwrap helper (errors.As with a fresh var).
func asDelivery(err error, out **DeliveryError) bool {
	for cur := err; cur != nil; {
		if d, ok := cur.(*DeliveryError); ok {
			*out = d
			return true
		}
		u, ok := cur.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}

// TestClientSourceIPIsWiredIntoDialer proves ClientConfig.SourceIP actually
// binds the outbound connection's local address, rather than merely being
// recorded. A second loopback alias (e.g. 127.0.0.2) isn't reliably bindable
// on every dev machine (macOS doesn't alias all of 127.0.0.0/8 to lo0 the way
// Linux does), so instead this uses a TEST-NET-3 address (RFC 5737,
// 203.0.113.0/24) that is guaranteed to never be a local address anywhere. If
// SourceIP were ignored by the dialer, the dial would proceed normally and
// fail only on connect/timeout; because it's wired into net.Dialer.LocalAddr,
// the OS rejects the bind before any connection attempt is made.
func TestClientSourceIPIsWiredIntoDialer(t *testing.T) {
	srv := startScripted(t, normalHandler)
	defer srv.stop()

	cfg := testClientConfig()
	cfg.SourceIP = net.ParseIP("203.0.113.1")
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(),
		Envelope: mail.Envelope{
			MailFrom:   "<alice@example.com>",
			Recipients: []string{"<bob@example.com>"},
		},
		Raw: "From: <alice@example.com>\r\nTo: <bob@example.com>\r\nSubject: hi\r\n\r\nbody\r\n",
	})
	if err == nil {
		t.Fatal("expected dial to fail binding an unassigned SourceIP, got nil error")
	}
	if !strings.Contains(err.Error(), "assign") && !strings.Contains(err.Error(), "bind") {
		t.Fatalf("expected a bind/assign error proving SourceIP reached the dialer, got: %v", err)
	}
}

// TestClientSourceIPLoopbackStillWorks confirms the common case: an explicit
// SourceIP that IS locally assignable (127.0.0.1, always bindable) still
// completes a normal delivery end-to-end.
func TestClientSourceIPLoopbackStillWorks(t *testing.T) {
	srv := startScripted(t, normalHandler)
	defer srv.stop()

	cfg := testClientConfig()
	cfg.SourceIP = net.ParseIP("127.0.0.1")
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Send(context.Background(), DeliveryRequest{
		Address: srv.address(),
		Envelope: mail.Envelope{
			MailFrom:   "<alice@example.com>",
			Recipients: []string{"<bob@example.com>"},
		},
		Raw: "From: <alice@example.com>\r\nTo: <bob@example.com>\r\nSubject: hi\r\n\r\nbody\r\n",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !res.Accepted || res.FinalCode != 250 {
		t.Fatalf("unexpected result: %+v", res)
	}
}
