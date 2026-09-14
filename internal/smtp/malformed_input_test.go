package smtp

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func TestStrictCommandCRLFFraming(t *testing.T) {
	t.Run("LF only is rejected without executing", func(t *testing.T) {
		server, client, reader, done := startSMTPForSizeTest(t, 1024, nil)
		defer server.Close()
		writeSMTP(t, client, "EHLO invalid\n")
		expectRawCRLF(t, reader, "500")
		writeSMTP(t, client, "MAIL FROM:<sender@example.com>\r\n")
		expectRawCRLF(t, reader, "503")
		writeSMTP(t, client, "EHLO valid.example\r\n")
		expectEHLO(t, reader)
		quitSMTP(t, client, reader, done)
	})

	t.Run("bare CR cannot inject a command", func(t *testing.T) {
		server, client, reader, done := startSMTPForSizeTest(t, 1024, nil)
		defer server.Close()
		writeSMTP(t, client, "EHLO invalid\rMAIL FROM:<injected@example.com>\r\n")
		expectRawCRLF(t, reader, "500")
		writeSMTP(t, client, "RCPT TO:<recipient@example.com>\r\n")
		expectRawCRLF(t, reader, "503")
		quitSMTP(t, client, reader, done)
	})

	t.Run("bare CR waits for deadline", func(t *testing.T) {
		server, client := net.Pipe()
		done := make(chan error, 1)
		config := Config{CommandLineLimit: 512, MaxMessageSize: 1024, ReadTimeout: 50 * time.Millisecond, WriteTimeout: time.Second}
		go func() { done <- HandleConnectionWithConfig(server, config, nil) }()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		writeSMTP(t, client, "EHLO incomplete\r")
		waitSMTP(t, done)
		client.Close()
	})

	t.Run("EOF without CRLF is never executed", func(t *testing.T) {
		server, client, reader, done := startSMTPForSizeTest(t, 1024, nil)
		defer server.Close()
		writeSMTP(t, client, "EHLO incomplete")
		client.Close()
		waitSMTP(t, done)
		_ = reader
	})
}

