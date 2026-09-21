package dmarc

import "testing"

func TestOrganizationalDomain(t *testing.T) {
	for in, want := range map[string]string{
		"example.com": "example.com", "mail.example.com": "example.com", "A.B.Example.COM.": "example.com",
		"example.co.uk": "example.co.uk", "mail.example.co.uk": "example.co.uk", "appmd.dev": "appmd.dev", "x.y.appmd.dev": "appmd.dev",
	} {
		got, err := OrganizationalDomain(in)
		if err != nil || got != want {
			t.Errorf("OrganizationalDomain(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"com", "co.uk", "uk", "", "localhost", "127.0.0.1", "exämple.com", "xn--e1afmkfd.com", "*.example.com", "a..b.com", "sub.blogspot.com"} {
		if got, err := OrganizationalDomain(in); err == nil {
			t.Errorf("OrganizationalDomain(%q) = %q, want error (no registrable ICANN domain)", in, got)
		}
	}
}

// Alignment is a relation between two NAMES; it never says SPF or DKIM passed.
func TestAlignmentMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, from, id string
		mode           Mode
		want           bool
	}{
		{"A strict identical", "example.com", "example.com", Strict, true},
		{"A strict identical, case", "Example.COM", "example.com.", Strict, true},
		{"B strict subdomain identity", "example.com", "mail.example.com", Strict, false},
		{"B strict subdomain From", "mail.example.com", "example.com", Strict, false},
		{"C relaxed subdomain identity", "example.com", "mail.example.com", Relaxed, true},
		{"D relaxed subdomain From", "mail.example.com", "example.com", Relaxed, true},
		{"relaxed siblings share org domain", "a.example.com", "b.example.com", Relaxed, true},
		{"E co.uk relaxed", "example.co.uk", "mail.example.co.uk", Relaxed, true},
		{"E co.uk strict", "example.co.uk", "mail.example.co.uk", Strict, false},
		{"E co.uk vs other co.uk org", "example.co.uk", "other.co.uk", Relaxed, false},
		{"E co.uk suffix itself", "example.co.uk", "co.uk", Relaxed, false},
		{"F lookalike", "example.com", "attackerexample.com", Relaxed, false},
		{"F lookalike reversed", "attackerexample.com", "example.com", Relaxed, false},
		{"F lookalike strict", "example.com", "attackerexample.com", Strict, false},
		{"G attacker suffix", "example.com", "example.com.attacker.com", Relaxed, false},
		{"G attacker suffix strict", "example.com", "example.com.attacker.com", Strict, false},
		{"public suffix com", "example.com", "com", Relaxed, false},
		{"public suffix both", "co.uk", "co.uk", Strict, false},
		{"different tld", "example.com", "example.org", Relaxed, false},
		{"empty identity", "example.com", "", Relaxed, false},
		{"ip literal", "example.com", "127.0.0.1", Relaxed, false},
		{"idn is not aligned (unsupported)", "example.com", "exämple.com", Relaxed, false},
		{"unknown mode is relaxed", "example.com", "mail.example.com", Mode("x"), true},
	} {
		if got := Aligned(tc.from, tc.id, tc.mode); got != tc.want {
			t.Errorf("%s: Aligned(%q,%q,%s) = %v", tc.name, tc.from, tc.id, tc.mode, got)
		}
	}
}

func TestWalkNamesBoundedAndOrdered(t *testing.T) {
	for in, want := range map[string][]string{
		"example.com":       {"example.com"},
		"mail.example.com":  {"mail.example.com", "example.com"},
		"a.b.example.co.uk": {"a.b.example.co.uk", "b.example.co.uk", "example.co.uk"},
		"co.uk":             nil,
		"":                  nil,
	} {
		got := walkNames(in, maxQueries)
		if len(got) != len(want) {
			t.Errorf("walkNames(%q) = %v want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("walkNames(%q) = %v want %v", in, got, want)
			}
		}
	}
	deep := "a.b.c.d.e.f.g.h.i.j.k.l.example.com"
	if n := len(walkNames(deep, maxQueries)); n > maxQueries {
		t.Fatalf("walk exceeded the query bound: %d", n)
	}
}
