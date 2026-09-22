package dmarc

import (
	"strings"
	"testing"
)

func TestIsDMARC(t *testing.T) {
	for txt, want := range map[string]bool{
		"v=DMARC1; p=none;": true, "V=dmarc1;p=none": true, "  v = DMARC1 ; p=reject": true, "v=DMARC1": true,
		"v=DMARC2; p=none": false, "p=none; v=DMARC1": false, "v=spf1 -all": false, "": false, "vDMARC1": false,
	} {
		if IsDMARC(txt) != want {
			t.Errorf("IsDMARC(%q) want %v", txt, want)
		}
	}
}

func TestParseValid(t *testing.T) {
	cases := map[string]func(Record) bool{
		"v=DMARC1; p=none;":      func(r Record) bool { return r.Policy == PolicyNone && r.Adkim == Relaxed && r.Aspf == Relaxed },
		"v=DMARC1; p=quarantine": func(r Record) bool { return r.Policy == PolicyQuarantine },
		"v=DMARC1; p=reject; sp=quarantine; np=none": func(r Record) bool {
			return r.Policy == PolicyReject && r.SubPolicy == PolicyQuarantine && r.NonExist == PolicyNone
		},
		"v=DMARC1; p=none; adkim=s":                  func(r Record) bool { return r.Adkim == Strict && r.Aspf == Relaxed },
		"v=DMARC1; p=none; adkim=r":                  func(r Record) bool { return r.Adkim == Relaxed },
		"v=DMARC1; p=none; aspf=s":                   func(r Record) bool { return r.Aspf == Strict && r.Adkim == Relaxed },
		"v=DMARC1; p=none; aspf=r":                   func(r Record) bool { return r.Aspf == Relaxed },
		"V=DMARC1 ;  P=REJECT ; ADKIM=S ":            func(r Record) bool { return r.Policy == PolicyReject && r.Adkim == Strict },
		"v=DMARC1; p=none; t=y; psd=n":               func(r Record) bool { return r.Testing && r.PSD == "n" },
		"v=DMARC1; p=none; x-custom=whatever; zzz=1": func(r Record) bool { return r.Policy == PolicyNone && len(r.Warnings) == 0 },
		"v=DMARC1; p=none; pct=50; ri=3600; rf=afrf": func(r Record) bool { return contains(r.Warnings, WarnDeprecatedTag) },
		"v=DMARC1; p=none;;; ":                       func(r Record) bool { return r.Policy == PolicyNone },
		"v=DMARC1; rua=mailto:agg@example.com": func(r Record) bool {
			return r.Policy == PolicyNone && contains(r.Warnings, WarnPolicyFromRUA) && len(r.RUAHosts) == 1
		},
		"v=DMARC1; p=none; rua=mailto:a@example.com!10m, https://x.example/r, mailto:b@other.org": func(r Record) bool {
			return len(r.RUAHosts) == 2 && contains(r.Warnings, WarnIgnoredReportURI)
		},
		"v=DMARC1; p=none; ruf=mailto:f@example.com; fo=1": func(r Record) bool { return r.RUFCount == 1 && contains(r.Warnings, WarnFailureReporting) },
	}
	for txt, ok := range cases {
		rec, err := Parse(txt)
		if err != nil || !ok(rec) {
			t.Errorf("Parse(%q) = %+v, %v", txt, rec, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	uris := "v=DMARC1; p=none; rua=" + strings.Repeat("mailto:a@example.com,", MaxReportURIs) + "mailto:z@example.com"
	tags := "v=DMARC1; p=none" + strings.Repeat("; x=1", MaxTags)
	cases := map[string]string{
		"p=none; v=DMARC1":                   ReasonBadVersion,
		"v=DMARC2; p=none":                   ReasonBadVersion,
		"; ;":                                ReasonBadVersion,
		"":                                   ReasonBadVersion,
		"v=DMARC1":                           ReasonMissingPolicy,
		"v=DMARC1; adkim=s":                  ReasonMissingPolicy,
		"v=DMARC1; rua=https://x.example/r":  ReasonMissingPolicy,
		"v=DMARC1; p=none; p=reject":         ReasonDuplicateTag,
		"v=DMARC1; p=none; adkim=s; adkim=r": ReasonDuplicateTag,
		"v=DMARC1; p=none; v=DMARC1":         ReasonDuplicateTag,
		"v=DMARC1; p=allow":                  ReasonBadPolicy,
		"v=DMARC1; p=":                       ReasonBadPolicy,
		"v=DMARC1; p=none; sp=maybe":         ReasonBadPolicy,
		"v=DMARC1; p=none; np=x":             ReasonBadPolicy,
		"v=DMARC1; p=none; adkim=x":          ReasonBadAlignment,
		"v=DMARC1; p=none; aspf=strict":      ReasonBadAlignment,
		"v=DMARC1; p=none; t=maybe":          ReasonBadValue,
		"v=DMARC1; p=none; psd=q":            ReasonBadValue,
		"v=DMARC1; p=none; garbage":          ReasonBadTag,
		"v=DMARC1; p=none; =x":               ReasonBadTag,
		"v=DMARC1; p=none\x00":               ReasonBadEncoding,
		"v=DMARC1; p=none; x=é":              ReasonBadEncoding,
		"v=DMARC1; p=none\r\n; adkim=s":      ReasonBadEncoding,
		uris:                                 ReasonTooManyURIs,
		tags:                                 ReasonTooManyTags,
		"v=DMARC1; p=none; x=" + strings.Repeat("a", MaxTagValue+1):  ReasonTagTooLong,
		"v=DMARC1; p=none; x=" + strings.Repeat("a", MaxRecordBytes): ReasonTooLong,
	}
	for txt, want := range cases {
		_, err := Parse(txt)
		ie, ok := err.(*InvalidError)
		if !ok || ie.Reason != want {
			t.Errorf("Parse(%.50q) = %v, want %s", txt, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "example") {
			t.Errorf("error leaks record text: %v", err)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"v=DMARC1; p=none;", "v=DMARC1; p=reject; rua=mailto:a@b.c!5m,mailto:", ";;;", "v=DMARC1; =; p", "v=DMARC1; rua=,,,", "v=DMARC1; p=none; rua=mailto:<@>"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		rec, err := Parse(s)
		if err != nil {
			return
		}
		if rec.Policy == "" || rec.Adkim == "" || rec.Aspf == "" {
			t.Fatalf("parsed record missing defaults: %+v", rec)
		}
		_ = IsDMARC(s)
	})
}
