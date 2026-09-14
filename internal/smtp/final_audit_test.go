package smtp

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func TestFinalAuditStateMachineOrdering(t *testing.T) {
	var received Session
	server, client, reader, done := startSMTPForSizeTest(t, 4096, func(session Session, _ mail.Message) error {
		received = session
		return nil
	})
	defer server.Close()

	for _, step := range []struct {
		command string
		reply   string
	}{
		{"MAIL FROM:<early@example.com>", "503"},
		{"RCPT TO:<early@example.com>", "503"},
		{"DATA", "503"},
		{"RSET", "250"},
		{"NOOP", "250"},
		{"EHLO audit.example", "250"},
		{"RCPT TO:<before-mail@example.com>", "503"},
		{"DATA", "503"},
		{"MAIL FROM:<stale@example.com>", "250"},
		{"MAIL FROM:<replacement@example.com>", "503"},
		{"RCPT TO:<first@example.com>", "250"},
		{"RCPT TO:<second@example.com>", "250"},
		{"RSET", "250"},
		{"RCPT TO:<after-reset@example.com>", "503"},
		{"NOOP", "250"},
		{"MAIL FROM:<final@example.com>", "250"},
		{"RCPT TO:<final-recipient@example.com>", "250"},
		{"DATA", "354"},
	} {
		writeSMTP(t, client, step.command+"\r\n")
		expectRawCRLF(t, reader, step.reply)
		if step.command == "EHLO audit.example" {
			expectRawCRLF(t, reader, "250")
		}
	}
	writeSMTP(t, client, "Subject: final\r\n\r\nbody\r\n.\r\n")
	expectRawCRLF(t, reader, "250")
	quitSMTP(t, client, reader, done)

	if received.Envelope.MailFrom != "<final@example.com>" {
		t.Fatalf("stale sender leaked into accepted transaction: %#v", received.Envelope)
	}
	if len(received.Envelope.Recipients) != 1 || received.Envelope.Recipients[0] != "<final-recipient@example.com>" {
		t.Fatalf("stale recipients leaked into accepted transaction: %#v", received.Envelope)
	}
}

