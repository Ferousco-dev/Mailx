package smtp

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func plain(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
}

func expectAuthEHLO(t *testing.T, r *bufio.Reader) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "250 ") {
			return
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatalf("unexpected EHLO reply line=%q", line)
		}
	}
}

func TestAuthRequiredBeforeMail(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	cfg := DefaultConfig()
	cfg.RequireAuth = true
	cfg.Authenticator = func(user, pass string) (string, bool) { return "t1", pass == "good" }
	done := make(chan struct{})
	go func() { HandleConnectionWithConfig(srv, cfg, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Write([]byte("EHLO client\r\n"))
	expectAuthEHLO(t, r)
	cli.Write([]byte("MAIL FROM:<a@example.com>\r\n"))
	expect(t, r, "530")
	cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
}

func TestAuthPlainSuccessAllowsMail(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	cfg := DefaultConfig()
	cfg.RequireAuth = true
	var gotTenant string
	var gotUser string
	cfg.Authenticator = func(user, pass string) (string, bool) { gotUser = user; return "t1", pass == "good" }
	var got Session
	done := make(chan struct{})
	go func() {
		HandleConnectionWithConfig(srv, cfg, func(s Session, m mail.Message) error { got = s; return nil })
		close(done)
	}()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Write([]byte("EHLO client\r\n"))
	expectAuthEHLO(t, r)
	cli.Write([]byte("AUTH PLAIN " + plain("user1", "good") + "\r\n"))
	expect(t, r, "235")
	for _, c := range []string{"MAIL FROM:<a@example.com>", "RCPT TO:<b@example.com>", "DATA"} {
		cli.Write([]byte(c + "\r\n"))
		p := "250"
		if c == "DATA" {
			p = "354"
		}
		expect(t, r, p)
	}
	cli.Write([]byte("Subject: hi\r\n\r\nbody\r\n.\r\n"))
	expect(t, r, "250")
	cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
	gotTenant = got.TenantID
	if gotUser != "user1" || gotTenant != "t1" || !got.Authenticated {
		t.Fatalf("unexpected session: user=%q tenant=%q auth=%v", gotUser, gotTenant, got.Authenticated)
	}
}

func TestAuthPlainWrongCredentialsRejected(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	cfg := DefaultConfig()
	cfg.RequireAuth = true
	cfg.Authenticator = func(user, pass string) (string, bool) { return "", false }
	done := make(chan struct{})
	go func() { HandleConnectionWithConfig(srv, cfg, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Write([]byte("EHLO client\r\n"))
	expectAuthEHLO(t, r)
	cli.Write([]byte("AUTH PLAIN " + plain("user1", "bad") + "\r\n"))
	expect(t, r, "535")
	cli.Write([]byte("MAIL FROM:<a@example.com>\r\n"))
	expect(t, r, "530")
	cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
}

func TestAuthMalformedBase64Rejected(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	cfg := DefaultConfig()
	cfg.RequireAuth = true
	cfg.Authenticator = func(string, string) (string, bool) { return "", true }
	done := make(chan struct{})
	go func() { HandleConnectionWithConfig(srv, cfg, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Write([]byte("EHLO client\r\n"))
	expectAuthEHLO(t, r)
	cli.Write([]byte("AUTH PLAIN not-base64!!\r\n"))
	expect(t, r, "501")
	cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
}

func TestAuthNotRequiredUnaffected(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() { HandleConnection(srv, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Write([]byte("EHLO client\r\n"))
	expectAuthEHLO(t, r)
	cli.Write([]byte("MAIL FROM:<a@example.com>\r\n"))
	expect(t, r, "250")
	cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
}
