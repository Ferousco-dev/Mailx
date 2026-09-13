package mail

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestParseMessage(t *testing.T) {
	raw := "fRoM: Alice <alice@example.com>\r\n" +
		"TO: Bob <bob@example.com>, Carol <carol@example.com>\r\n" +
		"Cc: Mary <mary@example.com>\r\n" +
		"Bcc: Hidden <hidden@example.com>\r\n" +
		"Subject: MailX\r\n" +
		" parser test\r\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\r\n" +
		"Message-ID: <test123@mailx.local>\r\n\r\n" +
		"Hello from MailX.\r\nThis is the body.\r\n"

	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.From != "Alice <alice@example.com>" || len(message.To) != 2 || message.To[1] != "Carol <carol@example.com>" || len(message.Cc) != 1 || len(message.Bcc) != 1 {
		t.Fatalf("unexpected addresses: %#v", message)
	}
	if message.Subject != "MailX parser test" || message.Date == "" || message.MessageID != "<test123@mailx.local>" {
		t.Fatalf("unexpected headers: %#v", message)
	}
	if message.Body != "Hello from MailX.\r\nThis is the body.\r\n" || message.Raw != raw {
		t.Fatalf("body or raw content was not preserved: %#v", message)
	}
}

func TestParseMessageOptionalHeadersAndBody(t *testing.T) {
	message, err := ParseMessage("From: alice@example.com\nTo: bob@example.com\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if message.Subject != "" || message.Body != "" || len(message.To) != 1 {
		t.Fatalf("unexpected optional fields: %#v", message)
	}
}

func TestParseMessageMIMEMetadata(t *testing.T) {
	message, err := ParseMessage("mImE-vErSiOn: 1.0\r\nCONTENT-TYPE: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\ncontent-disposition: inline\r\n\r\naGVsbG8=\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if message.MIMEVersion != "1.0" || message.ContentType != "text/plain; charset=UTF-8" || message.ContentTransferEncoding != "base64" || message.ContentDisposition != "inline" {
		t.Fatalf("unexpected MIME metadata: %#v", message)
	}

	html, err := ParseMessage("Content-Type: text/html; charset=UTF-8\nContent-Transfer-Encoding: quoted-printable\n\n<h1>Hello</h1>")
	if err != nil {
		t.Fatal(err)
	}
	if html.ContentType != "text/html; charset=UTF-8" || html.ContentTransferEncoding != "quoted-printable" {
		t.Fatalf("unexpected HTML metadata: %#v", html)
	}
}

func TestParseMessageAllowsMissingMIMEHeaders(t *testing.T) {
	message, err := ParseMessage("Subject: plain message\r\n\r\nHello")
	if err != nil {
		t.Fatal(err)
	}
	if message.MIMEVersion != "" || message.ContentType != "" || message.ContentTransferEncoding != "" || message.ContentDisposition != "" {
		t.Fatalf("unexpected MIME metadata: %#v", message)
	}
}

func TestParseMessageTextPlain(t *testing.T) {
	raw := "Content-Type: text/plain\r\n\r\nHello MailX.\r\nSecond line.\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.MediaType != "text/plain" || message.TextBody != "Hello MailX.\r\nSecond line.\r\n" || message.HTMLBody != "" || message.Body != message.TextBody || message.Raw != raw {
		t.Fatalf("unexpected text/plain message: %#v", message)
	}
}

func TestParseMessageTextHTML(t *testing.T) {
	raw := "Content-Type: TEXT/HTML; charset=UTF-8\r\n\r\n<html>\r\n<body>\r\n<h1>Hello MailX ✓</h1>\r\n</body>\r\n</html>\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.MediaType != "text/html" || message.Charset != "UTF-8" || message.TextBody != "" || message.HTMLBody != "<html>\r\n<body>\r\n<h1>Hello MailX ✓</h1>\r\n</body>\r\n</html>\r\n" || message.Body != message.HTMLBody || message.Raw != raw {
		t.Fatalf("unexpected text/html message: %#v", message)
	}
}

func TestParseMessageEmptyTextHTML(t *testing.T) {
	message, err := ParseMessage("Content-Type: text/html\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if message.MediaType != "text/html" || message.TextBody != "" || message.HTMLBody != "" {
		t.Fatalf("unexpected empty HTML message: %#v", message)
	}
}

