package dmarc

import (
	"context"
	"testing"
)

// zoneOrg builds an OrgFunc backed by a real tree walk over a fake DNS zone.
func zoneOrg(t *testing.T, zone txtMap) OrgFunc {
	t.Helper()
	svc := &Service{resolver: &fakeDNS{fn: zone.fn}}
	return func(d string) (string, bool) {
		w, err := svc.treeWalk(context.Background(), d)
		if err != nil {
			return "", false
		}
		return w.org, true
	}
}

func rec(v string) []string { return []string{v} }

// Strict alignment is a plain comparison and must never consult DNS.
func TestStrictAlignmentNeverUsesDNS(t *testing.T) {
	for _, tc := range []struct {
		name, from, id string
		want           bool
	}{
		{"identical", "example.com", "example.com", true},
		{"identical case and dot", "Example.COM", "example.com.", true},
		{"subdomain identity", "example.com", "mail.example.com", false},
		{"subdomain From", "mail.example.com", "example.com", false},
		{"lookalike", "example.com", "attackerexample.com", false},
		{"attacker suffix", "example.com", "example.com.attacker.com", false},
		{"different tld", "example.com", "example.org", false},
		{"empty", "example.com", "", false},
		{"ip literal", "example.com", "127.0.0.1", false},
		{"idn unsupported", "example.com", "exämple.com", false},
		{"punycode unsupported", "example.com", "xn--e1afmkfd.com", false},
	} {
		aligned, known := Aligned(tc.from, tc.id, Strict, func(string) (string, bool) {
			t.Fatalf("%s: strict alignment consulted the Organizational Domain", tc.name)
			return "", false
		})
		if aligned != tc.want || !known {
			t.Errorf("%s: Aligned(%q,%q,strict) = %v,%v", tc.name, tc.from, tc.id, aligned, known)
		}
	}
	// Identical domains are aligned in relaxed mode without DNS as well.
	if a, k := Aligned("example.com", "EXAMPLE.com", Relaxed, nil); !a || !k {
		t.Fatal("identical domains must align in relaxed mode with no DNS")
	}
}

// Relaxed alignment follows the RFC 9989 Tree Walk: A-G matrix from the milestone.
func TestRelaxedAlignmentUsesTreeWalkOrganizationalDomain(t *testing.T) {
	zone := txtMap{
		"_dmarc.example.com":   rec("v=DMARC1; p=none"),
		"_dmarc.example.co.uk": rec("v=DMARC1; p=none"),
		"_dmarc.attacker.com":  rec("v=DMARC1; p=none"),
	}
	org := zoneOrg(t, zone)
	for _, tc := range []struct {
		name, from, id string
		aligned, known bool
	}{
		{"B subdomain identity, org record at example.com", "mail.example.com", "example.com", true, true},
		{"C subdomain identity", "example.com", "mail.example.com", true, true},
		{"siblings under one org", "a.example.com", "b.example.com", true, true},
		{"deep name (F)", "deep.mail.example.com", "example.com", true, true},
		{"co.uk registrable domain", "example.co.uk", "mail.example.co.uk", true, true},
		{"D lookalike", "example.com", "attackerexample.com", false, true},
		{"D lookalike reversed", "attackerexample.com", "example.com", false, true},
		{"E attacker suffix", "example.com", "example.com.attacker.com", false, true},
		{"E attacker suffix reversed", "example.com.attacker.com", "example.com", false, true},
		{"different orgs", "example.com", "example.org", false, true},
		{"co.uk suffix is not an org of example.co.uk", "example.co.uk", "other.co.uk", false, true},
	} {
		aligned, known := Aligned(tc.from, tc.id, Relaxed, org)
		if aligned != tc.aligned || known != tc.known {
			t.Errorf("%s: Aligned(%q,%q,relaxed) = %v,%v want %v,%v", tc.name, tc.from, tc.id, aligned, known, tc.aligned, tc.known)
		}
	}
}

// Without any published record there is no Organizational Domain information:
// RFC 9989 4.10.2 says the starting domain is its own Organizational Domain, so
// distinct names do not align. (The old PSL logic would have said they do.)
func TestRelaxedAlignmentWithoutRecordsDoesNotGuessAnOrganization(t *testing.T) {
	org := zoneOrg(t, txtMap{})
	if a, k := Aligned("mail.example.com", "example.com", Relaxed, org); a || !k {
		t.Fatalf("no records anywhere: aligned=%v known=%v", a, k)
	}
}

func TestRelaxedAlignmentIsUnknownWhenDNSFails(t *testing.T) {
	fail := func(string) (string, bool) { return "", false }
	if a, k := Aligned("mail.example.com", "example.com", Relaxed, fail); a || k {
		t.Fatalf("aligned=%v known=%v: a DNS failure is unknown, never 'not aligned'", a, k)
	}
	if a, k := Aligned("mail.example.com", "example.com", Relaxed, nil); a || k {
		t.Fatal("relaxed alignment without an Organizational Domain source must be unknown")
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{"Example.COM.": "example.com", " a.b ": "a.b", "com": "com"} {
		if got, ok := canonical(in); !ok || got != want {
			t.Errorf("canonical(%q) = %q,%v", in, got, ok)
		}
	}
	for _, in := range []string{"", ".", "a..b", "-a.com", "a-.com", "a_b.com", "127.0.0.1", "::1", "exämple.com", "xn--a.com", "a b.com", "*.a.com"} {
		if got, ok := canonical(in); ok {
			t.Errorf("canonical(%q) = %q, want rejection", in, got)
		}
	}
}
