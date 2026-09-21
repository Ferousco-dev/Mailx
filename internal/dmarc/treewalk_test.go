package dmarc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/publicsuffix"
)

// pslOrg is the OLD (RFC 7489) determination, kept here only to prove where the
// RFC 9989 Tree Walk differs. It is not used by any production code.
func pslOrg(d string) string {
	o, err := publicsuffix.EffectiveTLDPlusOne(d)
	if err != nil {
		return d
	}
	return o
}

func walkOf(t *testing.T, start string, zone txtMap) (*walkResult, *fakeDNS) {
	t.Helper()
	dns := &fakeDNS{fn: zone.fn}
	w, err := (&Service{resolver: dns}).treeWalk(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	return w, dns
}

func queried(dns *fakeDNS) []string {
	dns.mu.Lock()
	defer dns.mu.Unlock()
	return append([]string(nil), dns.names...)
}

// RFC 9989 4.10: the 12-label example lists exactly these eight queries.
func TestTargetsMatchRFCExample(t *testing.T) {
	got := targets("a.b.c.d.e.f.g.h.i.j.mail.example.com")
	want := []string{"a.b.c.d.e.f.g.h.i.j.mail.example.com", "g.h.i.j.mail.example.com", "h.i.j.mail.example.com", "i.j.mail.example.com",
		"j.mail.example.com", "mail.example.com", "example.com", "com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v", got)
	}
	for in, n := range map[string]int{"com": 1, "example.com": 2, "a.mail.example.com": 4, "a.b.c.d.e.example.com": 7, "a.b.c.d.e.f.example.com": 8, "a.b.c.d.e.f.g.example.com": 8} {
		if got := targets(in); len(got) != n {
			t.Errorf("targets(%q) = %d names %v, want %d", in, len(got), got, n)
		}
	}
	// Absurdly deep, attacker-controlled names never exceed the cap.
	deep := strings.Repeat("x.", 120) + "example.com"
	if got := targets(deep); len(got) != maxQueries || got[len(got)-1] != "com" {
		t.Fatalf("deep: %d names", len(got))
	}
}

// RFC 9989 4.10.2 worked examples, verbatim.
func TestRFCOrganizationalDomainExamples(t *testing.T) {
	// Records at mail.example.com and example.com, none at the start or at com: fewest labels wins.
	w, _ := walkOf(t, "a.mail.example.com", txtMap{"_dmarc.mail.example.com": rec("v=DMARC1; p=none"), "_dmarc.example.com": rec("v=DMARC1; p=none")})
	if w.org != "example.com" {
		t.Fatalf("example 1: %s", w.org)
	}
	// psd=n at mail.example.com: it is the Organizational Domain and the walk stops.
	w, dns := walkOf(t, "a.mail.example.com", txtMap{"_dmarc.mail.example.com": rec("v=DMARC1; p=none; psd=n"), "_dmarc.example.com": rec("v=DMARC1; p=none")})
	if w.org != "mail.example.com" || len(queried(dns)) != 2 {
		t.Fatalf("example 2: %s %v", w.org, queried(dns))
	}
	// Only _dmarc.com with psd=y: the Organizational Domain is example.com.
	w, _ = walkOf(t, "a.mail.example.com", txtMap{"_dmarc.com": rec("v=DMARC1; p=reject; psd=y")})
	if w.org != "example.com" {
		t.Fatalf("example 3: %s", w.org)
	}
	// 4.10.2's own illustration: psd=y at example.com => mail.example.com.
	w, _ = walkOf(t, "mail.example.com", txtMap{"_dmarc.example.com": rec("v=DMARC1; p=none; psd=y")})
	if w.org != "mail.example.com" {
		t.Fatalf("psd=y example: %s", w.org)
	}
}

