package feedback

import (
	"strings"
	"testing"
)

func dsnMessage(t *testing.T, humanReadable, deliveryStatus string) []byte {
	if t != nil {
		t.Helper()
	}
	boundary := "BOUNDARY-1"
	var b strings.Builder
	b.WriteString("From: MAILER-DAEMON@relay.example\r\n")
	b.WriteString("To: sender@example.com\r\n")
	b.WriteString("Subject: Delivery Status Notification\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/report; report-type=delivery-status;\r\n boundary=\"" + boundary + "\"\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=us-ascii\r\n\r\n")
	b.WriteString(humanReadable + "\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: message/delivery-status\r\n\r\n")
	b.WriteString(deliveryStatus)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return []byte(b.String())
}

func permanentDeliveryStatus() string {
	return "Reporting-MTA: dns; relay.example\r\n" +
		"Arrival-Date: Mon, 1 Sep 2026 12:00:00 +0000\r\n\r\n" +
		"Original-Recipient: rfc822;Bob@Dest.Example\r\n" +
		"Final-Recipient: rfc822;bob@dest.example\r\n" +
		"Action: failed\r\n" +
		"Status: 5.1.1\r\n" +
		"Remote-MTA: dns;mx.dest.example\r\n" +
		"Diagnostic-Code: smtp; 550 5.1.1 mailbox doesn't exist\r\n"
}

func TestParseDSNValidPermanentFailure(t *testing.T) {
	raw := dsnMessage(t, "This is an automatically generated Delivery Status Notification.", permanentDeliveryStatus())
	groups, err := ParseDSN(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups", len(groups))
	}
	g := groups[0]
	if g.Action != ActionFailed || g.Status != "5.1.1" || g.FinalRecipient != "bob@dest.example" ||
		g.OriginalRecipient != "Bob@Dest.Example" || g.RemoteMTA != "mx.dest.example" || g.ReportingMTA != "relay.example" {
		t.Fatalf("%+v", g)
	}
	if !strings.Contains(g.DiagnosticCode, "550") {
		t.Fatalf("diagnostic: %q", g.DiagnosticCode)
	}
}

func TestParseDSNDelayed(t *testing.T) {
	body := "Reporting-MTA: dns; relay.example\r\n\r\n" +
		"Final-Recipient: rfc822;bob@dest.example\r\n" +
		"Action: delayed\r\n" +
		"Status: 4.4.7\r\n"
	groups, err := ParseDSN(dsnMessage(t, "delayed", body))
	if err != nil || len(groups) != 1 || groups[0].Action != ActionDelayed || groups[0].Status != "4.4.7" {
		t.Fatalf("%+v %v", groups, err)
	}
}

func TestParseDSNMultipleRecipients(t *testing.T) {
	body := "Reporting-MTA: dns; relay.example\r\n\r\n" +
		"Final-Recipient: rfc822;a@dest.example\r\n" +
		"Action: failed\r\n" +
		"Status: 5.1.1\r\n\r\n" +
		"Final-Recipient: rfc822;b@dest.example\r\n" +
		"Action: delayed\r\n" +
		"Status: 4.2.2\r\n"
	groups, err := ParseDSN(dsnMessage(t, "multi", body))
	if err != nil || len(groups) != 2 {
		t.Fatalf("%+v %v", groups, err)
	}
	if groups[0].FinalRecipient != "a@dest.example" || groups[1].FinalRecipient != "b@dest.example" {
		t.Fatalf("%+v", groups)
	}
}

func TestParseDSNMissingFieldsAreUnknownNotPanic(t *testing.T) {
	body := "Reporting-MTA: dns; relay.example\r\n\r\n" +
		"Final-Recipient: rfc822;a@dest.example\r\n" // no Action, no Status
	groups, err := ParseDSN(dsnMessage(t, "x", body))
	if err != nil || len(groups) != 1 {
		t.Fatalf("%+v %v", groups, err)
	}
	if k, _ := Classify(groups[0]); k != KindBounceUnknown {
		t.Fatalf("kind = %v, want unknown", k)
	}
}