func TestFinalAuditMixedTransactionsOnOneConnection(t *testing.T) {
	type acceptedMessage struct {
		sender     string
		recipients []string
		subject    string
		raw        string
	}
	var accepted []acceptedMessage
	sink := func(session Session, message mail.Message) error {
		if message.Subject == "storage-fail" {
			return errors.New("write /private/mailx/secret: device unavailable")
		}
		accepted = append(accepted, acceptedMessage{
			sender:     session.Envelope.MailFrom,
			recipients: append([]string(nil), session.Envelope.Recipients...),
			subject:    message.Subject,
			raw:        message.Raw,
		})
		return nil
	}

	server, client, reader, done := startSMTPForSizeTest(t, 128, sink)
	defer server.Close()
	writeSMTP(t, client, "EHLO audit.example\r\n")
	expectEHLO(t, reader)

	sendAuditTransaction(t, client, reader, "one@example.com", "one-recipient@example.com", "Subject: valid-1\r\n\r\none\r\n", "250")
	sendAuditTransaction(t, client, reader, "two@example.com", "two-recipient@example.com", "Subject: valid-2\r\n\r\ntwo\r\n", "250")

	writeSMTP(t, client, "MAIL FROM:broken\r\n")
	expectRawCRLF(t, reader, "501")
	sendAuditTransaction(t, client, reader, "three@example.com", "three-recipient@example.com", "Subject: after-invalid\r\n\r\nthree\r\n", "250")

	sendAuditTransaction(t, client, reader, "large@example.com", "old@example.com", "Subject: oversized\r\n\r\n"+strings.Repeat("x", 256)+"\r\n", "552")
	sendAuditTransaction(t, client, reader, "four@example.com", "four-recipient@example.com", "Subject: after-oversize\r\n\r\nfour\r\n", "250")

	sendAuditTransaction(t, client, reader, "bad@example.com", "old@example.com", "Subject: malformed\n\nbody\n", "554")
	sendAuditTransaction(t, client, reader, "five@example.com", "five-recipient@example.com", "Subject: after-malformed\r\n\r\nfive\r\n", "250")

	sendAuditTransaction(t, client, reader, "parser@example.com", "old@example.com", "", "554")
	sendAuditTransaction(t, client, reader, "six@example.com", "six-recipient@example.com", "Subject: after-parser\r\n\r\nsix\r\n", "250")

	sendAuditTransaction(t, client, reader, "storage@example.com", "old@example.com", "Subject: storage-fail\r\n\r\nbody\r\n", "451")
	sendAuditTransaction(t, client, reader, "seven@example.com", "seven-recipient@example.com", "Subject: after-storage\r\n\r\nseven\r\n", "250")

	sendAuditTransaction(t, client, reader, "dot@example.com", "dot-recipient@example.com", "Subject: dot\r\n\r\n..leading\r\n", "250")
	quitSMTP(t, client, reader, done)

	wantSubjects := []string{"valid-1", "valid-2", "after-invalid", "after-oversize", "after-malformed", "after-parser", "after-storage", "dot"}
	if len(accepted) != len(wantSubjects) {
		t.Fatalf("accepted messages=%d, want %d: %#v", len(accepted), len(wantSubjects), accepted)
	}
	for index, wantSubject := range wantSubjects {
		message := accepted[index]
		if message.subject != wantSubject {
			t.Fatalf("accepted[%d] subject=%q, want %q", index, message.subject, wantSubject)
		}
		wantSender := "<" + map[string]string{
			"valid-1":         "one@example.com",
			"valid-2":         "two@example.com",
			"after-invalid":   "three@example.com",
			"after-oversize":  "four@example.com",
			"after-malformed": "five@example.com",
			"after-parser":    "six@example.com",
			"after-storage":   "seven@example.com",
			"dot":             "dot@example.com",
		}[wantSubject] + ">"
		if message.sender != wantSender || len(message.recipients) != 1 {
			t.Fatalf("accepted[%d] has stale envelope: %#v", index, message)
		}
	}
	if got := accepted[len(accepted)-1].raw; got != "Subject: dot\r\n\r\n.leading\r\n" {
		t.Fatalf("dot transparency changed during mixed transactions: %q", got)
	}
}

func TestFinalAuditConnectionSlotReleasedDuringDATA(t *testing.T) {
	server, listener := startLimitedSMTPServer(t, limitedConfig(1), nil)
	client, reader := connectLimitedClient(t, listener)
	expectSMTPBanner(t, client, reader)
	beginDataCommandsForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
	writeSMTP(t, client, "Subject: disconnected\r\n\r\npartial")
	client.Close()
	waitForActiveConnections(t, server, 0)

	replacement, replacementReader := connectLimitedClient(t, listener)
	expectSMTPBanner(t, replacement, replacementReader)
	writeSMTP(t, replacement, "QUIT\r\n")
	expectRawCRLF(t, replacementReader, "221")
	waitForActiveConnections(t, server, 0)
}

func TestFinalAuditServerConfigurationsRemainIndependent(t *testing.T) {
	raw := "Subject: isolation\r\n\r\nbody\r\n"
	smallConfig := limitedConfig(1)
	smallConfig.MaxMessageSize = int64(len(raw) - 1)
	largeConfig := limitedConfig(1)
	largeConfig.MaxMessageSize = int64(len(raw))

	var smallCalls, largeCalls int
	smallServer, smallListener := startLimitedSMTPServer(t, smallConfig, func(Session, mail.Message) error {
		smallCalls++
		return nil
	})
	largeServer, largeListener := startLimitedSMTPServer(t, largeConfig, func(Session, mail.Message) error {
		largeCalls++
		return nil
	})

	smallClient, smallReader := connectLimitedClient(t, smallListener)
	largeClient, largeReader := connectLimitedClient(t, largeListener)
	expectSMTPBanner(t, smallClient, smallReader)
	expectSMTPBanner(t, largeClient, largeReader)
	beginDataCommandsForSizeTest(t, smallClient, smallReader, "small@example.com", "recipient@example.com")
	beginDataCommandsForSizeTest(t, largeClient, largeReader, "large@example.com", "recipient@example.com")
	writeSMTP(t, smallClient, raw+".\r\n")
	expectRawCRLF(t, smallReader, "552")
	writeSMTP(t, largeClient, raw+".\r\n")
	expectRawCRLF(t, largeReader, "250")
	writeSMTP(t, smallClient, "QUIT\r\n")
	expectRawCRLF(t, smallReader, "221")
	writeSMTP(t, largeClient, "QUIT\r\n")
	expectRawCRLF(t, largeReader, "221")
	waitForActiveConnections(t, smallServer, 0)
	waitForActiveConnections(t, largeServer, 0)

	if smallCalls != 0 || largeCalls != 1 {
		t.Fatalf("cross-instance message-size leakage: small=%d large=%d", smallCalls, largeCalls)
	}
}

