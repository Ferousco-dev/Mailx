package smtpidentity

import (
	"strings"
	"testing"
)

func TestValidateHostnameAccepts(t *testing.T) {
	for in, want := range map[string]string{
		"smtp.example.com":          "smtp.example.com",
		"mx1.example.com":           "mx1.example.com",
		"outbound.mail.example.com": "outbound.mail.example.com",
		"SMTP.EXAMPLE.COM.":         "smtp.example.com", // case and one root dot normalized
		"Mail-1.Example.Co.UK":      "mail-1.example.co.uk",
		"a1.b2.appmd.dev":           "a1.b2.appmd.dev",
		"3com.example.com":          "3com.example.com", // leading digit is legal (RFC 1912)
	} {
		got, err := ValidateHostname(in)
		if err != nil || got != want {
			t.Errorf("ValidateHostname(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestValidateHostnameRejects(t *testing.T) {
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example.com"
	cases := map[string]string{
		"":                                       ReasonEmpty,
		"mailx.local":                            ReasonLocalName,
		"localhost":                              ReasonLocalName,
		"foo.localhost":                          ReasonLocalName,
		"smtp.local":                             ReasonLocalName,
		".local":                                 ReasonLocalName,
		"host.internal":                          ReasonLocalName,
		"mail.lan":                               ReasonLocalName,
		"smtp.example":                           ReasonLocalName, // RFC 6761 special-use TLD
		"smtp.test":                              ReasonLocalName,
		"1.0.0.127.in-addr.arpa":                 ReasonLocalName,
		"smtp":                                   ReasonSingleLabel,
		"*.example.com":                          ReasonWildcard,
		"smtp*.example.com":                      ReasonWildcard,
		"https://smtp.example.com":               ReasonURL,
		"smtp.example.com/path":                  ReasonURL,
		"smtp.example.com?x=1":                   ReasonURL,
		"smtp.example.com:25":                    ReasonHostPort,
		"user@example.com":                       ReasonEmailAddress,
		"203.0.113.10":                           ReasonIPLiteral,
		"[203.0.113.10]":                         ReasonIPLiteral,
		"::1":                                    ReasonIPLiteral,
		"2001:db8::1":                            ReasonIPLiteral,
		"[IPv6:2001:db8::1]":                     ReasonIPLiteral,
		" smtp.example.com":                      ReasonControlOrSpace,
		"smtp.example.com ":                      ReasonControlOrSpace,
		"smtp .example.com":                      ReasonControlOrSpace,
		"smtp.example.com\n":                     ReasonControlOrSpace,
		"smtp.example.com\x00":                   ReasonControlOrSpace,
		"smtp\t.example.com":                     ReasonControlOrSpace,
		"smtp.exämple.com":                       ReasonNotASCII,
		"xn--e1afmkfd.example.com":               ReasonIDN, // no IDN, same as domain ownership
		"-smtp.example.com":                      ReasonInvalidLabel,
		"smtp-.example.com":                      ReasonInvalidLabel,
		"smtp..example.com":                      ReasonInvalidLabel,
		"smtp.example.com..":                     ReasonInvalidLabel,
		"sm_tp.example.com":                      ReasonInvalidLabel,
		"com":                                    ReasonSingleLabel,
		"co.uk":                                  ReasonPublicSuffix,
		"blogspot.com":                           ReasonPublicSuffix,
		"github.io":                              ReasonPublicSuffix,
		"smtp.unknowntld":                        ReasonNotPublicFQDN,
		strings.Repeat("a", 64) + ".example.com": ReasonInvalidLabel,
		long:                                     ReasonTooLong,
		strings.Repeat("a.", 200) + "com":        ReasonTooLong,
	}
	for in, want := range cases {
		got, err := ValidateHostname(in)
		he, ok := err.(*HostnameError)
		if !ok || got != "" || he.Reason != want {
			t.Errorf("ValidateHostname(%.40q) = %q, %v; want reason %s", in, got, err, want)
		}
		if err != nil && in != "" && strings.Contains(err.Error(), strings.TrimSpace(in)) && len(in) > 3 {
			t.Errorf("error leaks the value: %v", err)
		}
	}
}

func FuzzValidateHostname(f *testing.F) {
	for _, s := range []string{"smtp.example.com", "", ".", "..", "a", "[::1]", "a.b.c.d.e.f.g.h", "SMTP.EXAMPLE.COM.", "\x00", "é.com", strings.Repeat("a.", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ValidateHostname(s)
		if err != nil {
			if got != "" {
				t.Fatalf("error with a value: %q", got)
			}
			return
		}
		// Anything accepted is canonical, idempotent and safe to put in EHLO/Message-ID.
		again, err := ValidateHostname(got)
		if err != nil || again != got || got != strings.ToLower(got) || strings.ContainsAny(got, " \r\n\x00:@*/") {
			t.Fatalf("accepted %q -> %q -> %q, %v", s, got, again, err)
		}
	})
}