func TestParseMessageMultipartAlternative(t *testing.T) {
	raw := "MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"mailx.test_123\"\r\n\r\n" +
		"--mailx.test_123\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nHello from MailX.\r\n" +
		"--mailx.test_123\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<h1>Hello from MailX.</h1>\r\n" +
		"--mailx.test_123--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.MediaType != "multipart/alternative" || message.TextBody != "Hello from MailX." || message.HTMLBody != "<h1>Hello from MailX.</h1>" || message.Body == message.TextBody || message.Raw != raw {
		t.Fatalf("unexpected multipart/alternative message: %#v", message)
	}
}

func TestParseMessageMultipartAlternativeReverseAndDuplicateOrder(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/html\r\n\r\nfirst html\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\n\r\nplain text\r\n" +
		"--mailx\r\nContent-Type: text/html\r\n\r\nlast html\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "plain text" || message.HTMLBody != "last html" {
		t.Fatalf("unexpected alternative ordering: %#v", message)
	}
}

func TestParseMessageMultipartAlternativeDecodesEncodedBodies(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello=20MailX.\r\n" +
		"--mailx\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: base64\r\n\r\nPGgxPkhlbGxvIE1haWxYLjwvaDE+\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Hello MailX." || message.HTMLBody != "<h1>Hello MailX.</h1>" || message.Raw != raw {
		t.Fatalf("encoded alternative leaves were not decoded: %#v", message)
	}
}

func TestParseMessageMultipartAlternativePartDefaultsAndUnknownType(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=mailx\r\n\r\n" +
		"--mailx\r\n\r\nplain by default\r\n" +
		"--mailx\r\nContent-Type: application/example\r\n\r\nignored\r\n" +
		"--mailx\r\nContent-Type: text/html\r\n\r\n\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "plain by default" || message.HTMLBody != "" {
		t.Fatalf("unexpected multipart defaults: %#v", message)
	}
}

func TestParseMessageRejectsMalformedMultipartAlternative(t *testing.T) {
	for _, raw := range []string{
		"Content-Type: multipart/alternative\r\n\r\nbody",
		"Content-Type: multipart/alternative; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\n\r\ntruncated",
	} {
		if _, err := ParseMessage(raw); err == nil {
			t.Fatalf("ParseMessage(%q) returned no error", raw)
		}
	}
}

func TestParseMessageMultipartMixedTextBodies(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nHello plain\r\n" +
		"--mailx\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Disposition: inline\r\n\r\n<h1>Hello HTML</h1>\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.MediaType != "multipart/mixed" || message.TextBody != "Hello plain" || message.HTMLBody != "<h1>Hello HTML</h1>" || message.Body == message.TextBody || message.Raw != raw {
		t.Fatalf("unexpected multipart/mixed message: %#v", message)
	}
}

func TestParseMessageMultipartMixedSkipsAttachmentsAndUnknownTypes(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\n\r\nMain body\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nMust not replace body\r\n" +
		"--mailx\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"notes.html\"\r\n\r\nMust not become HTML\r\n" +
		"--mailx\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\n\r\nPDF bytes\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Main body" || message.HTMLBody != "" {
		t.Fatalf("attachments were interpreted as bodies: %#v", message)
	}
}

func TestParseMessageMultipartMixedDefaultsAndEncodedParts(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\n\r\nPlain by default\r\n" +
		"--mailx\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: base64\r\n\r\nPGgxPkVuY29kZWQ8L2gxPg==\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nencoded=20text\r\n" +
		"--mailx\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner--\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "encoded text" || message.HTMLBody != "<h1>Encoded</h1>" {
		t.Fatalf("encoded mixed leaves were not decoded: %#v", message)
	}
}

func TestParseMessageRejectsMalformedMultipartMixed(t *testing.T) {
	for _, raw := range []string{
		"Content-Type: multipart/mixed\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\n\r\ntruncated",
		"Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: not a disposition; =\r\n\r\nbody\r\n--mailx--\r\n",
	} {
		if _, err := ParseMessage(raw); err == nil {
			t.Fatalf("ParseMessage(%q) returned no error", raw)
		}
	}
}

func TestParseMessageNestedMultipartMixedAlternative(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\n\r\nNested plain\r\n" +
		"--inner\r\nContent-Type: text/html\r\n\r\n<p>Nested HTML</p>\r\n" +
		"--inner--\r\n" +
		"--outer\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\n\r\nPDF bytes\r\n" +
		"--outer--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Nested plain" || message.HTMLBody != "<p>Nested HTML</p>" || message.Raw != raw || message.Body == message.TextBody {
		t.Fatalf("unexpected nested multipart message: %#v", message)
	}
}

