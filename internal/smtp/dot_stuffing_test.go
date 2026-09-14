package smtp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func TestSMTPDotTransparency(t *testing.T) {
	wireBody := "..hello\r\n...hello\r\n....hello\r\n.abc\r\nhello.world\r\n .\r\n\r\n..\r\n...\r\n"
	wantBody := ".hello\r\n..hello\r\n...hello\r\nabc\r\nhello.world\r\n .\r\n\r\n.\r\n..\r\n"
	wire := "Subject: dots\r\nContent-Type: text/plain\r\n\r\n" + wireBody
	wantRaw := "Subject: dots\r\nContent-Type: text/plain\r\n\r\n" + wantBody

	message := receiveSMTPMessage(t, wire, int64(len(wantRaw)))
	if message.Raw != wantRaw {
		t.Fatalf("reconstructed raw changed:\n got %q\nwant %q", message.Raw, wantRaw)
	}
	if message.Body != wantBody || message.TextBody != wantBody {
		t.Fatalf("dot transparency did not precede parsing: body=%q text=%q", message.Body, message.TextBody)
	}
}

func TestSMTPDotTransparencyBeforeMIMEParsing(t *testing.T) {
	t.Run("text html", func(t *testing.T) {
		wire := "Content-Type: text/html; charset=UTF-8\r\n\r\n..<p>Hello.MailX</p>\r\n"
		wantRaw := "Content-Type: text/html; charset=UTF-8\r\n\r\n.<p>Hello.MailX</p>\r\n"
		message := receiveSMTPMessage(t, wire, int64(len(wantRaw)))
		if message.Raw != wantRaw || message.HTMLBody != ".<p>Hello.MailX</p>\r\n" || message.TextBody != "" {
			t.Fatalf("unexpected HTML message: %#v", message)
		}
	})

	t.Run("multipart", func(t *testing.T) {
		wire := "MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=mailx\r\n\r\n" +
			"--mailx\r\nContent-Type: text/plain\r\n\r\n..plain\r\n" +
			"--mailx\r\nContent-Type: text/html\r\n\r\n..<b>html</b>\r\n" +
			"--mailx--\r\n"
		wantRaw := strings.ReplaceAll(wire, "\r\n..", "\r\n.")
		message := receiveSMTPMessage(t, wire, int64(len(wantRaw)))
		if message.Raw != wantRaw || message.TextBody != ".plain" || message.HTMLBody != ".<b>html</b>" {
			t.Fatalf("unexpected multipart message: %#v", message)
		}
	})
}

func TestSMTPDotTransparencyPersistsAndAllowsNextTransaction(t *testing.T) {
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, client, reader, done := startSMTPForSizeTest(t, 1024, func(session Session, message mail.Message) error {
		record, err := storage.NewMessageRecord(session.Envelope, message)
		if err != nil {
			return err
		}
		return store.Save(record)
	})
	defer server.Close()

	firstWire := "Subject: first\r\n\r\n..dot\r\n"
	firstRaw := "Subject: first\r\n\r\n.dot\r\n"
	beginDataForSizeTest(t, client, reader, "first@example.com", "one@example.com")
	writeSMTP(t, client, firstWire+".\r\n")
	expectRawCRLF(t, reader, "250")

	secondRaw := "Subject: second\r\n\r\nnormal\r\n"
	beginTransactionForSizeTest(t, client, reader, "second@example.com", "two@example.com")
	writeSMTP(t, client, "DATA\r\n")
	expectRawCRLF(t, reader, "354")
	writeSMTP(t, client, secondRaw+".\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "QUIT\r\n")
	expectRawCRLF(t, reader, "221")
	client.Close()
	waitSMTP(t, done)

	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("stored records=%d, want 2", len(records))
	}
	want := map[string]string{
		"<first@example.com>":  firstRaw,
		"<second@example.com>": secondRaw,
	}
	for _, record := range records {
		diskRaw, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, "message.eml"))
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.Load(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantRaw := want[stored.Metadata.Envelope.MailFrom]
		if string(diskRaw) != wantRaw || string(stored.Raw) != wantRaw {
			t.Fatalf("persisted raw for %q: disk=%q load=%q want=%q", stored.Metadata.Envelope.MailFrom, diskRaw, stored.Raw, wantRaw)
		}
	}
}

func TestSMTPDotTransparencyUsesCanonicalSize(t *testing.T) {
	canonical := "Subject: size\r\n\r\n.abc\r\n.\r\n..\r\n"
	wire := "Subject: size\r\n\r\n..abc\r\n..\r\n...\r\n"

	t.Run("exact boundary", func(t *testing.T) {
		message := receiveSMTPMessage(t, wire, int64(len(canonical)))
		if message.Raw != canonical {
			t.Fatalf("raw=%q, want %q", message.Raw, canonical)
		}
	})

	t.Run("one canonical byte over", func(t *testing.T) {
		var sinkCalls int
		server, client, reader, done := startSMTPForSizeTest(t, int64(len(canonical)-1), func(Session, mail.Message) error {
			sinkCalls++
			return nil
		})
		defer server.Close()
		beginDataForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
		writeSMTP(t, client, wire+".\r\n")
		expectRawCRLF(t, reader, "552")
		writeSMTP(t, client, "QUIT\r\n")
		expectRawCRLF(t, reader, "221")
		client.Close()
		waitSMTP(t, done)
		if sinkCalls != 0 {
			t.Fatalf("oversized message reached sink %d times", sinkCalls)
		}
	})
}

func TestSMTPDotTransparencyAcrossBoundedFragments(t *testing.T) {
	content := strings.Repeat("x", 64*1024)
	wire := "Subject: long\r\n\r\n.." + content + "\r\n"
	canonical := "Subject: long\r\n\r\n." + content + "\r\n"
	message := receiveSMTPMessage(t, wire, int64(len(canonical)))
	if message.Raw != canonical || message.TextBody != "."+content+"\r\n" {
		t.Fatalf("long dot-prefixed line was reconstructed incorrectly: raw=%d text=%d", len(message.Raw), len(message.TextBody))
	}
}

func TestOversizedDrainDoesNotTreatStuffedDotAsTerminator(t *testing.T) {
	server, client, reader, done := startSMTPForSizeTest(t, 16, nil)
	defer server.Close()
	beginDataForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
	writeSMTP(t, client, strings.Repeat("x", 128)+"\r\n..\r\n...\r\n.\r\n")
	expectRawCRLF(t, reader, "552")
	writeSMTP(t, client, "QUIT\r\n")
	expectRawCRLF(t, reader, "221")
	client.Close()
	waitSMTP(t, done)
}

func receiveSMTPMessage(t *testing.T, wire string, limit int64) mail.Message {
	t.Helper()
	var received mail.Message
	server, client, reader, done := startSMTPForSizeTest(t, limit, func(_ Session, message mail.Message) error {
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
	return received
}
