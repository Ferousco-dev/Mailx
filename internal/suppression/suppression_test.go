package suppression

import (
	"strings"
	"testing"
)

func TestNormalizeAccepts(t *testing.T) {
	for in, want := range map[string]string{
		"person@example.com":          "person@example.com",
		"  person@example.com  ":      "person@example.com",
		"<person@example.com>":        "person@example.com", // envelope form seen by the worker
		"Person@EXAMPLE.COM":          "person@example.com", // domain is DNS; local part folded for the key only (RFC 5321 2.4 discourages case-sensitivity)
		"PERSON@Example.Com.":         "person@example.com", // one trailing root dot
		"john.smith@gmail.com":        "john.smith@gmail.com",
		"johnsmith@gmail.com":         "johnsmith@gmail.com",  // NOT folded with the dotted form
		"user+tag@example.com":        "user+tag@example.com", // NOT folded with user@
		"user@example.com":            "user@example.com",
		"a.b-c_d'e@sub.example.co.uk": "a.b-c_d'e@sub.example.co.uk",
		"x@a.b":                       "x@a.b",
		"user@xn--80ak6aa92e.com":     "user@xn--80ak6aa92e.com", // already-ASCII A-label domains are fine
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// The dot and plus variants really are distinct keys.
	a, _ := Normalize("john.smith@gmail.com")
	b, _ := Normalize("johnsmith@gmail.com")
	c, _ := Normalize("john.smith+news@gmail.com")
	if a == b || a == c || b == c {
		t.Fatal("provider-specific mailbox equivalence must never be assumed")
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, in := range []string{
		"", " ", "<>", "person", "@example.com", "person@", "person@@example.com", "a@b@example.com",
		"Person <person@example.com>", "\"person\"@example.com", "\"a b\"@example.com", "person@example.com>", "<person@example.com",
		"person @example.com", "person@exa mple.com", "person@example.com\r\nBcc: x@y.z", "person@example.com\x00",
		"pérson@example.com", "person@exämple.com", "person@[127.0.0.1]", "person@127.0.0.1.", "person@example.com..", "person@.", "person@-example.com",
		"person@example-.com", "person@example..com", ".person@example.com", "person.@example.com", "pe..son@example.com",
		"person@example.com,other@example.com", "person@example.com;", "(c)person@example.com",
		strings.Repeat("a", 65) + "@example.com", "a@" + strings.Repeat("a", 64) + ".com", strings.Repeat("a", 250) + "@b.co",
		"person@" + strings.Repeat("a.", 130) + "com",
	} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%.40q) = %q, want an error", in, got)
		}
	}
}

func TestNormalizeIsIdempotentAndCanonical(t *testing.T) {
	for _, in := range []string{"Foo.Bar+Tag@Example.COM.", "<X@y.z>", "a@b.c"} {
		once, err := Normalize(in)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := Normalize(once)
		if err != nil || twice != once || once != strings.ToLower(once) {
			t.Fatalf("%q -> %q -> %q, %v", in, once, twice, err)
		}
	}
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"a@b.c", "<A@B.C>", "", "@", "a@b@c", "\x00@x.y", "a@" + strings.Repeat("b.", 200), "\"q\"@x.y", "a@[1.2.3.4]", "é@x.y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Normalize(s)
		if err != nil {
			if got != "" {
				t.Fatalf("error with a value: %q", got)
			}
			return
		}
		again, err := Normalize(got)
		if err != nil || again != got || got != strings.ToLower(got) || strings.Count(got, "@") != 1 || len(got) > maxAddressBytes ||
			strings.ContainsAny(got, " \t\r\n\x00<>,;\"") {
			t.Fatalf("%q -> %q -> %q, %v", s, got, again, err)
		}
	})
}

func TestParseAPIReason(t *testing.T) {
	for in, want := range map[string]Reason{"": ReasonManual, "manual": ReasonManual} {
		if got, err := ParseAPIReason(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	// Callers cannot forge system-derived or unimplemented reasons.
	for _, bad := range []string{"hard_bounce", "complaint", "unsubscribe", "MANUAL", "spam", " manual"} {
		if _, err := ParseAPIReason(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, r := range []Reason{ReasonManual, ReasonHardBounce, ReasonComplaint, ReasonUnsubscribe} {
		if !r.Valid() {
			t.Errorf("%s", r)
		}
	}
	if Reason("x").Valid() || Source("x").Valid() {
		t.Fatal("unknown values must be invalid")
	}
}

func TestQualifiesHardBounce(t *testing.T) {
	type c struct {
		name      string
		stage     string
		permanent bool
		accepted  bool
		code      int
		enhanced  string
		rcpt      string
		want      bool
	}
	for _, tc := range []c{
		{"550 5.1.1 at RCPT", "rcpt_to", true, false, 550, "5.1.1", "<a@b.c>", true},
		{"553 5.1.6 moved", "rcpt_to", true, false, 553, "5.1.6", "<a@b.c>", true},
		{"relay says the same", "rcpt_to", true, false, 550, "5.1.1", "<a@b.c>", true},
		// Not about the recipient mailbox:
		{"temporary 4.1.1", "rcpt_to", false, false, 451, "4.1.1", "<a@b.c>", false},
		{"450 mailbox busy", "rcpt_to", false, false, 450, "4.2.1", "<a@b.c>", false},
		{"sender rejected at MAIL FROM", "mail_from", true, false, 550, "5.1.1", "", false},
		{"policy 5.7.1 at RCPT", "rcpt_to", true, false, 550, "5.7.1", "<a@b.c>", false},
		{"auth/DKIM/SPF policy 5.7.26", "rcpt_to", true, false, 550, "5.7.26", "<a@b.c>", false},
		{"bad domain 5.1.2", "rcpt_to", true, false, 550, "5.1.2", "<a@b.c>", false},
		{"null MX 5.1.10", "rcpt_to", true, false, 556, "5.1.10", "<a@b.c>", false},
		{"bad syntax 5.1.3", "rcpt_to", true, false, 553, "5.1.3", "<a@b.c>", false},
		{"mailbox full 5.2.2", "rcpt_to", true, false, 552, "5.2.2", "<a@b.c>", false},
		{"mailbox disabled 5.2.1", "rcpt_to", true, false, 550, "5.2.1", "<a@b.c>", false},
		{"bare 550 without enhanced code", "rcpt_to", true, false, 550, "", "<a@b.c>", false},
		{"content rejected at DATA", "data", true, false, 554, "5.6.0", "", false},
		{"data response 5.1.1 is not a RCPT verdict", "data_response", true, false, 550, "5.1.1", "<a@b.c>", false},
		{"relay auth failure", "auth", true, false, 535, "5.7.8", "", false},
		{"accepted delivery", "rcpt_to", true, true, 250, "5.1.1", "<a@b.c>", false},
		{"no recipient named", "rcpt_to", true, false, 550, "5.1.1", "", false},
		{"non-5xx code with 5.1.1", "rcpt_to", true, false, 451, "5.1.1", "<a@b.c>", false},
		{"malformed enhanced code", "rcpt_to", true, false, 550, "5.1.1.1", "<a@b.c>", false},
	} {
		if got := QualifiesHardBounce(tc.stage, tc.permanent, tc.accepted, tc.code, tc.enhanced, tc.rcpt); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
