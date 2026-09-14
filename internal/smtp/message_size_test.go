package smtp

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func TestMessageSizeBoundaries(t *testing.T) {
	message := "Subject: boundary\r\n\r\none\r\n\r\ntwo\r\n"
	tests := []struct {
		name       string
		raw        string
		limit      int64
		wantReply  string
		wantStored bool
	}{
		{name: "empty DATA reaches parser", raw: "", limit: 1, wantReply: "554"},
		{name: "tiny DATA", raw: "\r\n", limit: 2, wantReply: "250", wantStored: true},
		{name: "one byte below maximum", raw: message, limit: int64(len(message) + 1), wantReply: "250", wantStored: true},
		{name: "exactly maximum", raw: message, limit: int64(len(message)), wantReply: "250", wantStored: true},
		{name: "one byte above maximum", raw: message, limit: int64(len(message) - 1), wantReply: "552"},
		{name: "substantially above maximum", raw: message + strings.Repeat("x", 128) + "\r\n", limit: 8, wantReply: "552"},
		{name: "multiple and blank lines at boundary", raw: "Subject: x\r\n\r\na\r\n\r\nb\r\n", limit: int64(len("Subject: x\r\n\r\na\r\n\r\nb\r\n")), wantReply: "250", wantStored: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var messages []mail.Message
			server, client, reader, done := startSMTPForSizeTest(t, test.limit, func(_ Session, message mail.Message) error {
				messages = append(messages, message)
				return nil
			})
			defer server.Close()
			beginDataForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
			writeSMTP(t, client, test.raw+".\r\n")
			expectRawCRLF(t, reader, test.wantReply)
			writeSMTP(t, client, "QUIT\r\n")
			expectRawCRLF(t, reader, "221")
			client.Close()
			waitSMTP(t, done)

			if got := len(messages); got != boolInt(test.wantStored) {
				t.Fatalf("sink calls=%d, want %d", got, boolInt(test.wantStored))
			}
			if test.wantStored && messages[0].Raw != test.raw {
				t.Fatalf("raw changed:\n got %q\nwant %q", messages[0].Raw, test.raw)
			}
		})
	}
}