func TestParseMessageNestedAlternativeMixed(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\n\r\nNested text\r\n" +
		"--inner\r\nContent-Type: text/html\r\n\r\n<b>Nested HTML</b>\r\n" +
		"--inner--\r\n" +
		"--outer--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Nested text" || message.HTMLBody != "<b>Nested HTML</b>" {
		t.Fatalf("unexpected nested alternative/mixed message: %#v", message)
	}
}

func TestParseMessageNestedMultipartSafety(t *testing.T) {
	attachmentContainer := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=hidden\r\nContent-Disposition: attachment\r\n\r\n" +
		"--hidden\r\nContent-Type: text/plain\r\n\r\nHidden attachment text\r\n--hidden--\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nVisible text\r\n--outer--\r\n"
	message, err := ParseMessage(attachmentContainer)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Visible text" {
		t.Fatalf("attachment container was interpreted: %#v", message)
	}

	unknownContainer := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/related; boundary=unknown\r\n\r\n--unknown--\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nVisible text\r\n--outer--\r\n"
	message, err = ParseMessage(unknownContainer)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Visible text" {
		t.Fatalf("unknown multipart subtype was not skipped: %#v", message)
	}
}

func TestParseMessageNestedMultipartEncodedLeavesAndErrors(t *testing.T) {
	encoded := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nVisible text\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nRW5jb2RlZA==\r\n" +
		"--inner\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nencoded=20HTML\r\n" +
		"--inner--\r\n--outer--\r\n"
	message, err := ParseMessage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Encoded" || message.HTMLBody != "encoded HTML" {
		t.Fatalf("encoded nested leaves were not decoded: %#v", message)
	}

	for _, raw := range []string{
		"Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative\r\n\r\nbody\r\n--outer--\r\n",
		"Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\ntruncated",
	} {
		if _, err := ParseMessage(raw); err == nil {
			t.Fatalf("ParseMessage(%q) returned no error", raw)
		}
	}
}

func TestParseMessageDecodesTopLevelTransferEncodings(t *testing.T) {
	for _, test := range []struct {
		name      string
		raw       string
		textBody  string
		htmlBody  string
		body      string
		mediaType string
	}{
		{
			name:      "base64 plain text",
			raw:       "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8gTWFpbFgu\r\n",
			textBody:  "Hello MailX.",
			body:      "SGVsbG8gTWFpbFgu\r\n",
			mediaType: "text/plain",
		},
		{
			name:      "quoted printable HTML",
			raw:       "Content-Type: text/html\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<h1>Hello=20MailX</h1>",
			htmlBody:  "<h1>Hello MailX</h1>",
			body:      "<h1>Hello=20MailX</h1>",
			mediaType: "text/html",
		},
		{
			name:      "wrapped base64 and case insensitive encoding",
			raw:       "Content-Type: text/plain\r\nContent-Transfer-Encoding: BASE64\r\n\r\nSGVsbG8g\r\nTWFpbFgu\r\n",
			textBody:  "Hello MailX.",
			body:      "SGVsbG8g\r\nTWFpbFgu\r\n",
			mediaType: "text/plain",
		},
		{
			name:      "quoted printable soft line break",
			raw:       "Content-Type: text/plain\r\nContent-Transfer-Encoding: QuOtEd-PrInTaBlE\r\n\r\nHello=\r\nMailX.",
			textBody:  "HelloMailX.",
			body:      "Hello=\r\nMailX.",
			mediaType: "text/plain",
		},
		{
			name:      "empty encoded body",
			raw:       "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n",
			textBody:  "",
			body:      "",
			mediaType: "text/plain",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := ParseMessage(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if message.TextBody != test.textBody || message.HTMLBody != test.htmlBody || message.Body != test.body || message.Raw != test.raw || message.MediaType != test.mediaType {
				t.Fatalf("unexpected decoded message: %#v", message)
			}
		})
	}
}

func TestParseMessagePreservesIdentityTransferEncodings(t *testing.T) {
	for _, transferEncoding := range []string{"", "7bit", "8bit", "binary"} {
		t.Run(transferEncoding, func(t *testing.T) {
			headers := "Content-Type: text/plain\r\n"
			if transferEncoding != "" {
				headers += "Content-Transfer-Encoding: " + transferEncoding + "\r\n"
			}
			raw := headers + "\r\nplain body"
			message, err := ParseMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if message.TextBody != "plain body" || message.Body != "plain body" {
				t.Fatalf("identity body changed: %#v", message)
			}
		})
	}
}

