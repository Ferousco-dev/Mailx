package dmarc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func notFound() error { return &net.DNSError{Err: "no such host", IsNotFound: true} }

// --- fakes ------------------------------------------------------------------

type fakeStore struct {
	mu      sync.Mutex
	domains map[string]database.Domain
	err     error
}

func (s *fakeStore) add(tenant, id, name string, verified bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.domains == nil {
		s.domains = map[string]database.Domain{}
	}
	st := database.DomainPending
	if verified {
		st = database.DomainVerified
	}
	s.domains[tenant+"|"+id] = database.Domain{ID: id, TenantID: tenant, Name: name, VerificationStatus: st}
}

func (s *fakeStore) GetDomain(_ context.Context, tenant, id string) (database.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return database.Domain{}, s.err
	}
	d, ok := s.domains[tenant+"|"+id]
	if !ok {
		return database.Domain{}, database.ErrNotFound
	}
	return d, nil
}

type fakeDNS struct {
	calls atomic.Int64
	mu    sync.Mutex
	names []string
	fn    func(ctx context.Context, name string) ([]string, error)
}

func (d *fakeDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	d.calls.Add(1)
	d.mu.Lock()
	d.names = append(d.names, name)
	d.mu.Unlock()
	if d.fn == nil {
		return nil, notFound()
	}
	return d.fn(ctx, name)
}

type txtMap map[string][]string

func (m txtMap) fn(_ context.Context, name string) ([]string, error) {
	if r, ok := m[name]; ok {
		return r, nil
	}
	return nil, notFound()
}

type fakeDKIM struct {
	domain string
	active bool
	err    error
}

func (f fakeDKIM) ActiveSigningDomain(context.Context, string, string) (string, bool, error) {
	return f.domain, f.active, f.err
}

type fakeSPF struct {
	view SPFView
	err  error
}

func (f fakeSPF) SPFView(_ context.Context, _, _ string, check bool) (SPFView, error) {
	v := f.view
	if !check {
		v.Status = ""
	}
	return v, f.err
}

type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) DMARCResult(dns, readiness string) {
	r.mu.Lock()
	r.got = append(r.got, dns+"/"+readiness)
	r.mu.Unlock()
}