func TestMessageSizeCountsCanonicalDotUnstuffedBytes(t *testing.T) {
	wire := "Subject: dots\r\n\r\n..leading\r\n"
	canonical := "Subject: dots\r\n\r\n.leading\r\n"
	var received mail.Message
	server, client, reader, done := startSMTPForSizeTest(t, int64(len(canonical)), func(_ Session, message mail.Message) error {
		received = message
		return nil
	})
	defer server.Close()
	beginDataForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
	writeSMTP(t, client, wire+".\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "QUIT\r\n")
	expectRawCRLF(t, reader, "221")
	client.Close()
	waitSMTP(t, done)
	if received.Raw != canonical {
		t.Fatalf("dot-unstuffed raw=%q, want %q", received.Raw, canonical)
	}
}

func TestOversizedMessageDrainsAndNextTransactionPersists(t *testing.T) {
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	validRaw := "Subject: accepted\r\n\r\nok\r\n"
	var sinkCalls int
	server, client, reader, done := startSMTPForSizeTest(t, int64(len(validRaw)), func(session Session, message mail.Message) error {
		sinkCalls++
		record, err := storage.NewMessageRecord(session.Envelope, message)
		if err != nil {
			return err
		}
		return store.Save(record)
	})
	defer server.Close()

	beginDataForSizeTest(t, client, reader, "first@example.com", "old@example.com")
	writeSMTP(t, client, "Subject: rejected\r\n\r\n"+strings.Repeat("x", 256)+"\r\n.\r\n")
	expectRawCRLF(t, reader, "552")

	beginTransactionForSizeTest(t, client, reader, "second@example.com", "new@example.com")
	writeSMTP(t, client, "DATA\r\n")
	expectRawCRLF(t, reader, "354")
	writeSMTP(t, client, validRaw+".\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "QUIT\r\n")
	expectRawCRLF(t, reader, "221")
	client.Close()
	waitSMTP(t, done)

	if sinkCalls != 1 {
		t.Fatalf("sink calls=%d, want 1", sinkCalls)
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("stored records=%d, want 1", len(records))
	}
	if records[0].Envelope.MailFrom != "<second@example.com>" || len(records[0].Envelope.RcptTo) != 1 || records[0].Envelope.RcptTo[0] != "<new@example.com>" {
		t.Fatalf("stale envelope reached storage: %#v", records[0].Envelope)
	}
	stored, err := store.Load(records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Raw) != validRaw {
		t.Fatalf("stored raw=%q, want %q", stored.Raw, validRaw)
	}
}

func TestOversizedDATAFailurePaths(t *testing.T) {
	t.Run("client disconnect while draining", func(t *testing.T) {
		var sinkCalls int
		server, client, reader, done := startSMTPForSizeTest(t, 16, func(Session, mail.Message) error {
			sinkCalls++
			return nil
		})
		defer server.Close()
		beginDataForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
		writeSMTP(t, client, strings.Repeat("x", 128)+"\r\n")
		client.Close()
		waitSMTP(t, done)
		if sinkCalls != 0 {
			t.Fatalf("sink called %d times", sinkCalls)
		}
	})

	t.Run("timeout while draining", func(t *testing.T) {
		var sinkCalls int
		config := Config{CommandLineLimit: 512, MaxMessageSize: 16, ReadTimeout: 50 * time.Millisecond, WriteTimeout: time.Second}
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- HandleConnectionWithConfig(server, config, func(Session, mail.Message) error {
				sinkCalls++
				return nil
			})
		}()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		beginDataCommandsForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
		writeSMTP(t, client, strings.Repeat("x", 128)+"\r\n")
		waitSMTP(t, done)
		client.Close()
		if sinkCalls != 0 {
			t.Fatalf("sink called %d times", sinkCalls)
		}
	})

	t.Run("large line without newline remains bounded", func(t *testing.T) {
		var sinkCalls int
		config := Config{CommandLineLimit: 512, MaxMessageSize: 32, ReadTimeout: 50 * time.Millisecond, WriteTimeout: time.Second}
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- HandleConnectionWithConfig(server, config, func(Session, mail.Message) error {
				sinkCalls++
				return nil
			})
		}()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		beginDataCommandsForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
		writeDone := make(chan struct{})
		go func() {
			_, _ = client.Write([]byte(strings.Repeat("x", 64*1024)))
			close(writeDone)
		}()
		select {
		case <-writeDone:
		case <-time.After(time.Second):
			t.Fatal("server stopped draining the oversized line")
		}
		waitSMTP(t, done)
		client.Close()
		if sinkCalls != 0 {
			t.Fatalf("sink called %d times", sinkCalls)
		}
	})

	t.Run("rejection uses write deadline", func(t *testing.T) {
		config := Config{CommandLineLimit: 512, MaxMessageSize: 16, ReadTimeout: time.Second, WriteTimeout: 50 * time.Millisecond}
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- HandleConnectionWithConfig(server, config, nil) }()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		beginDataCommandsForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
		writeSMTP(t, client, strings.Repeat("x", 128)+"\r\n.\r\n")

		// Do not read the 552 response. net.Pipe has no write buffering, so the
		// server can return only when its configured write deadline expires.
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("blocked 552 write returned no error")
			}
		case <-time.After(time.Second):
			t.Fatal("blocked 552 write did not time out")
		}
		client.Close()
	})
}