func TestParseMessageRejectsInvalidTransferEncodings(t *testing.T) {
	for _, raw := range []string{
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nnot base64!",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ninvalid=\rX",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: x-mailx\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=mailx\r\nContent-Transfer-Encoding: base64\r\n\r\n--mailx--\r\n",
	} {
		if _, err := ParseMessage(raw); err == nil {
			t.Fatalf("ParseMessage(%q) returned no error", raw)
		}
	}
}

func TestParseMessageDoesNotDecodeAttachments(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\n\r\nVisible text\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\nU2VjcmV0\r\n" +
		"--mailx\r\nContent-Type: text/html\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<b>Hidden=20HTML</b>\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Visible text" || message.HTMLBody != "" {
		t.Fatalf("attachment was decoded or interpreted: %#v", message)
	}
}

func TestParseMessageExtractsAttachments(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\n\r\nHello MailX.\r\n" +
		"--mailx\r\nContent-Type: application/pdf; name=\"fallback.pdf\"\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes with spaces.txt\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello=20attachment\r\n" +
		"--mailx\r\nContent-Type: application/octet-stream; name=\"fallback.bin\"\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: binary\r\n\r\nbinary\x00bytes\r\n" +
		"--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment\r\n\r\nunnamed bytes\r\n" +
		"--mailx--\r\n"

	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Hello MailX." || message.HTMLBody != "" || message.Body == message.TextBody || message.Raw != raw {
		t.Fatalf("message body or raw was changed: %#v", message)
	}
	want := []Attachment{
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4\n")},
		{Filename: "notes with spaces.txt", ContentType: "text/plain", Content: []byte("Hello attachment")},
		{Filename: "fallback.bin", ContentType: "application/octet-stream", Content: []byte("binary\x00bytes")},
		{Filename: "", ContentType: "application/octet-stream", Content: []byte("unnamed bytes")},
	}
	if len(message.Attachments) != len(want) {
		t.Fatalf("attachment count = %d, want %d: %#v", len(message.Attachments), len(want), message.Attachments)
	}
	for index, expected := range want {
		actual := message.Attachments[index]
		if actual.Filename != expected.Filename || actual.ContentType != expected.ContentType || !bytes.Equal(actual.Content, expected.Content) {
			t.Fatalf("attachment %d = %#v, want %#v", index, actual, expected)
		}
	}
}

func TestParseMessageAttachmentTextPartsDoNotBecomeBodies(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nHello attachment\r\n" +
		"--mailx\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"page.html\"\r\n\r\n<h1>Attachment</h1>\r\n" +
		"--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"../../secret.txt\"\r\n\r\nbytes\r\n" +
		"--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"/tmp/absolute.bin\"\r\n\r\nmore bytes\r\n" +
		"--mailx--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "" || message.HTMLBody != "" || len(message.Attachments) != 4 {
		t.Fatalf("text attachments were interpreted as visible bodies: %#v", message)
	}
	if message.Attachments[2].Filename != "../../secret.txt" || message.Attachments[3].Filename != "/tmp/absolute.bin" {
		t.Fatalf("filenames were unexpectedly changed: %#v", message.Attachments)
	}
}

func TestParseMessageExtractsNestedAttachments(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\n\r\nNested text\r\n" +
		"--inner\r\nContent-Type: text/html\r\n\r\n<p>Nested HTML</p>\r\n" +
		"--inner--\r\n" +
		"--outer\r\nContent-Type: multipart/mixed; boundary=files\r\n\r\n" +
		"--files\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"nested.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\nTmVzdGVkIGJ5dGVz\r\n" +
		"--files--\r\n" +
		"--outer--\r\n"
	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Nested text" || message.HTMLBody != "<p>Nested HTML</p>" || len(message.Attachments) != 1 || message.Attachments[0].Filename != "nested.bin" || !bytes.Equal(message.Attachments[0].Content, []byte("Nested bytes")) {
		t.Fatalf("nested attachment was not extracted: %#v", message)
	}
}

func TestParseMessageRealisticEncodedMIMEMessage(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=alternative\r\n\r\n" +
		"--alternative\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello=20MailX.=\r\n\nSecond=20line.\r\n" +
		"--alternative\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\nPGgxPkhlbGxvIE1haWxYPC9oMT4=\r\n" +
		"--alternative--\r\n" +
		"--outer\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n" +
		"--outer\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nMeeting=20notes\r\n" +
		"--outer--\r\n"

	message, err := ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.TextBody != "Hello MailX.\nSecond line." || message.HTMLBody != "<h1>Hello MailX</h1>" {
		t.Fatalf("encoded alternatives were not decoded: %#v", message)
	}
	if len(message.Attachments) != 2 || message.Attachments[0].Filename != "report.pdf" || !bytes.Equal(message.Attachments[0].Content, []byte("%PDF-1.4\n")) || message.Attachments[1].Filename != "notes.txt" || !bytes.Equal(message.Attachments[1].Content, []byte("Meeting notes")) {
		t.Fatalf("attachments were not extracted in MIME order: %#v", message.Attachments)
	}
	if message.Body == message.TextBody || message.Body == message.HTMLBody || message.Raw != raw {
		t.Fatalf("raw MIME data was not preserved: %#v", message)
	}
}

