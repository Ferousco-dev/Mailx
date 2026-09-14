package smtp

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func TestSessionStateAndDotStuffing(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	var got Session
	var message mail.Message
	done := make(chan struct{})
	go func() {
		HandleConnection(srv, func(s Session, m mail.Message) error { got, message = s, m; return nil })
		close(done)
	}()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Write([]byte("RCPT TO:<a@example.com>\r\n"))
	expect(t, r, "503")
	for _, c := range []string{"EHLO client", "MAIL FROM:<s@example.com>", "RCPT TO:<a@example.com>", "DATA"} {
		cli.Write([]byte(c + "\r\n"))
		p := "250"
		if c == "DATA" {
			p = "354"
		}
		expect(t, r, p)
	}
	cli.Write([]byte("Subject: test\r\n\r\n..hello\r\n.\r\n"))
	expect(t, r, "250")
	cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
	if got.Envelope.MailFrom != "<s@example.com>" || len(got.Envelope.Recipients) != 1 || message.Subject != "test" || message.Body != ".hello\r\n" || message.Raw != "Subject: test\r\n\r\n.hello\r\n" {
		t.Fatalf("unexpected transaction: %#v %#v", got, message)
	}
}

func TestControlCommandsAndMalformedEnvelope(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() { HandleConnection(srv, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	_, _ = cli.Write([]byte("NOOP\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("RSET\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("MAIL FROM:\r\n"))
	expect(t, r, "503")
	_, _ = cli.Write([]byte("EHLO client\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("MAIL    FROM:\r\n"))
	expect(t, r, "501")
	_, _ = cli.Write([]byte("MAIL\tFROM:\t<s@example.com>\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("NOOP\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("RCPT TO:<a@example.com>\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("RSET\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("RCPT TO:<a@example.com>\r\n"))
	expect(t, r, "503")
	_, _ = cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
}

func TestDATARejectsArgumentsWithoutResettingTransaction(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() { HandleConnection(srv, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	for _, command := range []string{"EHLO client", "MAIL FROM:<sender@example.com>", "RCPT TO:<recipient@example.com>"} {
		_, _ = cli.Write([]byte(command + "\r\n"))
		expect(t, r, "250")
	}
	_, _ = cli.Write([]byte("DATA unexpected\r\n"))
	expect(t, r, "501")
	_, _ = cli.Write([]byte("DATA\r\n"))
	expect(t, r, "354")
	_, _ = cli.Write([]byte("Subject: valid\r\n\r\nbody\r\n.\r\nQUIT\r\n"))
	expect(t, r, "250")
	expect(t, r, "221")
	<-done
}

func TestNewGreetingAndDataResetTransaction(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	var received []Session
	done := make(chan struct{})
	go func() {
		HandleConnection(srv, func(s Session, _ mail.Message) error { received = append(received, s); return nil })
		close(done)
	}()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	for _, step := range []struct {
		command string
		reply   string
	}{
		{"EHLO first", "250"},
		{"MAIL FROM:<old@example.com>", "250"},
		{"RCPT TO:<old-recipient@example.com>", "250"},
		{"HELO second", "250"},
		{"RCPT TO:<new-recipient@example.com>", "503"},
		{"MAIL FROM:<new@example.com>", "250"},
		{"RCPT TO:<new-recipient@example.com>", "250"},
		{"DATA", "354"},
	} {
		_, _ = cli.Write([]byte(step.command + "\r\n"))
		expect(t, r, step.reply)
	}
	_, _ = cli.Write([]byte("\r\nhello\r\n.\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("DATA\r\n"))
	expect(t, r, "503")
	_, _ = cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done

	if len(received) != 1 || received[0].Envelope.MailFrom != "<new@example.com>" || len(received[0].Envelope.Recipients) != 1 || received[0].Envelope.Recipients[0] != "<new-recipient@example.com>" {
		t.Fatalf("unexpected received transaction: %#v", received)
	}
}

func TestDisconnectDuringCommand(t *testing.T) {
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() { HandleConnection(srv, nil); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	cli.Close()
	<-done
}

func TestConnectionsKeepIndependentSessions(t *testing.T) {
	serverOne, clientOne := net.Pipe()
	serverTwo, clientTwo := net.Pipe()
	defer clientOne.Close()
	defer clientTwo.Close()

	done := make(chan struct{}, 2)
	go func() { HandleConnection(serverOne, nil); done <- struct{}{} }()
	go func() { HandleConnection(serverTwo, nil); done <- struct{}{} }()

	readerOne := bufio.NewReader(clientOne)
	readerTwo := bufio.NewReader(clientTwo)
	expect(t, readerOne, "220")
	expect(t, readerTwo, "220")
	_, _ = clientOne.Write([]byte("EHLO one\r\n"))
	expect(t, readerOne, "250")
	_, _ = clientOne.Write([]byte("MAIL FROM:<one@example.com>\r\n"))
	expect(t, readerOne, "250")

	_, _ = clientTwo.Write([]byte("RCPT TO:<two@example.com>\r\n"))
	expect(t, readerTwo, "503")
	_, _ = clientOne.Write([]byte("QUIT\r\n"))
	expect(t, readerOne, "221")
	_, _ = clientTwo.Write([]byte("QUIT\r\n"))
	expect(t, readerTwo, "221")
	<-done
	<-done
}

func TestSMTPPreservesEnvelopeAndParsesMessage(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	var envelope mail.Envelope
	var message mail.Message
	done := make(chan struct{})
	go func() {
		HandleConnection(srv, func(session Session, received mail.Message) error {
			envelope = session.Envelope
			message = received
			return nil
		})
		close(done)
	}()

	r := bufio.NewReader(cli)
	expect(t, r, "220")
	for _, command := range []string{
		"EHLO localhost",
		"MAIL FROM:<bounce@example.com>",
		"RCPT TO:<john@example.com>",
		"DATA",
	} {
		_, _ = cli.Write([]byte(command + "\r\n"))
		if command == "DATA" {
			expect(t, r, "354")
		} else {
			expect(t, r, "250")
		}
	}
	_, _ = cli.Write([]byte("From: Alice <alice@example.com>\r\nTo: John <john@example.com>\r\nCc: Mary <mary@example.com>\r\nSubject: MailX parser test\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nMessage-ID: <test123@mailx.local>\r\n\r\nHello from MailX.\r\nThis is the body.\r\n.\r\n"))
	expect(t, r, "250")
	_, _ = cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done

	if envelope.MailFrom != "<bounce@example.com>" || len(envelope.Recipients) != 1 || envelope.Recipients[0] != "<john@example.com>" {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
	if message.From != "Alice <alice@example.com>" || len(message.To) != 1 || message.To[0] != "John <john@example.com>" || len(message.Cc) != 1 || message.Subject != "MailX parser test" || message.Date == "" || message.MessageID != "<test123@mailx.local>" || message.Body != "Hello from MailX.\r\nThis is the body.\r\n" || message.Raw == "" {
		t.Fatalf("unexpected message: %#v", message)
	}
}

func TestSMTPPersistsRawMessageBeforeAcceptingDATA(t *testing.T) {
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, cli := net.Pipe()
	defer cli.Close()
	var record storage.MessageRecord
	done := make(chan struct{})
	go func() {
		HandleConnection(srv, func(session Session, message mail.Message) error {
			var err error
			record, err = storage.NewMessageRecord(session.Envelope, message)
			if err != nil {
				return err
			}
			return store.Save(record)
		})
		close(done)
	}()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	for _, command := range []string{"EHLO localhost", "MAIL FROM:<sender@example.com>", "RCPT TO:<recipient@example.com>", "DATA"} {
		_, _ = cli.Write([]byte(command + "\r\n"))
		if command == "DATA" {
			expect(t, r, "354")
		} else {
			expect(t, r, "250")
		}
	}
	raw := "Subject: stored\r\nContent-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\n\r\nHello persisted.\r\n" +
		"--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n" +
		"--mailx--\r\n"
	_, _ = cli.Write([]byte(raw + ".\r\nQUIT\r\n"))
	expect(t, r, "250")
	expect(t, r, "221")
	<-done
	stored, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, "message.eml"))
	if err != nil || string(stored) != raw {
		t.Fatalf("SMTP accepted before raw persistence: %q, err=%v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(store.MessagesDir(), record.ID, "metadata.json")); err != nil {
		t.Fatalf("SMTP accepted before metadata persistence: %v", err)
	}
	attachment, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, "attachments", "0001.bin"))
	if err != nil || string(attachment) != "%PDF-1.4\n" {
		t.Fatalf("SMTP accepted before attachment persistence: %q, err=%v", attachment, err)
	}
}

func TestSMTPReturnsTemporaryFailureWhenStorageFails(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() {
		HandleConnection(srv, func(Session, mail.Message) error { return errors.New("disk unavailable") })
		close(done)
	}()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	for _, command := range []string{"EHLO localhost", "MAIL FROM:<sender@example.com>", "RCPT TO:<recipient@example.com>", "DATA"} {
		_, _ = cli.Write([]byte(command + "\r\n"))
		if command == "DATA" {
			expect(t, r, "354")
		} else {
			expect(t, r, "250")
		}
	}
	_, _ = cli.Write([]byte("Subject: failed store\r\n\r\nbody\r\n.\r\nQUIT\r\n"))
	expect(t, r, "451")
	expect(t, r, "221")
	<-done
}
func expect(t *testing.T, r *bufio.Reader, p string) {
	t.Helper()
	line, e := r.ReadString('\n')
	if e != nil || !strings.HasPrefix(line, p) {
		t.Fatalf("reply=%q err=%v", line, e)
	}
	if p == "250" && strings.HasPrefix(line, "250-") {
		line, _ = r.ReadString('\n')
		if !strings.HasPrefix(line, "250") {
			t.Fatalf("multiline=%q", line)
		}
	}
}
