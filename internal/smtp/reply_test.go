package smtp

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func TestReplyWireFormat(t *testing.T) {
	tests := []struct {
		name  string
		reply reply
		want  string
	}{
		{name: "standard", reply: standardReply(220, "localhost ready"), want: "220 localhost ready\r\n"},
		{name: "enhanced", reply: enhancedReply(503, statusInvalidCommand, "Bad sequence of commands"), want: "503 5.5.1 Bad sequence of commands\r\n"},
		{name: "multiline", reply: multilineReply(250, "localhost", "ENHANCEDSTATUSCODES"), want: "250-localhost\r\n250 ENHANCEDSTATUSCODES\r\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wire, err := test.reply.wire()
			if err != nil {
				t.Fatal(err)
			}
			if wire != test.want {
				t.Fatalf("wire=%q, want %q", wire, test.want)
			}
		})
	}
}

func TestReplyRejectsInvalidConstruction(t *testing.T) {
	tests := []reply{
		standardReply(99, "bad code"),
		{code: 250},
		standardReply(250, "line\nfeed"),
		enhancedReply(451, statusSyntaxError, "class mismatch"),
		enhancedReply(500, enhancedStatus("5.05.2"), "leading zero"),
		enhancedReply(500, enhancedStatus("5.x.2"), "non-numeric"),
	}
	for _, response := range tests {
		if wire, err := response.wire(); err == nil {
			t.Fatalf("invalid reply produced wire output %q: %#v", wire, response)
		}
	}
}

func TestReplyCompletesShortWrites(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	conn := connection{
		conn:   &shortWriteConn{Conn: server, maximum: 3},
		config: Config{WriteTimeout: time.Second},
	}
	response := enhancedReply(503, statusInvalidCommand, "Bad sequence of commands")
	want, err := response.wire()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- conn.reply(response) }()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read complete SMTP reply: %v; partial=%q", err, got)
	}
	if string(got) != want {
		t.Fatalf("reply=%q, want %q", got, want)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	server.Close()
}

func TestEnhancedStatusReplyClasses(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- HandleConnectionWithConfig(server, testSMTPConfig(4096), nil) }()
	reader := bufio.NewReader(client)

	expectExactReply(t, reader, "220 localhost MailX SMTP Server\r\n")
	writeSMTP(t, client, "UNKNOWN\r\n")
	expectExactReply(t, reader, "500 5.5.2 Command unrecognized\r\n")
	writeSMTP(t, client, "RCPT garbage\r\n")
	expectExactReply(t, reader, "501 5.5.4 Syntax: RCPT TO:<address>\r\n")
	writeSMTP(t, client, "RCPT TO:<early@example.com>\r\n")
	expectExactReply(t, reader, "503 5.5.1 Bad sequence of commands\r\n")

	// RFC 2034 excludes responses to EHLO and HELO from enhanced statuses.
	writeSMTP(t, client, "EHLO invalid argument\r\n")
	expectExactReply(t, reader, "501 Syntax: EHLO hostname\r\n")
	writeSMTP(t, client, "EHLO client.example\r\n")
	expectExactReply(t, reader, "250-localhost\r\n")
	expectExactReply(t, reader, "250 ENHANCEDSTATUSCODES\r\n")
	writeSMTP(t, client, "MAIL FROM:<sender@example.com>\r\n")
	expectExactReply(t, reader, "250 2.1.0 Sender OK\r\n")
	writeSMTP(t, client, "RCPT TO:<recipient@example.com>\r\n")
	expectExactReply(t, reader, "250 2.1.5 Recipient OK\r\n")
	writeSMTP(t, client, "DATA\r\n")
	expectExactReply(t, reader, "354 End data with <CR><LF>.<CR><LF>\r\n")
	writeSMTP(t, client, "Subject: accepted\r\n\r\nbody\r\n.\r\n")
	expectExactReply(t, reader, "250 2.6.0 Message accepted by MailX\r\n")
	writeSMTP(t, client, "NOOP\r\n")
	expectExactReply(t, reader, "250 2.0.0 OK\r\n")
	writeSMTP(t, client, "QUIT\r\n")
	expectExactReply(t, reader, "221 2.0.0 Bye\r\n")
	client.Close()
	waitSMTP(t, done)
}