func FuzzDataFramingDoesNotPanic(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("Subject: valid\r\n\r\nbody\r\n"),
		[]byte("Subject: malformed\n\nbody\n"),
		[]byte("..dot\r\n"),
		bytes.Repeat([]byte("x"), 2048),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16*1024 {
			t.Skip()
		}
		input := append(append([]byte(nil), data...), []byte("\r\n.\r\n")...)
		conn := &staticConn{reader: bytes.NewReader(input)}
		connection := connection{
			conn:   conn,
			config: testSMTPConfig(1024),
			reader: bufio.NewReaderSize(conn, DefaultCommandLineLimit),
		}
		result, _ := connection.readData()
		if !result.oversized && int64(len(result.raw)) > connection.config.MaxMessageSize {
			t.Fatalf("accepted %d bytes with limit %d", len(result.raw), connection.config.MaxMessageSize)
		}
	})
}

func TestFinalAuditRecipientCap(t *testing.T) {
	config := Config{CommandLineLimit: 512, MaxMessageSize: 4096, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRecipients: 3}
	server, client, reader, done := startSMTPWithConfigForSizeTest(t, config, func(Session, mail.Message) error { return nil })
	defer server.Close()

	for _, step := range []struct {
		command string
		reply   string
	}{
		{"EHLO test", "250"},
		{"MAIL FROM:<sender@example.com>", "250"},
		{"RCPT TO:<a@example.com>", "250"},
		{"RCPT TO:<b@example.com>", "250"},
		{"RCPT TO:<c@example.com>", "250"},
		{"RCPT TO:<d@example.com>", "452"},
		{"RCPT TO:<e@example.com>", "452"},
	} {
		writeSMTP(t, client, step.command+"\r\n")
		expectRawCRLF(t, reader, step.reply)
		if step.command == "EHLO test" {
			expectRawCRLF(t, reader, "250")
		}
	}
	quitSMTP(t, client, reader, done)
}

func sendAuditTransaction(t *testing.T, client net.Conn, reader *bufio.Reader, sender, recipient, raw, wantReply string) {
	t.Helper()
	writeSMTP(t, client, "MAIL FROM:<"+sender+">\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "RCPT TO:<"+recipient+">\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "DATA\r\n")
	expectRawCRLF(t, reader, "354")
	writeSMTP(t, client, raw+".\r\n")
	expectRawCRLF(t, reader, wantReply)
}

type staticConn struct {
	reader *bytes.Reader
}

func (c *staticConn) Read(content []byte) (int, error)  { return c.reader.Read(content) }
func (c *staticConn) Write(content []byte) (int, error) { return len(content), nil }
func (c *staticConn) Close() error                      { return nil }
func (c *staticConn) LocalAddr() net.Addr               { return testAddr("local") }
func (c *staticConn) RemoteAddr() net.Addr              { return testAddr("remote") }
func (c *staticConn) SetDeadline(time.Time) error       { return nil }
func (c *staticConn) SetReadDeadline(time.Time) error   { return nil }
func (c *staticConn) SetWriteDeadline(time.Time) error  { return nil }

var _ net.Conn = (*staticConn)(nil)
