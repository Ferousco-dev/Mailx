package spf

import (
	"net/netip"
	"strings"
	"testing"
)

func TestIsSPF(t *testing.T) {
	for txt, want := range map[string]bool{
		"v=spf1": true, "V=SPF1 -all": true, "v=spf1 ip4:1.2.3.4": true,
		"v=spf10 -all": false, "v=spf2.0/pra": false, "v=spf1-all": false, "": false, "google-site=x": false,
		" v=spf1 -all": false,
	} {
		if IsSPF(txt) != want {
			t.Errorf("IsSPF(%q)=%v want %v", txt, !want, want)
		}
	}
}

func TestParseAccepts(t *testing.T) {
	for _, txt := range []string{
		"v=spf1",
		"v=spf1 -all",
		"v=spf1 ip4:203.0.113.9 ip6:2001:db8::1 ~all",
		"V=SPF1 IP4:203.0.113.0/24 INCLUDE:_spf.example.net -ALL",
		"v=spf1 ip4:198.51.100.7   mx a a:mail.example.com a/24 a//64 mx:x.example/24//96 ptr ptr:example.com exists:%{i}.example.com ?all",
		"v=spf1 redirect=_spf.example.com",
		"v=spf1 unknown-modifier=whatever exp=explain.example.com -all",
		"v=spf1 ip6:2001:db8::/32 ip4:0.0.0.0/0",
		"v=spf1 +ip4:1.2.3.4 -ip4:5.6.7.8 ~ip6:::1 ?ip4:9.9.9.9",
	} {
		if _, err := Parse(txt); err != nil {
			t.Errorf("Parse(%q): %v", txt, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	long := "v=spf1 " + strings.Repeat("a", MaxTermBytes+1)
	many := "v=spf1" + strings.Repeat(" a", MaxTerms+1)
	cases := map[string]string{
		"v=spf1 ip4:1.2.3.4/33":        ReasonBadCIDR,
		"v=spf1 ip4:1.2.3.4/":          ReasonBadCIDR,
		"v=spf1 ip4:1.2.3.4/-1":        ReasonBadCIDR,
		"v=spf1 ip4:1.2.3.4/abc":       ReasonBadCIDR,
		"v=spf1 ip6:2001:db8::/129":    ReasonBadCIDR,
		"v=spf1 a/33":                  ReasonBadCIDR,
		"v=spf1 a//129":                ReasonBadCIDR,
		"v=spf1 a//":                   ReasonBadCIDR,
		"v=spf1 ip4:999.1.1.1":         ReasonBadIP,
		"v=spf1 ip4:2001:db8::1":       ReasonBadIP,
		"v=spf1 ip6:1.2.3.4":           ReasonBadIP,
		"v=spf1 ip4:":                  ReasonBadIP,
		"v=spf1 ip4":                   ReasonBadIP,
		"v=spf1 ip4:01.2.3.4":          ReasonBadIP,
		"v=spf1 ip6:fe80::1%eth0":      ReasonBadIP,
		"v=spf1 ip4:1.2.3.4 bogus":     ReasonUnknownMechanism,
		"v=spf1 ip4:1.2.3.4 -":         ReasonUnknownMechanism,
		"v=spf1 all:example.com":       ReasonBadArgument,
		"v=spf1 include":               ReasonBadArgument,
		"v=spf1 include:":              ReasonBadArgument,
		"v=spf1 exists":                ReasonBadArgument,
		"v=spf1 redirect=":             ReasonBadArgument,
		"v=spf1 redirect=a redirect=b": ReasonDuplicateModifier,
		"v=spf1 exp=a exp=b":           ReasonDuplicateModifier,
		"v=spf1 1bad=x":                ReasonBadModifier,
		"v=spf1 bad name=x":            ReasonUnknownMechanism,
		"v=spf1\tip4:1.2.3.4":          ReasonBadEncoding,
		"v=spf1 ip4:1.2.3.4\x00":       ReasonBadEncoding,
		"v=spf1 ip4:1.2.3.4 \u00e9":    ReasonBadEncoding,
		"v=spf1 ip4:1.2.3.4\r\n -all":  ReasonBadEncoding,
		long:                           ReasonTermTooLong,
		many:                           ReasonTooManyTerms,
		"v=spf1 " + strings.Repeat("a ", MaxRecordBytes): ReasonTooLong,
	}
	for txt, want := range cases {
		_, err := Parse(txt)
		ie, ok := err.(*InvalidError)
		if !ok || ie.Reason != want {
			t.Errorf("Parse(%.40q) = %v, want reason %s", txt, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "203.0") {
			t.Errorf("error leaks record text: %v", err)
		}
	}
}

func TestCoversOrderingAndQualifiers(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.10")
	for _, tc := range []struct {
		txt  string
		want Coverage
	}{
		{"v=spf1 ip4:203.0.113.10 -all", Authorized},
		{"v=spf1 ip4:203.0.113.0/24 -all", Authorized},
		{"v=spf1 +ip4:203.0.113.10", Authorized},
		{"v=spf1 -ip4:203.0.113.10 ip4:203.0.113.10", Denied},
		{"v=spf1 ~ip4:203.0.113.10", Denied},
		{"v=spf1 ?ip4:203.0.113.0/24", Denied},
		{"v=spf1 -all ip4:203.0.113.10", NotListed}, // terms after all are ignored
		{"v=spf1 include:_spf.example.net -all", NotListed},
		{"v=spf1 ip4:203.0.113.11 -all", NotListed},
		{"v=spf1 ip4:203.0.113.99/24 -all", Authorized}, // host bits are masked
		{"v=spf1 ip6:2001:db8::1 -all", NotListed},
		{"v=spf1 ip4:0.0.0.0/0", Authorized},
	} {
		rec, err := Parse(tc.txt)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.Covers(ip); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.txt, got, tc.want)
		}
	}
	rec, _ := Parse("v=spf1 ip6:2001:db8:aa::/48 -all")
	if rec.Covers(netip.MustParseAddr("2001:db8:aa::77")) != Authorized {
		t.Fatal("ipv6 CIDR not honored")
	}
	rec, _ = Parse("v=spf1 ip4:203.0.113.10 -all")
	if rec.Covers(netip.MustParseAddr("::ffff:203.0.113.10")) != Authorized {
		t.Fatal("IPv4-mapped IPv6 must match the IPv4 mechanism")
	}
}

func TestRecordProperties(t *testing.T) {
	rec, _ := Parse("v=spf1 include:_spf.example.net a mx ptr exists:x.example redirect=y.example ip4:1.2.3.4 +all")
	if !rec.Unevaluated() || rec.DNSTerms() != 6 || !rec.PermitsAll() {
		t.Fatalf("unexpected properties: uneval=%v dns=%d all=%v", rec.Unevaluated(), rec.DNSTerms(), rec.PermitsAll())
	}
	if !rec.Includes("_SPF.Example.NET.") {
		t.Fatal("include compare must ignore case and trailing dot")
	}
	rec, _ = Parse("v=spf1 -all include:_spf.example.net")
	if rec.Includes("_spf.example.net") {
		t.Fatal("include after all is unreachable and must not count")
	}
	rec, _ = Parse("v=spf1 -include:_spf.example.net -all")
	if rec.Includes("_spf.example.net") {
		t.Fatal("a non-pass include must not count as authorization")
	}
	rec, _ = Parse("v=spf1 ip4:1.2.3.4 -all")
	if rec.Unevaluated() {
		t.Fatal("literal-only record is fully evaluated")
	}
}

func TestMergeKeepsOneRecordAndFrontInserts(t *testing.T) {
	rec, _ := Parse("v=spf1 include:_spf.google.com -ip4:203.0.113.10 ~all")
	got, err := rec.Merge([]string{"ip4:203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "v=spf1 ip4:203.0.113.10 include:_spf.google.com -ip4:203.0.113.10 ~all" {
		t.Fatalf("merge = %q", got)
	}
	if strings.Count(got, "v=spf1") != 1 {
		t.Fatal("merge must produce exactly one record")
	}
	big, err := Parse("v=spf1 " + strings.Repeat("include:"+strings.Repeat("a", 25)+" ", 60))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := big.Merge([]string{"ip4:9.9.9.9"}); err == nil {
		t.Fatal("oversized merge must be refused")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"v=spf1 -all", "v=spf1 ip4:1.2.3.4/24 a//64", "v=spf1 redirect=x", "v=spf1 %{ } ip6:::", "v=spf1 a:/", "v=spf1 =", "v=spf1 +", "v=spf1 /"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		rec, err := Parse(s)
		if err != nil {
			return
		}
		_ = rec.Covers(netip.MustParseAddr("203.0.113.10"))
		_ = rec.Unevaluated()
		_, _ = rec.Merge([]string{"ip4:9.9.9.9"})
	})
}
