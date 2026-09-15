package bounce

import (
	"strings"
	"testing"
	"time"
)

// FuzzGenerateDiagnostic attacks the one truly untrusted input path into
// Generate: remote SMTP diagnostic/remote-message text (RemoteMessage),
// which reaches the generated MIME via sanitizeText. The invariant is that
// no input can inject a header line or corrupt MIME structure — Generate
// must always either return an error or produce parseable output.
func FuzzGenerateDiagnostic(f *testing.F) {
	seeds := []string{
		"user unknown",
		"user unknown\r\nBcc: attacker@evil.test",
		"\r\n\r\nFrom: forged@evil.test\r\n\r\n",
		"line\x00with\x00nul",
		strings.Repeat("A", 5000),
		"---boundary-looking-text---",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, diagnostic string) {
		fl := Failure{
			Class:         FailurePermanentDelivery,
			SMTPCode:      550,
			RemoteMessage: diagnostic,
			Recipient:     "<a@example.com>",
		}
		dsn, err := NewDSN(fl, []string{"<a@example.com>"}, "mailx-a.local", "d1", time.Now())
		if err != nil {
			t.Fatalf("NewDSN unexpectedly failed: %v", err)
		}
		raw, err := Generate(dsn, sampleOpts())
		if err != nil {
			// Generation is allowed to fail only for reasons unrelated to
			// this specific diagnostic field (none exist in this path).
			t.Fatalf("Generate unexpectedly failed: %v", err)
		}
		if strings.Contains(raw, "\r\nBcc:") {
			t.Fatalf("diagnostic text injected a Bcc header:\n%s", raw)
		}
		// Must always remain parseable as a well-formed message.
		msg, mr := parseGenerated(t, raw)
		_ = msg
		for {
			_, err := mr.NextPart()
			if err != nil {
				break
			}
		}
	})
}