func TestPsdSemantics(t *testing.T) {
	// psd=y on the starting domain's own record does not make a one-label-below org; it stays the start.
	w, dns := walkOf(t, "example.com", txtMap{"_dmarc.example.com": rec("v=DMARC1; p=none; psd=y")})
	if w.org != "example.com" || len(queried(dns)) != 1 {
		t.Fatalf("psd=y at start: %s %v", w.org, queried(dns))
	}
	// psd=n stops the walk: nothing above it is queried or consulted.
	w, dns = walkOf(t, "a.b.example.com", txtMap{"_dmarc.b.example.com": rec("v=DMARC1; p=reject; psd=n"), "_dmarc.example.com": rec("v=DMARC1; p=none")})
	if w.org != "b.example.com" || len(queried(dns)) != 2 || w.policy == nil || w.policy.name != "b.example.com" || w.policy.source != SourceOrg {
		t.Fatalf("psd=n: %+v %v", w, queried(dns))
	}
	// psd=y at a parent: policy falls to the PSD record when the org itself has none (4.10.1 third preference).
	w, _ = walkOf(t, "a.mail.example.com", txtMap{"_dmarc.example.com": rec("v=DMARC1; p=quarantine; sp=reject; psd=y")})
	if w.org != "mail.example.com" || w.policy == nil || w.policy.source != SourcePSD || w.policy.name != "example.com" {
		t.Fatalf("psd=y policy: %+v", w)
	}
	// psd=y at a parent, but the organization below it published its own record: that record governs.
	w, _ = walkOf(t, "a.mail.example.com", txtMap{"_dmarc.mail.example.com": rec("v=DMARC1; p=none"), "_dmarc.example.com": rec("v=DMARC1; p=reject; psd=y")})
	if w.org != "mail.example.com" || w.policy == nil || w.policy.source != SourceOrg || w.policy.name != "mail.example.com" {
		t.Fatalf("org record beats PSD record: %+v %+v", w, w.policy)
	}
	// A malformed record still counts as a record for the walk, and its psd tag is honoured.
	w, dns = walkOf(t, "a.mail.example.com", txtMap{"_dmarc.mail.example.com": rec("v=DMARC1; p=bogus; psd=n"), "_dmarc.example.com": rec("v=DMARC1; p=none")})
	if w.org != "mail.example.com" || len(queried(dns)) != 2 || w.policy == nil || w.policy.invalid != ReasonBadPolicy {
		t.Fatalf("malformed psd=n: %+v", w)
	}
	// Unknown psd value is just "u": the walk continues.
	if w, _ = walkOf(t, "a.example.com", txtMap{"_dmarc.a.example.com": rec("v=DMARC1; p=none; psd=maybe"), "_dmarc.example.com": rec("v=DMARC1; p=none")}); w.org != "example.com" {
		t.Fatalf("psd=maybe: %s", w.org)
	}
}

// Cases where the old PSL determination and the RFC 9989 Tree Walk disagree.
// These are the evidence that the fix changed behavior, not just code location.
func TestOldPSLAndTreeWalkDisagree(t *testing.T) {
	type tc struct {
		name, from, id string
		zone           txtMap
		wantAligned    bool
	}
	for _, c := range []tc{
		{"psd=n makes a division its own organization: PSL says same org", "a.mail.example.com", "example.com",
			txtMap{"_dmarc.mail.example.com": rec("v=DMARC1; p=none; psd=n"), "_dmarc.example.com": rec("v=DMARC1; p=none")}, false},
		{"psd=y makes example.com a PSD: PSL says same org", "a.mail.example.com", "b.example.com",
			txtMap{"_dmarc.example.com": rec("v=DMARC1; p=none; psd=y")}, false},
		{"no published record anywhere: PSL says same org", "mail.example.com", "example.com", txtMap{}, false},
		{"a PSO publishing psd=n at co.uk makes it one organization: PSL says two", "a.example.co.uk", "b.other.co.uk",
			txtMap{"_dmarc.co.uk": rec("v=DMARC1; p=none; psd=n")}, true},
	} {
		got, known := Aligned(c.from, c.id, Relaxed, zoneOrg(t, c.zone))
		psl := pslOrg(c.from) == pslOrg(c.id)
		if !known || got != c.wantAligned {
			t.Errorf("%s: tree walk aligned=%v (want %v)", c.name, got, c.wantAligned)
		}
		if psl == got {
			t.Errorf("%s: test does not distinguish the algorithms (PSL=%v, walk=%v)", c.name, psl, got)
		}
	}
}

// Policy discovery uses the record at the Author Domain, else at the Organizational
// Domain, else at the PSD: an intermediate record is NOT the policy (RFC 9989 4.10.1).
func TestPolicyRecordSelectionFollowsRFC(t *testing.T) {
	w, _ := walkOf(t, "a.mail.example.com", txtMap{
		"_dmarc.mail.example.com": rec("v=DMARC1; p=reject"),
		"_dmarc.example.com":      rec("v=DMARC1; p=none"),
	})
	if w.org != "example.com" || w.policy == nil || w.policy.name != "example.com" || w.policy.source != SourceOrg {
		t.Fatalf("intermediate record must not govern: %+v %+v", w, w.policy)
	}
	w, _ = walkOf(t, "a.mail.example.com", txtMap{"_dmarc.a.mail.example.com": rec("v=DMARC1; p=reject"), "_dmarc.example.com": rec("v=DMARC1; p=none")})
	if w.policy == nil || w.policy.name != "a.mail.example.com" || w.policy.source != SourceDomain || w.org != "example.com" {
		t.Fatalf("the author's own record governs, org still from the walk: %+v %+v", w, w.policy)
	}
	if w, _ = walkOf(t, "a.example.com", txtMap{}); w.policy != nil || w.org != "a.example.com" {
		t.Fatalf("no records: %+v", w)
	}
}