func TestEnhancedMessageFailuresAndRecovery(t *testing.T) {
	t.Run("message too large", func(t *testing.T) {
		validRaw := "Subject: x\r\n\r\na\r\n"
		oneByteOver := "Subject: x\r\n\r\nab\r\n"
		server, client, reader, done := startSMTPForSizeTest(t, int64(len(validRaw)), nil)
		defer server.Close()
		beginDataCommandsForSizeTest(t, client, reader, "first@example.com", "old@example.com")
		writeSMTP(t, client, oneByteOver+".\r\n")
		expectExactReply(t, reader, "552 5.3.4 Message size exceeds fixed limit\r\n")
		completeExactTransaction(t, client, reader, "second@example.com", "new@example.com", validRaw)
		quitSMTP(t, client, reader, done)
	})

	t.Run("malformed DATA framing", func(t *testing.T) {
		var sinkCalls int
		server, client, reader, done := startSMTPForSizeTest(t, 4096, func(Session, mail.Message) error {
			sinkCalls++
			return nil
		})
		defer server.Close()
		beginDataCommandsForSizeTest(t, client, reader, "first@example.com", "old@example.com")
		writeSMTP(t, client, "Subject: malformed\n.\n.\r\n")
		expectExactReply(t, reader, "554 5.6.0 Malformed mail data\r\n")
		completeExactTransaction(t, client, reader, "second@example.com", "new@example.com", "Subject: valid\r\n\r\nok\r\n")
		quitSMTP(t, client, reader, done)
		if sinkCalls != 1 {
			t.Fatalf("sink calls=%d, want 1", sinkCalls)
		}
	})

	t.Run("parser rejection", func(t *testing.T) {
		var sinkCalls int
		server, client, reader, done := startSMTPForSizeTest(t, 4096, func(Session, mail.Message) error {
			sinkCalls++
			return nil
		})
		defer server.Close()
		beginDataCommandsForSizeTest(t, client, reader, "first@example.com", "old@example.com")
		writeSMTP(t, client, ".\r\n")
		expectExactReply(t, reader, "554 5.6.0 Message content rejected\r\n")
		completeExactTransaction(t, client, reader, "second@example.com", "new@example.com", "Subject: valid\r\n\r\nok\r\n")
		quitSMTP(t, client, reader, done)
		if sinkCalls != 1 {
			t.Fatalf("parser-rejected message reached sink; calls=%d", sinkCalls)
		}
	})

	t.Run("temporary storage failure", func(t *testing.T) {
		var sinkCalls int
		server, client, reader, done := startSMTPForSizeTest(t, 4096, func(Session, mail.Message) error {
			sinkCalls++
			if sinkCalls == 1 {
				return errors.New("open /secret/mailx/messages: permission denied")
			}
			return nil
		})
		defer server.Close()
		beginDataCommandsForSizeTest(t, client, reader, "first@example.com", "old@example.com")
		writeSMTP(t, client, "Subject: first\r\n\r\nbody\r\n.\r\n")
		expectExactReply(t, reader, "451 4.3.0 Temporary internal failure\r\n")
		completeExactTransaction(t, client, reader, "second@example.com", "new@example.com", "Subject: second\r\n\r\nbody\r\n")
		quitSMTP(t, client, reader, done)
		if sinkCalls != 2 {
			t.Fatalf("sink calls=%d, want 2", sinkCalls)
		}
	})
}

func completeExactTransaction(t *testing.T, client net.Conn, reader *bufio.Reader, sender, recipient, raw string) {
	t.Helper()
	writeSMTP(t, client, "MAIL FROM:<"+sender+">\r\n")
	expectExactReply(t, reader, "250 2.1.0 Sender OK\r\n")
	writeSMTP(t, client, "RCPT TO:<"+recipient+">\r\n")
	expectExactReply(t, reader, "250 2.1.5 Recipient OK\r\n")
	writeSMTP(t, client, "DATA\r\n")
	expectExactReply(t, reader, "354 End data with <CR><LF>.<CR><LF>\r\n")
	writeSMTP(t, client, raw+".\r\n")
	expectExactReply(t, reader, "250 2.6.0 Message accepted by MailX\r\n")
}

func expectExactReply(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read SMTP reply: %v", err)
	}
	if line != want {
		t.Fatalf("reply=%q, want %q", line, want)
	}
	if !strings.HasSuffix(line, "\r\n") {
		t.Fatalf("reply is not CRLF terminated: %q", line)
	}
}

func testSMTPConfig(maxMessageSize int64) Config {
	return Config{
		CommandLineLimit: 512,
		ReadTimeout:      time.Second,
		WriteTimeout:     time.Second,
		MaxMessageSize:   maxMessageSize,
	}
}

type shortWriteConn struct {
	net.Conn
	maximum int
}

func (c *shortWriteConn) Write(content []byte) (int, error) {
	if len(content) > c.maximum {
		content = content[:c.maximum]
	}
	return c.Conn.Write(content)
}
