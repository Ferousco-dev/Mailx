package bimi

import (
	"errors"
	"strings"
	"testing"
)

func TestIsBIMI(t *testing.T) {
	for txt, want := range map[string]bool{
		"v=BIMI1; l=https://example.com/logo.svg": true,
		"V=bimi1; l=":          true,
		"  v = BIMI1 ; l=":     true,
		"v=BIMI2; l=":          false,
		"l=https://x; v=BIMI1": false,
		"":                     false,
		"vBIMI1":               false,
	} {
		if IsBIMI(txt) != want {
			t.Errorf("IsBIMI(%q) want %v", txt, want)
		}
	}
}

func TestParseValidRecord(t *testing.T) {
	rec, err := Parse("v=BIMI1; l=https://example.com/logo.svg; a=https://example.com/vmc.pem")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Location != "https://example.com/logo.svg" || rec.Authority != "https://example.com/vmc.pem" || rec.Declined {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestParseDeclinedRecord(t *testing.T) {
	rec, err := Parse("v=BIMI1; l=")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Declined || rec.Location != "" {
		t.Fatalf("expected declined with no location: %+v", rec)
	}
}

func TestParseNoAuthorityIsOptional(t *testing.T) {
	rec, err := Parse("v=BIMI1; l=https://example.com/logo.svg")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Authority != "" {
		t.Fatalf("expected no authority, got %q", rec.Authority)
	}
}

func TestParseRejectsMissingL(t *testing.T) {
	_, err := Parse("v=BIMI1")
	assertReason(t, err, ReasonMissingL)
}

func TestParseRejectsNonHTTPSLocation(t *testing.T) {
	_, err := Parse("v=BIMI1; l=http://example.com/logo.svg")
	assertReason(t, err, ReasonNotHTTPS)
}

func TestParseRejectsNonHTTPSAuthority(t *testing.T) {
	_, err := Parse("v=BIMI1; l=https://example.com/logo.svg; a=ftp://example.com/vmc.pem")
	assertReason(t, err, ReasonNotHTTPS)
}

func TestParseRejectsDuplicateTags(t *testing.T) {
	_, err := Parse("v=BIMI1; l=https://a/x.svg; l=https://b/y.svg")
	assertReason(t, err, ReasonDuplicate)
}

func TestParseRejectsBadVersion(t *testing.T) {
	_, err := Parse("v=BIMI2; l=https://example.com/logo.svg")
	assertReason(t, err, ReasonBadVersion)
}

func TestParseRejectsOversizedRecord(t *testing.T) {
	huge := "v=BIMI1; l=https://example.com/" + strings.Repeat("a", MaxRecordBytes)
	_, err := Parse(huge)
	assertReason(t, err, ReasonTooLong)
}

func TestParseIgnoresUnknownTags(t *testing.T) {
	rec, err := Parse("v=BIMI1; l=https://example.com/logo.svg; x-custom=whatever")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Location == "" {
		t.Fatal("expected location to still parse with an unknown tag present")
	}
}

func TestQueryName(t *testing.T) {
	if got := QueryName(DefaultSelector, "example.com"); got != "default._bimi.example.com" {
		t.Fatalf("unexpected query name: %q", got)
	}
}

func assertReason(t *testing.T, err error, want string) {
	t.Helper()
	var ie *InvalidError
	if !errors.As(err, &ie) {
		t.Fatalf("expected *InvalidError, got %v", err)
	}
	if ie.Reason != want {
		t.Fatalf("reason = %q, want %q", ie.Reason, want)
	}
}