// Multiple records at a name are all discarded and the walk CONTINUES (4.10 step 2),
// but MailX still reports the conflict so the owner can fix it.
func TestMultipleRecordsDiscardedWalkContinues(t *testing.T) {
	dup := []string{"v=DMARC1; p=none", "v=DMARC1; p=reject"}
	w, dns := walkOf(t, "mail.example.com", txtMap{"_dmarc.mail.example.com": dup, "_dmarc.example.com": rec("v=DMARC1; p=quarantine")})
	if !reflect.DeepEqual(w.conflicts, []string{"mail.example.com"}) || w.policy == nil || w.policy.name != "example.com" || w.org != "example.com" || len(queried(dns)) != 3 {
		t.Fatalf("%+v %+v %v", w, w.policy, queried(dns))
	}
	f := findingFrom(w)
	if f.status != StatusConflict || f.reason != ReasonMultiple || f.name != "mail.example.com" {
		t.Fatalf("author-level conflict must be surfaced as conflict: %+v", f)
	}
	// A conflict at an ancestor with a valid policy elsewhere is a warning, not a status change.
	w, _ = walkOf(t, "a.example.com", txtMap{"_dmarc.a.example.com": rec("v=DMARC1; p=none"), "_dmarc.example.com": dup})
	f = findingFrom(w)
	res := assess(input{from: "a.example.com", align: func(a, b string, m Mode) (bool, bool) { return Aligned(a, b, m, nil) }}, f)
	if f.status != StatusMonitoring || !contains(res.Warnings, WarnConflictDiscarded) {
		t.Fatalf("%+v %v", f, res.Warnings)
	}
	// Only a conflicting set and nothing else: conflict, never an arbitrary pick.
	w, _ = walkOf(t, "example.com", txtMap{"_dmarc.com": dup})
	if f = findingFrom(w); f.status != StatusConflict {
		t.Fatalf("%+v", f)
	}
}

// A DNS error anywhere in the walk ends it: no weaker parent policy, no guessed boundary.
func TestDNSErrorMidWalkFailsSafely(t *testing.T) {
	for _, bad := range []error{&net.DNSError{Err: "server misbehaving", IsTemporary: true}, &net.DNSError{IsTimeout: true, IsTemporary: true}, errors.New("boom"), context.DeadlineExceeded} {
		dns := &fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
			switch name {
			case "_dmarc.a.mail.example.com", "_dmarc.mail.example.com":
				return nil, notFound()
			case "_dmarc.example.com":
				return nil, bad // parent SERVFAIL after a clean "no record" at the child
			}
			return []string{"v=DMARC1; p=none"}, nil // .com would give a (weak) policy if consulted
		}}
		w, err := (&Service{resolver: dns}).treeWalk(context.Background(), "a.mail.example.com")
		if err == nil || w != nil {
			t.Fatalf("%v: walk must fail, got %+v", bad, w)
		}
		if names := queried(dns); names[len(names)-1] != "_dmarc.example.com" {
			t.Fatalf("walk continued past the failing name: %v", names)
		}
		// And alignment built on it is unknown, not 'not aligned'.
		svc := &Service{resolver: dns}
		org := func(d string) (string, bool) {
			w, err := svc.treeWalk(context.Background(), d)
			if err != nil {
				return "", false
			}
			return w.org, true
		}
		if a, k := Aligned("a.mail.example.com", "example.com", Relaxed, org); a || k {
			t.Fatalf("aligned=%v known=%v", a, k)
		}
	}
}

func TestWalkHostileAnswers(t *testing.T) {
	huge := "v=DMARC1; p=none; x=" + strings.Repeat("a", 100000)
	for name, zone := range map[string]txtMap{
		"oversized record":  {"_dmarc.example.com": rec(huge)},
		"too many records":  {"_dmarc.example.com": manyTXT()},
		"garbage":           {"_dmarc.example.com": {"\x00\x01", "v=DMARC1;;;;;;", strings.Repeat(";", 5000)}},
		"psd on bad record": {"_dmarc.example.com": rec(huge + "; psd=n")},
	} {
		w, _ := walkOf(t, "example.com", zone)
		if w == nil || w.queries > maxQueries {
			t.Fatalf("%s: %+v", name, w)
		}
	}
	// An oversized record with a psd tag past the size bound is 'u' (the scan is bounded).
	if scanPSD("v=DMARC1; "+strings.Repeat("a", MaxRecordBytes)+"; psd=n") != "u" {
		t.Fatal("scanPSD must be bounded")
	}
}