func newSvc(t *testing.T, st *fakeStore, dns *fakeDNS, dk DKIMState, sp SPFState, obs Observer) *Service {
	t.Helper()
	s, err := NewService(st, dns, dk, sp, obs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const dom = "example.com"

func directSPF(status string) fakeSPF { return fakeSPF{view: SPFView{Mode: "direct", Status: status}} }

// --- DNS matrix -------------------------------------------------------------

func verifyOnce(t *testing.T, name string, dns *fakeDNS, dk DKIMState, sp SPFState) Result {
	t.Helper()
	st := &fakeStore{}
	st.add("t", "d", name, true)
	res, err := newSvc(t, st, dns, dk, sp, nil).Verify(context.Background(), "t", "d")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDNSMatrix(t *testing.T) {
	tests := []struct {
		name     string
		records  map[string][]string
		err      error
		status   DNSStatus
		reason   string
		action   string
		effect   Policy
		wantWarn string
	}{
		{"A no record", nil, nil, StatusNotConfigured, "", "create", "", ""},
		{"B p=none", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none;"}}, nil, StatusMonitoring, "", "none", PolicyNone, WarnMonitoringOnly},
		{"C p=quarantine", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=quarantine"}}, nil, StatusEnforcing, "", "none", PolicyQuarantine, ""},
		{"D p=reject", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=reject; adkim=s; aspf=s"}}, nil, StatusEnforcing, "", "none", PolicyReject, ""},
		{"J bad version", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none", "not dmarc"}}, nil, StatusMonitoring, "", "none", PolicyNone, ""},
		{"K missing policy", map[string][]string{"_dmarc.example.com": {"v=DMARC1; adkim=s"}}, nil, StatusInvalid, ReasonMissingPolicy, "fix_record", "", ""},
		{"L duplicate tag", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none; p=reject"}}, nil, StatusInvalid, ReasonDuplicateTag, "fix_record", "", ""},
		{"M invalid policy", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=drop"}}, nil, StatusInvalid, ReasonBadPolicy, "fix_record", "", ""},
		{"N invalid alignment", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none; adkim=x"}}, nil, StatusInvalid, ReasonBadAlignment, "fix_record", "", ""},
		{"O pct is removed (RFC 9989): ignored, warned", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=reject; pct=25"}}, nil, StatusEnforcing, "", "none", PolicyReject, WarnDeprecatedTag},
		{"P unknown extension tag", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none; x-foo=bar"}}, nil, StatusMonitoring, "", "none", PolicyNone, ""},
		{"Q multiple records", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none", "v=DMARC1; p=reject"}}, nil, StatusConflict, ReasonMultiple, "merge_records", "", ""},
		{"R oversized record", map[string][]string{"_dmarc.example.com": {"v=DMARC1; p=none; x=" + strings.Repeat("a", 5000)}}, nil, StatusInvalid, ReasonTooLong, "fix_record", "", ""},
		{"too many TXT records", map[string][]string{"_dmarc.example.com": manyTXT()}, nil, StatusInvalid, ReasonTooManyTXT, "fix_record", "", ""},
		{"S temporary DNS error", nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}, StatusTempError, ReasonDNS, "retry_later", "", ""},
		{"T DNS timeout", nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true, IsTemporary: true}, StatusTempError, ReasonDNS, "retry_later", "", ""},
		{"unknown resolver error is temporary", nil, errors.New("boom"), StatusTempError, ReasonDNS, "retry_later", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dns *fakeDNS
			if tc.err != nil {
				dns = &fakeDNS{fn: func(context.Context, string) ([]string, error) { return nil, tc.err }}
			} else {
				dns = &fakeDNS{fn: txtMap(tc.records).fn}
			}
			res := verifyOnce(t, dom, dns, fakeDKIM{domain: dom, active: true}, directSPF("verified"))
			if res.DNS.Status != tc.status || res.DNS.Reason != tc.reason || res.Expected.Action != tc.action || res.DNS.Effective != tc.effect {
				t.Fatalf("%+v", res)
			}
			if tc.wantWarn != "" && !contains(res.Warnings, tc.wantWarn) {
				t.Fatalf("missing warning %s: %v", tc.wantWarn, res.Warnings)
			}
			if tc.action == "create" && res.Expected.Value != "v=DMARC1; p=none" || tc.action != "create" && res.Expected.Value != "" {
				t.Fatalf("recommendation value: %+v", res.Expected)
			}
			if res.Expected.Value != "" && strings.Count(strings.ToLower(res.Expected.Value), "v=dmarc1") != 1 {
				t.Fatal("must never recommend more than one record")
			}
		})
	}
}

func manyTXT() []string {
	out := make([]string, MaxTXTRecords+1)
	for i := range out {
		out[i] = fmt.Sprintf("noise-%d", i)
	}
	return out
}

// An existing policy is never rewritten: a stricter policy than MailX's default is preserved.
func TestExistingPolicyIsPreservedNeverDowngraded(t *testing.T) {
	dns := &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=reject; rua=mailto:r@example.com"}}.fn}
	res := verifyOnce(t, dom, dns, fakeDKIM{domain: dom, active: true}, directSPF("verified"))
	if res.Expected.Action != "none" || res.Expected.Value != "" || res.DNS.Effective != PolicyReject {
		t.Fatalf("existing p=reject must be left alone: %+v", res)
	}
}

// --- inheritance and the bounded walk ---------------------------------------

func TestPolicyDiscoveryWalksToOrganizationalDomainAndIsBounded(t *testing.T) {
	// Subdomain with no own record inherits the parent's; sp applies to existing subdomains.
	dns := &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=reject; sp=none"}}.fn}
	res := verifyOnce(t, "mail.example.com", dns, fakeDKIM{domain: "mail.example.com", active: true}, directSPF("verified"))
	if res.DNS.Source != SourceOrg || res.DNS.Policy != PolicyReject || res.DNS.Effective != PolicyNone || res.DNS.Status != StatusMonitoring {
		t.Fatalf("inheritance: %+v", res.DNS)
	}
	if got := dns.names; len(got) != 3 || got[0] != "_dmarc.mail.example.com" || got[1] != "_dmarc.example.com" || got[2] != "_dmarc.com" {
		t.Fatalf("queries (RFC 9989 walks to the TLD): %v", got)
	}
	// Without sp the parent's p applies.
	dns = &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=quarantine"}}.fn}
	if res = verifyOnce(t, "mail.example.com", dns, nil, nil); res.DNS.Effective != PolicyQuarantine || res.DNS.Status != StatusEnforcing {
		t.Fatalf("%+v", res.DNS)
	}
	// The domain's own record governs; the walk still continues to find the Organizational Domain.
	dns = &fakeDNS{fn: txtMap{"_dmarc.mail.example.com": {"v=DMARC1; p=none"}, "_dmarc.example.com": {"v=DMARC1; p=reject"}}.fn}
	if res = verifyOnce(t, "mail.example.com", dns, nil, nil); res.DNS.Source != SourceDomain || res.DNS.Effective != PolicyNone || dns.calls.Load() != 3 || res.OrgDomain != "example.com" {
		t.Fatalf("%+v calls=%d org=%s", res.DNS, dns.calls.Load(), res.OrgDomain)
	}
	// A conflict at the domain is reported even though receivers would fall back to the parent's policy.
	dns = &fakeDNS{fn: txtMap{"_dmarc.mail.example.com": {"v=DMARC1; p=none", "v=DMARC1; p=none"}, "_dmarc.example.com": {"v=DMARC1; p=reject"}}.fn}
	if res = verifyOnce(t, "mail.example.com", dns, nil, nil); res.DNS.Status != StatusConflict || res.DNS.Effective != "" {
		t.Fatalf("%+v", res.DNS)
	}
	// Never more than the query bound, even for absurdly deep names.
	dns = &fakeDNS{}
	verifyOnce(t, "a.b.c.d.e.f.g.h.i.j.k.l.m.n.example.com", dns, nil, nil)
	if dns.calls.Load() > maxQueries {
		t.Fatalf("%d queries", dns.calls.Load())
	}
	// A temporary error at the child never lets the parent's (possibly weaker) policy win.
	dns = &fakeDNS{fn: func(_ context.Context, n string) ([]string, error) {
		if n == "_dmarc.mail.example.com" {
			return nil, &net.DNSError{IsTimeout: true, IsTemporary: true}
		}
		return []string{"v=DMARC1; p=none"}, nil
	}}
	if res = verifyOnce(t, "mail.example.com", dns, nil, nil); res.DNS.Status != StatusTempError {
		t.Fatalf("%+v", res.DNS)
	}
}

// --- readiness matrix: alignment/configuration, never a receiver result --------

func TestReadinessMatrix(t *testing.T) {
	monitoring := txtMap{"_dmarc.example.com": {"v=DMARC1; p=none"}}
	dk := func(active bool) fakeDKIM { return fakeDKIM{domain: dom, active: active} }
	tests := []struct {
		name      string
		dkim      DKIMState
		spf       SPFState
		dnsRecs   txtMap
		readiness string
		dkimPath  string
		spfPath   string
	}{
		{"1 DKIM+SPF aligned and configured", dk(true), directSPF("verified"), monitoring, ReadinessReady, PathReady, PathReady},
		{"2 DKIM ready, SPF configured wrongly", dk(true), directSPF("mismatch"), monitoring, ReadinessReady, PathReady, PathNotConfigured},
		{"3 DKIM inactive, SPF ready", dk(false), directSPF("verified"), monitoring, ReadinessReady, PathNotConfigured, PathReady},
		{"4 neither configured", dk(false), directSPF("not_configured"), monitoring, ReadinessAuthIncomp, PathNotConfigured, PathNotConfigured},
		{"5 DKIM unavailable + SPF ready", fakeDKIM{err: errors.New("db")}, directSPF("verified"), monitoring, ReadinessReady, PathUnknown, PathReady},
		{"6 SPF unavailable + DKIM ready", dk(true), fakeSPF{err: errors.New("x")}, monitoring, ReadinessReady, PathReady, PathUnknown},
		{"7 both unavailable", fakeDKIM{err: errors.New("db")}, fakeSPF{err: errors.New("x")}, monitoring, ReadinessUnknown, PathUnknown, PathUnknown},
		{"8 relay: SPF identity externally managed", dk(true), fakeSPF{view: SPFView{Mode: "relay", Status: "verified"}}, monitoring, ReadinessReady, PathReady, PathUnknown},
		{"8b relay without DKIM cannot be called ready", dk(false), fakeSPF{view: SPFView{Mode: "relay", Status: "verified"}}, monitoring, ReadinessUnknown, PathNotConfigured, PathUnknown},
		{"spf temporary error is unknown, not a misconfiguration", dk(false), directSPF("temporary_error"), monitoring, ReadinessUnknown, PathNotConfigured, PathUnknown},
		{"no DMARC record needs DNS action even if aligned", dk(true), directSPF("verified"), nil, ReadinessDNSAction, PathReady, PathReady},
		{"nil dependencies degrade to unknown", nil, nil, monitoring, ReadinessUnknown, PathNotConfigured, PathUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dns *fakeDNS
			dns = &fakeDNS{fn: tc.dnsRecs.fn}
			if tc.dnsRecs == nil {
				dns = &fakeDNS{}
			}
			res := verifyOnce(t, dom, dns, tc.dkim, tc.spf)
			if res.Readiness != tc.readiness || res.DKIM.Status != tc.dkimPath && !(tc.dkim == nil) || res.SPF.Status != tc.spfPath {
				t.Fatalf("readiness=%s dkim=%+v spf=%+v", res.Readiness, res.DKIM, res.SPF)
			}
		})
	}
}

// Strict vs relaxed changes the outcome only when identities differ from the From domain.
func TestStrictAndRelaxedModesDriveAlignmentOfPaths(t *testing.T) {
	// A DKIM signing domain that is a subdomain: aligned under adkim=r, not under adkim=s.
	sub := fakeDKIM{domain: "mail.example.com", active: true}
	relaxed := &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=none; adkim=r"}}.fn}
	strict := &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=none; adkim=s"}}.fn}
	if r := verifyOnce(t, dom, relaxed, sub, directSPF("verified")); r.DKIM.Status != PathReady || !r.DKIM.Aligned || r.DKIM.Mode != Relaxed {
		t.Fatalf("relaxed: %+v", r.DKIM)
	}
	if r := verifyOnce(t, dom, strict, sub, directSPF("verified")); r.DKIM.Status != PathNotAligned || r.DKIM.Aligned || r.DKIM.Mode != Strict {
		t.Fatalf("strict: %+v", r.DKIM)
	}
	// A foreign signing domain never aligns, in either mode.
	foreign := fakeDKIM{domain: "attackerexample.com", active: true}
	if r := verifyOnce(t, dom, relaxed, foreign, directSPF("verified")); r.DKIM.Status != PathNotAligned {
		t.Fatalf("%+v", r.DKIM)
	}
	// aspf strict with MAIL FROM == From (direct mode) stays aligned.
	sstrict := &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=none; aspf=s"}}.fn}
	if r := verifyOnce(t, dom, sstrict, nil, directSPF("verified")); r.SPF.Status != PathReady || r.SPF.Mode != Strict {
		t.Fatalf("%+v", r.SPF)
	}
}

// Relaxed alignment of BOTH paths follows the tree-walk Organizational Domain.
// Every zone here is one where the old PSL logic would say "aligned".
func TestPathAlignmentUsesTreeWalkForDKIMAndSPF(t *testing.T) {
	zone := txtMap{
		"_dmarc.example.com":      {"v=DMARC1; p=none; adkim=r; aspf=r"},
		"_dmarc.mail.example.com": {"v=DMARC1; p=none; psd=n"}, // the mail.example.com division is its own organization
	}
	svc := &Service{resolver: &fakeDNS{fn: zone.fn}}
	align := func(from, id string, mode Mode) (bool, bool) {
		return Aligned(from, id, mode, func(d string) (string, bool) {
			w, err := svc.treeWalk(context.Background(), d)
			if err != nil {
				return "", false
			}
			return w.org, true
		})
	}
	base := input{from: "example.com", checked: true, dkimActive: true, spf: SPFView{Mode: "direct", Status: "verified"}, align: align}
	for _, tc := range []struct {
		name        string
		dkimDomain  string
		spfIdentity string
		dkim, spf   string
	}{
		{"both aligned", "example.com", "example.com", PathReady, PathReady},
		{"DKIM aligned, SPF identity in another org division", "example.com", "a.mail.example.com", PathReady, PathNotAligned},
		{"DKIM in another org division, SPF aligned", "a.mail.example.com", "example.com", PathNotAligned, PathReady},
		{"neither aligned", "a.mail.example.com", "b.mail.example.com", PathNotAligned, PathNotAligned},
		{"foreign domains", "attacker.net", "attackerexample.com", PathNotAligned, PathNotAligned},
	} {
		in := base
		in.dkimDomain, in.spfIdentity = tc.dkimDomain, tc.spfIdentity
		dk, sp := paths(in, Relaxed, Relaxed)
		if dk.Status != tc.dkim || sp.Status != tc.spf {
			t.Errorf("%s: dkim=%+v spf=%+v", tc.name, dk, sp)
		}
	}
	// Strict needs no DNS and ignores the walk.
	in := base
	in.dkimDomain, in.spfIdentity = "mail.example.com", "mail.example.com"
	if dk, sp := paths(in, Strict, Strict); dk.Status != PathNotAligned || sp.Status != PathNotAligned {
		t.Fatalf("strict: %+v %+v", dk, sp)
	}
	// The identities of the same organization division DO align (both under mail.example.com).
	in.from = "a.mail.example.com"
	in.dkimDomain, in.spfIdentity = "b.mail.example.com", "c.mail.example.com"
	if dk, sp := paths(in, Relaxed, Relaxed); dk.Status != PathReady || sp.Status != PathReady {
		t.Fatalf("same division: %+v %+v", dk, sp)
	}
	// A DNS failure while determining the boundary is unknown, never a false 'not aligned' or 'ready'.
	in.align = func(f, id string, m Mode) (bool, bool) {
		return Aligned(f, id, m, func(string) (string, bool) { return "", false })
	}
	if dk, sp := paths(in, Relaxed, Relaxed); dk.Status != PathUnknown || dk.Reason != ReasonDNS || sp.Status != PathUnknown {
		t.Fatalf("dns failure: %+v %+v", dk, sp)
	}
}

// Service-level: a SERVFAIL at the parent after a clean miss at the child fails the
// whole discovery; the weaker grandparent policy is never used and readiness is not claimed.
func TestParentServfailAfterChildMissIsTemporaryAndNeverReady(t *testing.T) {
	dns := &fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		switch name {
		case "_dmarc.a.example.com":
			return nil, notFound()
		case "_dmarc.example.com":
			return nil, &net.DNSError{Err: "server failure", IsTemporary: true}
		}
		return []string{"v=DMARC1; p=none"}, nil
	}}
	res := verifyOnce(t, "a.example.com", dns, fakeDKIM{domain: "a.example.com", active: true}, directSPF("verified"))
	if res.DNS.Status != StatusTempError || res.Readiness != ReadinessUnknown || res.DNS.Effective != "" || res.OrgDomain != "" {
		t.Fatalf("%+v", res)
	}
	if names := dns.names; names[len(names)-1] != "_dmarc.example.com" {
		t.Fatalf("queried past the failure: %v", names)
	}
}

func TestWarningsForReportingAndTesting(t *testing.T) {
	dns := &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=none; t=y; rua=mailto:agg@thirdparty.org; ruf=mailto:f@example.com"}}.fn}
	res := verifyOnce(t, dom, dns, fakeDKIM{domain: dom, active: true}, directSPF("verified"))
	for _, w := range []string{WarnTesting, WarnExternalReportDest, WarnFailureReporting, WarnMonitoringOnly} {
		if !contains(res.Warnings, w) {
			t.Errorf("missing %s in %v", w, res.Warnings)
		}
	}
	dns = &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=none; rua=mailto:agg@example.com"}}.fn}
	if res = verifyOnce(t, dom, dns, nil, nil); contains(res.Warnings, WarnExternalReportDest) {
		t.Fatal("same-organization report destination is not external")
	}
	// Relay + ready DKIM warns that the relay may alter signed content.
	dns = &fakeDNS{fn: txtMap{"_dmarc.example.com": {"v=DMARC1; p=none"}}.fn}
	res = verifyOnce(t, dom, dns, fakeDKIM{domain: dom, active: true}, fakeSPF{view: SPFView{Mode: "relay"}})
	if !contains(res.Warnings, WarnRelayAlters) {
		t.Fatalf("%v", res.Warnings)
	}
}

func TestDescribeMakesNoDNSQueryAndIsUnchecked(t *testing.T) {
	st, dns := &fakeStore{}, &fakeDNS{}
	st.add("t", "d", dom, false) // even an unverified domain may read guidance
	svc := newSvc(t, st, dns, fakeDKIM{domain: dom, active: true}, directSPF("verified"), nil)
	res, err := svc.Describe(context.Background(), "t", "d")
	if err != nil || dns.calls.Load() != 0 || res.Checked || res.Readiness != ReadinessUnchecked || res.DNS.Status != StatusUnchecked ||
		res.Expected.Action != "create" || res.Expected.Name != "_dmarc.example.com" || res.Expected.Value != "v=DMARC1; p=none" {
		t.Fatalf("%+v %v calls=%d", res, err, dns.calls.Load())
	}
	if res.SPF.Status != PathUnknown || res.SPF.Reason != ReasonNotChecked {
		t.Fatalf("SPF must not be claimed without a check: %+v", res.SPF)
	}
}

// --- ownership, isolation, failure injection, concurrency --------------------

func TestOwnershipAndTenantIsolation(t *testing.T) {
	st, dns := &fakeStore{}, &fakeDNS{}
	st.add("A", "d", dom, true)
	st.add("A", "p", "pending.example.com", false)
	svc := newSvc(t, st, dns, nil, nil, nil)
	ctx := context.Background()
	if _, err := svc.Verify(ctx, "B", "d"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant verify: %v", err)
	}
	if _, err := svc.Describe(ctx, "B", "d"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant describe: %v", err)
	}
	if _, err := svc.Verify(ctx, "A", "p"); !errors.Is(err, ErrDomainNotVerified) {
		t.Fatalf("unverified: %v", err)
	}
	if dns.calls.Load() != 0 {
		t.Fatal("DNS was queried for another tenant's or an unverified domain")
	}
}

func TestFailureInjection(t *testing.T) {
	st := &fakeStore{}
	st.add("t", "d", dom, true)
	before := runtime.NumGoroutine()
	hang := &fakeDNS{fn: func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() }}
	svc := newSvc(t, st, hang, nil, nil, nil)
	svc.timeout = 50 * time.Millisecond
	start := time.Now()
	res, err := svc.Verify(context.Background(), "t", "d")
	if err != nil || res.DNS.Status != StatusTempError || time.Since(start) > 2*time.Second {
		t.Fatalf("timeout: %+v %v", res.DNS, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	blocked := &fakeDNS{fn: func(c context.Context, _ string) ([]string, error) { cancel(); <-c.Done(); return nil, c.Err() }}
	if _, err := newSvc(t, st, blocked, nil, nil, nil).Verify(ctx, "t", "d"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	st.err = errors.New("db down")
	if _, err := newSvc(t, st, &fakeDNS{}, nil, nil, nil).Verify(context.Background(), "t", "d"); err == nil {
		t.Fatal("store failure must be an error, not a status")
	}
	st.err = nil
	if _, err := newSvc(t, st, &fakeDNS{}, nil, nil, panicObserver{}).Verify(context.Background(), "t", "d"); err != nil {
		t.Fatalf("observer panic escaped: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if leaked := runtime.NumGoroutine() - before; leaked > 2 {
		t.Fatalf("goroutine leak: %d", leaked)
	}
}

type panicObserver struct{}

func (panicObserver) DMARCResult(string, string) { panic("observer bug") }

func TestConcurrentVerificationIsolatedAndBounded(t *testing.T) {
	st := &fakeStore{}
	var inflight, peak atomic.Int64
	dns := &fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer inflight.Add(-1)
		time.Sleep(time.Millisecond)
		switch {
		case strings.Contains(name, "monitor"):
			return []string{"v=DMARC1; p=none; adkim=s"}, nil
		case strings.Contains(name, "enforce"):
			return []string{"v=DMARC1; p=reject"}, nil
		case strings.Contains(name, "dup"):
			return []string{"v=DMARC1; p=none", "v=DMARC1; p=reject"}, nil
		case strings.Contains(name, "bad"):
			return []string{"v=DMARC1; p=nope"}, nil
		case strings.Contains(name, "slow"):
			return nil, &net.DNSError{IsTimeout: true, IsTemporary: true}
		case strings.HasPrefix(name, "_dmarc.psdn"):
			return []string{"v=DMARC1; p=none; psd=n"}, nil
		case strings.HasPrefix(name, "_dmarc.psdy"): // a PSD record above the domain: governs via the PSD source
			return []string{"v=DMARC1; p=reject; sp=none; psd=y"}, nil
		}
		return nil, notFound()
	}}
	want := map[string]DNSStatus{}
	for tn := 0; tn < 6; tn++ {
		for i, kind := range []string{"monitor", "enforce", "dup", "bad", "slow", "none", "psdn", "psdy"} {
			tenant, id := fmt.Sprintf("t%d", tn), fmt.Sprintf("d%d", i)
			domain := fmt.Sprintf("%s-%d.example.com", kind, tn)
			if kind == "psdy" {
				domain = "x." + domain // the psd=y record sits ABOVE the domain, at psdy-N.example.com
			}
			st.add(tenant, id, domain, true)
			want[tenant+"|"+id] = map[string]DNSStatus{"monitor": StatusMonitoring, "enforce": StatusEnforcing, "dup": StatusConflict,
				"bad": StatusInvalid, "slow": StatusTempError, "none": StatusNotConfigured, "psdn": StatusMonitoring, "psdy": StatusMonitoring}[kind]
		}
	}
	obs := &recorder{}
	svc := newSvc(t, st, dns, fakeDKIM{active: true, domain: "x.example.com"}, directSPF("verified"), obs)
	var wg sync.WaitGroup
	for rep := 0; rep < 6; rep++ {
		for key, status := range want {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tenant, id, _ := strings.Cut(key, "|")
				res, err := svc.Verify(context.Background(), tenant, id)
				if err != nil || res.DNS.Status != status {
					t.Errorf("%s: %s %v", key, res.DNS.Status, err)
				}
			}()
		}
	}
	wg.Wait()
	if peak.Load() > maxConcurrent {
		t.Fatalf("DNS concurrency %d > bound %d", peak.Load(), maxConcurrent)
	}
	if len(obs.got) != len(want)*6 {
		t.Fatalf("observed %d of %d", len(obs.got), len(want)*6)
	}
}

func TestBusyWhenSaturated(t *testing.T) {
	st := &fakeStore{}
	st.add("t", "d", dom, true)
	release := make(chan struct{})
	dns := &fakeDNS{fn: func(context.Context, string) ([]string, error) { <-release; return nil, notFound() }}
	svc := newSvc(t, st, dns, nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrent; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = svc.Verify(context.Background(), "t", "d") }()
	}
	for len(svc.slots) < maxConcurrent {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := svc.Verify(ctx, "t", "d"); !errors.Is(err, ErrBusy) {
		t.Fatalf("saturated: %v", err)
	}
	close(release)
	wg.Wait()
}