func TestGreetingAndControlCommandSyntax(t *testing.T) {
	server, client, reader, done := startSMTPForSizeTest(t, 2048, nil)
	defer server.Close()
	for _, test := range []struct {
		wire  string
		reply string
	}{
		{"\r\n", "500"},
		{" EHLO example.com\r\n", "500"},
		{"\tEHLO example.com\r\n", "500"},
		{"EHLO\r\n", "501"},
		{"EHLO \r\n", "501"},
		{"EHLOexample.com\r\n", "500"},
		{"EHLO example.com   \r\n", "501"},
		{"EHLO example.com extra\r\n", "501"},
		{"HELO\r\n", "501"},
		{"HELO example.com extra\r\n", "501"},
		{"NOOP\x00\r\n", "501"},
		{"EHLO t\xc3\xa9st.example\r\n", "501"},
		{"\xe2\x98\x83\r\n", "500"},
		{"FOO\r\n", "500"},
		{"WAT hello\r\n", "500"},
		{"XYZ123\r\n", "500"},
	} {
		writeSMTP(t, client, test.wire)
		expectRawCRLF(t, reader, test.reply)
	}

	writeSMTP(t, client, "EhLo Client.Example\r\n")
	expectEHLO(t, reader)
	writeSMTP(t, client, "NOOP\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "NOOP hello\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "NOOP hello world   \r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "NOOP   \r\n")
	expectRawCRLF(t, reader, "501")
	writeSMTP(t, client, "QUIT extra\r\n")
	expectRawCRLF(t, reader, "501")
	writeSMTP(t, client, "QUIT   \r\n")
	expectRawCRLF(t, reader, "501")
	writeSMTP(t, client, "NOOP after malformed QUIT\r\n")
	expectRawCRLF(t, reader, "250")
	quitSMTP(t, client, reader, done)
}

func TestMalformedEnvelopeCommandsPreserveState(t *testing.T) {
	var received Session
	server, client, reader, done := startSMTPForSizeTest(t, 2048, func(session Session, _ mail.Message) error {
		received = session
		return nil
	})
	defer server.Close()

	writeSMTP(t, client, "RCPT garbage\r\n")
	expectRawCRLF(t, reader, "501")
	writeSMTP(t, client, "EHLO client.example\r\n")
	expectEHLO(t, reader)
	writeSMTP(t, client, "RCPT TO:<early@example.com>\r\n")
	expectRawCRLF(t, reader, "503")

	for _, command := range []string{
		"MAIL FROM:",
		"MAIL",
		"MAIL TO:<wrong@example.com>",
		"MAIL FROM <missing-colon@example.com>",
		"MAIL FROM:<extra@example.com> extra",
		"MAIL FROM:<missing@example.com",
		"MAIL    FROM:<spaces@example.com>",
		"MAIL\tFROM:<tab@example.com>",
	} {
		writeSMTP(t, client, command+"\r\n")
		expectRawCRLF(t, reader, "501")
	}

	writeSMTP(t, client, "MAIL FROM:<>\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "RSET\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "MAIL FROM:<Good.Sender@example.com>\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "WAT reset-transaction\r\n")
	expectRawCRLF(t, reader, "500")
	writeSMTP(t, client, "EHLO\r\n")
	expectRawCRLF(t, reader, "501")

	for _, command := range []string{
		"MAIL FROM:<replacement@example.com> extra",
		"RCPT TO:",
		"RCPT",
		"RCPT FROM:<wrong@example.com>",
		"RCPT TO <missing-colon@example.com>",
		"RCPT TO:<extra@example.com> extra",
		"RCPT TO:<missing@example.com",
		"RCPT TO:<>",
	} {
		writeSMTP(t, client, command+"\r\n")
		expectRawCRLF(t, reader, "501")
	}

	writeSMTP(t, client, "RCPT TO:<Good.Recipient@example.com>\r\n")
	expectRawCRLF(t, reader, "250")
	for _, command := range []string{"DATA ", "DATA extra", "DATA\tfoo"} {
		writeSMTP(t, client, command+"\r\n")
		expectRawCRLF(t, reader, "501")
	}
	writeSMTP(t, client, "DATA\r\n")
	expectRawCRLF(t, reader, "354")
	writeSMTP(t, client, "Subject: recovered\r\n\r\nok\r\n.\r\n")
	expectRawCRLF(t, reader, "250")
	quitSMTP(t, client, reader, done)

	if received.Envelope.MailFrom != "<Good.Sender@example.com>" || len(received.Envelope.Recipients) != 1 || received.Envelope.Recipients[0] != "<Good.Recipient@example.com>" {
		t.Fatalf("malformed command mutated envelope: %#v", received.Envelope)
	}
}

func TestMalformedRSETDoesNotResetTransaction(t *testing.T) {
	server, client, reader, done := startSMTPForSizeTest(t, 1024, nil)
	defer server.Close()
	writeSMTP(t, client, "EHLO client\r\n")
	expectEHLO(t, reader)
	writeSMTP(t, client, "MAIL FROM:<sender@example.com>\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "RSET extra\r\n")
	expectRawCRLF(t, reader, "501")
	writeSMTP(t, client, "RCPT TO:<recipient@example.com>\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "RSET\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "RCPT TO:<recipient@example.com>\r\n")
	expectRawCRLF(t, reader, "503")
	quitSMTP(t, client, reader, done)
}

func TestMalformedDATAFramingIsRejectedAndRecoverable(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{name: "LF only including false terminator", wire: "Subject: invalid\n\nbody\n.\n.\r\n"},
		{name: "bare CR inside line", wire: "Subject: invalid\r\n\r\nbody\rinjected\r\n.\r\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var messages []mail.Message
			server, client, reader, done := startSMTPForSizeTest(t, 2048, func(_ Session, message mail.Message) error {
				messages = append(messages, message)
				return nil
			})
			defer server.Close()
			beginDataForSizeTest(t, client, reader, "bad@example.com", "old@example.com")
			writeSMTP(t, client, test.wire)
			expectRawCRLF(t, reader, "554")

			beginTransactionForSizeTest(t, client, reader, "good@example.com", "new@example.com")
			writeSMTP(t, client, "DATA\r\n")
			expectRawCRLF(t, reader, "354")
			validRaw := "Subject: valid\r\n\r\n..dot\r\n"
			writeSMTP(t, client, validRaw+".\r\n")
			expectRawCRLF(t, reader, "250")
			quitSMTP(t, client, reader, done)

			if len(messages) != 1 || messages[0].Raw != "Subject: valid\r\n\r\n.dot\r\n" {
				t.Fatalf("malformed DATA reached sink or recovery failed: %#v", messages)
			}
		})
	}
}

func TestMalformedCommandFloodRemainsRecoverable(t *testing.T) {
	server, client, reader, done := startSMTPForSizeTest(t, 1024, nil)
	defer server.Close()
	for index := 0; index < 100; index++ {
		writeSMTP(t, client, "UNKNOWN"+strings.Repeat("X", index%8)+"\r\n")
		expectRawCRLF(t, reader, "500")
	}
	writeSMTP(t, client, "EHLO recovered.example\r\n")
	expectEHLO(t, reader)
	quitSMTP(t, client, reader, done)
}

func FuzzCommandDoesNotPanic(f *testing.F) {
	for _, seed := range []string{"", "EHLO example.com", "MAIL FROM:<a@example.com>", " DATA", "NOOP\x00", "QUIT extra", "\xe2\x98\x83"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 1024 {
			t.Skip()
		}
		verb, _, _, valid := command(line)
		if valid && verb == "" {
			t.Fatal("valid command parse returned an empty verb")
		}
		parsePathArgument(line, true, "FROM", true)
		parsePathArgument(line, true, "TO", false)
	})
}

func expectEHLO(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	expectRawCRLF(t, reader, "250-")
	expectRawCRLF(t, reader, "250")
}

func quitSMTP(t *testing.T, client net.Conn, reader *bufio.Reader, done <-chan error) {
	t.Helper()
	writeSMTP(t, client, "QUIT\r\n")
	expectRawCRLF(t, reader, "221")
	client.Close()
	waitSMTP(t, done)
}