// Both the policy and the Organizational Domain come from ONE walk, so they can
// never disagree. Checked over many pseudo-random zones.
func TestPolicyAndOrganizationalDomainAreConsistent(t *testing.T) {
	rng := rand.New(rand.NewSource(9989))
	labelsPool := []string{"a", "b", "c", "mail", "example", "co", "uk", "com"}
	values := []string{"v=DMARC1; p=none", "v=DMARC1; p=reject; psd=y", "v=DMARC1; p=none; psd=n", "v=DMARC1; p=bogus", "v=DMARC1; p=none; psd=u"}
	for i := 0; i < 400; i++ {
		n := 1 + rng.Intn(12)
		parts := make([]string, n)
		for j := range parts {
			parts[j] = labelsPool[rng.Intn(len(labelsPool))]
		}
		start := strings.Join(parts, ".")
		zone := txtMap{}
		for _, name := range targets(start) {
			switch rng.Intn(5) {
			case 0:
				zone["_dmarc."+name] = rec(values[rng.Intn(len(values))])
			case 1:
				zone["_dmarc."+name] = []string{"v=DMARC1; p=none", "v=DMARC1; p=none"}
			}
		}
		dns := &fakeDNS{fn: zone.fn}
		w, err := (&Service{resolver: dns}).treeWalk(context.Background(), start)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(queried(dns)); got > maxQueries || got != w.queries {
			t.Fatalf("%s: %d queries (walk says %d)", start, got, w.queries)
		}
		// The organization is the start itself or a label-aligned ancestor of it.
		if w.org != start && !strings.HasSuffix(start, "."+w.org) {
			t.Fatalf("%s: org %q is not on the path", start, w.org)
		}
		if w.policy != nil {
			switch w.policy.source {
			case SourceDomain:
				if w.policy.name != start {
					t.Fatalf("%s: domain-sourced policy at %s", start, w.policy.name)
				}
			case SourceOrg:
				if w.policy.name != w.org || w.org == start {
					t.Fatalf("%s: org-sourced policy at %s but org is %s", start, w.policy.name, w.org)
				}
			case SourcePSD:
				if w.policy.name == start || !strings.HasSuffix(start, "."+w.policy.name) {
					t.Fatalf("%s: PSD policy at %s", start, w.policy.name)
				}
			default:
				t.Fatalf("unknown source %q", w.policy.source)
			}
		}
		// Determinism: the same zone yields the same answer.
		w2, _ := (&Service{resolver: &fakeDNS{fn: zone.fn}}).treeWalk(context.Background(), start)
		if w.org != w2.org || (w.policy == nil) != (w2.policy == nil) {
			t.Fatalf("%s: nondeterministic", start)
		}
	}
}

func TestOneLabelBelow(t *testing.T) {
	for _, c := range [][3]string{{"a.mail.example.com", "com", "example.com"}, {"a.mail.example.com", "example.com", "mail.example.com"},
		{"mail.example.com", "example.com", "mail.example.com"}, {"example.com", "com", "example.com"}} {
		if got := oneLabelBelow(c[0], c[1]); got != c[2] {
			t.Errorf("oneLabelBelow(%q,%q) = %q want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestWalkInputEdgeCases(t *testing.T) {
	for _, d := range []string{"com", "example.com", "a.b.c.d.e.f.g.h.i.j.k.example.com", strings.Repeat("a.", 100) + "com", "EXAMPLE.com"} {
		c, ok := canonical(d)
		if !ok {
			continue
		}
		w, dns := walkOf(t, c, txtMap{})
		if len(queried(dns)) > maxQueries || w.org != c {
			t.Fatalf("%q: %d queries, org %q", d, len(queried(dns)), w.org)
		}
	}
	_ = fmt.Sprint
}

func FuzzTreeWalkInputs(f *testing.F) {
	for _, s := range []string{"", ".", "com", "a.b.c.d.e.f.g.h.i.j.k.example.com", "EXAMPLE.COM.", "xn--a.com", "a..b", strings.Repeat("a.", 200) + "com", "co.uk", "*.example.com", "\x00.com"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		c, ok := canonical(in)
		if !ok {
			return
		}
		if n := len(targets(c)); n < 1 || n > maxQueries {
			t.Fatalf("%q: %d targets", c, n)
		}
		svc := &Service{resolver: &fakeDNS{}}
		w, err := svc.treeWalk(context.Background(), c)
		if err != nil || w.queries > maxQueries || w.org == "" {
			t.Fatalf("%q: %+v %v", c, w, err)
		}
		_, _ = Aligned(c, "example.com", Relaxed, func(string) (string, bool) { return c, true })
	})
}