func TestMessageSizeConfigIsIndependentAcrossConnections(t *testing.T) {
	config := Config{CommandLineLimit: 512, MaxMessageSize: 32, ReadTimeout: time.Second, WriteTimeout: time.Second}
	var mu sync.Mutex
	accepted := 0
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if accepted != 3 {
			t.Errorf("accepted messages=%d, want 3", accepted)
		}
	})

	for _, test := range []struct {
		name      string
		prelude   string
		raw       string
		wantRaw   string
		wantReply string
	}{
		{name: "valid", raw: "Subject: valid\r\n\r\nok\r\n", wantRaw: "Subject: valid\r\n\r\nok\r\n", wantReply: "250"},
		{name: "dot stuffed", raw: "Subject: dots\r\n\r\n..dot\r\n", wantRaw: "Subject: dots\r\n\r\n.dot\r\n", wantReply: "250"},
		{name: "malformed then recovered", prelude: "EHLO malformed\n", raw: "Subject: recovered\r\n\r\nok\r\n", wantRaw: "Subject: recovered\r\n\r\nok\r\n", wantReply: "250"},
		{name: "oversized", raw: "Subject: large\r\n\r\n" + strings.Repeat("x", 128) + "\r\n", wantReply: "552"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var receivedRaw string
			server, client, reader, done := startSMTPWithConfigForSizeTest(t, config, func(_ Session, message mail.Message) error {
				mu.Lock()
				accepted++
				mu.Unlock()
				receivedRaw = message.Raw
				return nil
			})
			defer server.Close()
			if test.prelude != "" {
				writeSMTP(t, client, test.prelude)
				expectRawCRLF(t, reader, "500")
			}
			beginDataForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
			writeSMTP(t, client, test.raw+".\r\n")
			expectRawCRLF(t, reader, test.wantReply)
			writeSMTP(t, client, "QUIT\r\n")
			expectRawCRLF(t, reader, "221")
			client.Close()
			waitSMTP(t, done)
			if receivedRaw != test.wantRaw {
				t.Fatalf("raw=%q, want %q", receivedRaw, test.wantRaw)
			}
		})
	}
}

func startSMTPForSizeTest(t *testing.T, maxMessageSize int64, sink func(Session, mail.Message) error) (net.Conn, net.Conn, *bufio.Reader, <-chan error) {
	t.Helper()
	config := Config{CommandLineLimit: 512, MaxMessageSize: maxMessageSize, ReadTimeout: time.Second, WriteTimeout: time.Second}
	return startSMTPWithConfigForSizeTest(t, config, sink)
}

func startSMTPWithConfigForSizeTest(t *testing.T, config Config, sink func(Session, mail.Message) error) (net.Conn, net.Conn, *bufio.Reader, <-chan error) {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- HandleConnectionWithConfig(server, config, sink) }()
	reader := bufio.NewReader(client)
	expectRawCRLF(t, reader, "220")
	return server, client, reader, done
}

func beginDataForSizeTest(t *testing.T, client net.Conn, reader *bufio.Reader, sender, recipient string) {
	t.Helper()
	beginDataCommandsForSizeTest(t, client, reader, sender, recipient)
}

func beginDataCommandsForSizeTest(t *testing.T, client net.Conn, reader *bufio.Reader, sender, recipient string) {
	t.Helper()
	for _, step := range []struct {
		command string
		reply   string
	}{
		{"EHLO test", "250"},
		{"MAIL FROM:<" + sender + ">", "250"},
		{"RCPT TO:<" + recipient + ">", "250"},
		{"DATA", "354"},
	} {
		writeSMTP(t, client, step.command+"\r\n")
		expectRawCRLF(t, reader, step.reply)
		if step.command == "EHLO test" {
			expectRawCRLF(t, reader, "250")
		}
	}
}

func beginTransactionForSizeTest(t *testing.T, client net.Conn, reader *bufio.Reader, sender, recipient string) {
	t.Helper()
	for _, command := range []string{"MAIL FROM:<" + sender + ">", "RCPT TO:<" + recipient + ">"} {
		writeSMTP(t, client, command+"\r\n")
		expectRawCRLF(t, reader, "250")
	}
}

func writeSMTP(t *testing.T, conn net.Conn, value string) {
	t.Helper()
	if _, err := conn.Write([]byte(value)); err != nil {
		t.Fatalf("SMTP write failed: %v", err)
	}
}

func waitSMTP(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP server did not stop")
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