func TestParseDSNUnknownFieldsTolerated(t *testing.T) {
	body := "Reporting-MTA: dns; relay.example\r\nX-Vendor-Extension: whatever\r\n\r\n" +
		"Final-Recipient: rfc822;a@dest.example\r\n" +
		"Action: failed\r\n" +
		"Status: 5.1.1\r\n" +
		"X-Vendor-Field: something\r\n"
	groups, err := ParseDSN(dsnMessage(t, "x", body))
	if err != nil || len(groups) != 1 || groups[0].Action != ActionFailed {
		t.Fatalf("%+v %v", groups, err)
	}
}

func TestParseDSNRejectsNotMultipartReport(t *testing.T) {
	raw := []byte("From: a@b.com\r\nTo: c@d.com\r\nContent-Type: text/plain\r\n\r\nhello\r\n")
	if _, err := ParseDSN(raw); err != ErrNotDSN {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDSNRejectsMissingDeliveryStatusPart(t *testing.T) {
	boundary := "B"
	raw := []byte("Content-Type: multipart/report; boundary=\"" + boundary + "\"\r\n\r\n" +
		"--" + boundary + "\r\nContent-Type: text/plain\r\n\r\nonly text\r\n--" + boundary + "--\r\n")
	if _, err := ParseDSN(raw); err != ErrNotDSN {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDSNRejectsEmptyAndOversized(t *testing.T) {
	if _, err := ParseDSN(nil); err != ErrNotDSN {
		t.Fatalf("empty: %v", err)
	}
	big := make([]byte, MaxInputBytes+1)
	if _, err := ParseDSN(big); err != ErrOversized {
		t.Fatalf("oversized: %v", err)
	}
}

// Malformed MIME (bad boundary, truncated, binary garbage, deeply "nested"
// looking boundaries) must never panic — only ever return an error.
func TestParseDSNMalformedInputsNeverPanic(t *testing.T) {
	cases := [][]byte{
		[]byte("Content-Type: multipart/report; boundary=\r\n\r\ngarbage"),
		[]byte("Content-Type: multipart/report; boundary=\"B\"\r\n\r\n--B\r\n\x00\x01\x02binary\xff\xfe--B--"),
		append([]byte("Content-Type: multipart/report; boundary=\"B\"\r\n\r\n--B\r\nContent-Type: message/delivery-status\r\n\r\n"),
			bytesRepeat('A', 10000)...),
		[]byte(""),
		[]byte("\r\n\r\n\r\n"),
	}
	for i, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			_, _ = ParseDSN(c)
		}()
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// Excessive per-recipient groups: bounded by maxMIMEParts, never unbounded allocation.
func TestParseDSNExcessiveRecipientGroupsIsBounded(t *testing.T) {
	var body strings.Builder
	body.WriteString("Reporting-MTA: dns; relay.example\r\n\r\n")
	for i := 0; i < 500; i++ {
		body.WriteString("Final-Recipient: rfc822;x@dest.example\r\nAction: failed\r\nStatus: 5.1.1\r\n\r\n")
	}
	raw := dsnMessage(t, "x", body.String())
	if len(raw) >= MaxInputBytes {
		t.Fatalf("test fixture (%d bytes) must stay under MaxInputBytes to exercise the group-count bound, not the size bound", len(raw))
	}
	groups, err := ParseDSN(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) > maxMIMEParts {
		t.Fatalf("%d groups, want <= %d", len(groups), maxMIMEParts)
	}
}

func TestSanitizeStripsControlCharsAndBounds(t *testing.T) {
	got := sanitize("a\r\nb\x00c"+strings.Repeat("z", 1000), 10)
	if len(got) > 10 || strings.ContainsAny(got, "\r\n\x00") {
		t.Fatalf("%q", got)
	}
}