func TestParseMessageAttachmentErrorsAndInlineParts(t *testing.T) {
	for _, raw := range []string{
		"Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\nnot base64!\r\n--mailx--\r\n",
		"Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ninvalid=\rX\r\n--mailx--\r\n",
		"Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: x-mailx\r\n\r\nbytes\r\n--mailx--\r\n",
	} {
		if _, err := ParseMessage(raw); err == nil {
			t.Fatalf("ParseMessage(%q) returned no error", raw)
		}
	}

	inline, err := ParseMessage("Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\n\r\nVisible text\r\n--mailx\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=\"image.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\naW1hZ2U=\r\n--mailx--\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if inline.TextBody != "Visible text" || len(inline.Attachments) != 0 {
		t.Fatalf("inline part was treated as an attachment: %#v", inline)
	}
}

func TestParseMessageMIMENestingLimit(t *testing.T) {
	if _, err := ParseMessage(nestedMixedMessage(maxMIMEDepth)); err != nil {
		t.Fatalf("maximum valid nesting depth failed: %v", err)
	}
	if _, err := ParseMessage(nestedMixedMessage(maxMIMEDepth + 1)); err == nil {
		t.Fatal("nesting beyond the maximum depth returned no error")
	}
}

func nestedMixedMessage(levels int) string {
	var raw strings.Builder
	raw.WriteString("Content-Type: multipart/mixed; boundary=b0\r\n\r\n")
	for level := 0; level < levels-1; level++ {
		raw.WriteString(fmt.Sprintf("--b%d\r\nContent-Type: multipart/mixed; boundary=b%d\r\n\r\n", level, level+1))
	}
	raw.WriteString(fmt.Sprintf("--b%d\r\nContent-Type: text/plain\r\n\r\ndeep text\r\n", levels-1))
	for level := levels - 1; level >= 0; level-- {
		raw.WriteString(fmt.Sprintf("--b%d--\r\n", level))
	}
	return raw.String()
}

func TestParseMessageTextPlainCharset(t *testing.T) {
	message, err := ParseMessage("Content-Type: TEXT/PLAIN; charset=UTF-8\r\n\r\nHello, MailX ✓")
	if err != nil {
		t.Fatal(err)
	}
	if message.MediaType != "text/plain" || message.Charset != "UTF-8" || message.TextBody != "Hello, MailX ✓" {
		t.Fatalf("unexpected UTF-8 text/plain message: %#v", message)
	}
}

func TestParseMessageEmptyAndDefaultTextPlain(t *testing.T) {
	empty, err := ParseMessage("Content-Type: text/plain\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if empty.TextBody != "" {
		t.Fatalf("expected empty text body, got %#v", empty)
	}

	defaulted, err := ParseMessage("Subject: no content type\r\n\r\nHello")
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.MediaType != "text/plain" || defaulted.Charset != "us-ascii" || defaulted.TextBody != "Hello" {
		t.Fatalf("unexpected default text/plain interpretation: %#v", defaulted)
	}
}

func TestParseMessageMalformedContentTypeUsesDefault(t *testing.T) {
	message, err := ParseMessage("Content-Type: not a media type\r\n\r\nHello")
	if err != nil {
		t.Fatal(err)
	}
	if message.ContentType != "not a media type" || message.MediaType != "text/plain" || message.Charset != "us-ascii" || message.TextBody != "Hello" {
		t.Fatalf("unexpected malformed Content-Type handling: %#v", message)
	}
}

func TestParseMessageRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{
		"Subject: missing separator",
		"not a header\r\n\r\nbody",
		" continuation\r\n\r\nbody",
		"To: not-an-address\r\n\r\nbody",
	} {
		if _, err := ParseMessage(raw); err == nil {
			t.Fatalf("ParseMessage(%q) returned no error", raw)
		}
	}
}

func FuzzParseMessageDoesNotPanic(f *testing.F) {
	for _, raw := range []string{
		"\r\n",
		"Subject: test\r\n\r\nbody",
		"Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx--\r\n",
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8=",
	} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_, _ = ParseMessage(raw)
	})
}
