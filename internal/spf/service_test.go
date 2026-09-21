package spf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// v4 is a public address (never contacted) so NewService accepts it.
var v4 = netip.MustParseAddr("8.8.8.8")

func directCfg(t *testing.T, list string) Config {
	t.Helper()
	// Test addresses come from documentation ranges, which production parsing
	// correctly rejects; build the Config directly like a test of Analyze should.
	var ips []netip.Addr
	for _, s := range strings.Split(list, ",") {
		ips = append(ips, netip.MustParseAddr(strings.TrimSpace(s)))
	}
	return Config{Mode: ModeDirect, IPs: ips}
}

func relayCfg() Config { return Config{Mode: ModeRelay, RelayInclude: "_spf.relay.example"} }

func notFound() error { return &net.DNSError{Err: "no such host", IsNotFound: true} }

func TestAnalyzeMatrix(t *testing.T) {
	d4 := directCfg(t, "192.0.2.10")
	d6 := directCfg(t, "2001:db8::10")
	both := directCfg(t, "192.0.2.10,2001:db8::10")
	tests := []struct {
		name    string
		cfg     Config
		records []string
		err     error
		status  Status
		reason  string
		action  string
		value   string // exact expected record when non-empty
	}{
		{"A ipv4 authorized", d4, []string{"v=spf1 ip4:192.0.2.10 -all"}, nil, StatusVerified, "", "none", ""},
		{"B ipv6 authorized", d6, []string{"v=spf1 ip6:2001:db8::/64 -all"}, nil, StatusVerified, "", "none", ""},
		{"both families need both", both, []string{"v=spf1 ip4:192.0.2.10 -all"}, nil, StatusMismatch, ReasonNotListed, "update_existing",
			"v=spf1 ip6:2001:db8::10 ip4:192.0.2.10 -all"},
		{"C missing", d4, []string{"google-site-verification=abc", "MS=ms123"}, nil, StatusNotConfigured, "", "create",
			"v=spf1 ip4:192.0.2.10 ~all"},
		{"C nxdomain/nodata", d4, nil, notFound(), StatusNotConfigured, "", "create", "v=spf1 ip4:192.0.2.10 ~all"},
		{"D existing correct case-insens", d4, []string{"V=SPF1 IP4:192.0.2.0/24 ~ALL"}, nil, StatusVerified, "", "none", ""},
		{"E unrelated record", d4, []string{"v=spf1 ip4:203.0.113.5 -all"}, nil, StatusMismatch, ReasonNotListed, "update_existing",
			"v=spf1 ip4:192.0.2.10 ip4:203.0.113.5 -all"},
		{"E denied explicitly", d4, []string{"v=spf1 -ip4:192.0.2.10 ip4:192.0.2.10"}, nil, StatusMismatch, ReasonExplicitlyDeny, "update_existing",
			"v=spf1 ip4:192.0.2.10 -ip4:192.0.2.10 ip4:192.0.2.10"},
		{"E softfail listing is not authorization", d4, []string{"v=spf1 ~ip4:192.0.2.10 -all"}, nil, StatusMismatch, ReasonExplicitlyDeny, "update_existing", ""},
		{"E +all is not verification", d4, []string{"v=spf1 +all"}, nil, StatusMismatch, ReasonNotListed, "update_existing", "v=spf1 ip4:192.0.2.10 +all"},
		{"F multiple records", d4, []string{"v=spf1 ip4:192.0.2.10 -all", "v=spf1 include:_spf.google.com ~all"}, nil, StatusConflict, ReasonMultipleRecords, "merge_records", ""},
		{"G malformed", d4, []string{"v=spf1 ip4:192.0.2.10/99 -all"}, nil, StatusInvalid, ReasonBadCIDR, "fix_record", ""},
		{"G unknown mechanism", d4, []string{"v=spf1 frobnicate -all"}, nil, StatusInvalid, ReasonUnknownMechanism, "fix_record", ""},
		{"H servfail is temporary", d4, nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}, StatusTemporaryError, ReasonDNS, "", ""},
		{"I timeout is bounded/temporary", d4, nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true, IsTemporary: true}, StatusTemporaryError, ReasonDNS, "", ""},
		{"I context deadline", d4, nil, context.DeadlineExceeded, StatusTemporaryError, ReasonDNS, "", ""},
		{"unknown resolver error is temporary", d4, nil, errors.New("boom"), StatusTemporaryError, ReasonDNS, "", ""},
		{"J provider mechanisms: one record extended", d4, []string{"v=spf1 include:_spf.google.com include:mailgun.org ~all"}, nil, StatusMismatch, ReasonNotListed, "update_existing",
			"v=spf1 ip4:192.0.2.10 include:_spf.google.com include:mailgun.org ~all"},
		{"N relay: include present", relayCfg(), []string{"v=spf1 include:_spf.relay.example -all"}, nil, StatusVerified, "", "none", ""},
		{"N relay: include missing extends existing", relayCfg(), []string{"v=spf1 include:_spf.google.com ~all"}, nil, StatusMismatch, ReasonIncludeMissing, "update_existing",
			"v=spf1 include:_spf.relay.example include:_spf.google.com ~all"},
		{"N relay: missing record", relayCfg(), nil, notFound(), StatusNotConfigured, "", "create", "v=spf1 include:_spf.relay.example ~all"},
		{"N relay ignores a literal ip", relayCfg(), []string{"v=spf1 ip4:192.0.2.10 -all"}, nil, StatusMismatch, ReasonIncludeMissing, "update_existing", ""},
		{"M direct ignores relay include", d4, []string{"v=spf1 include:_spf.relay.example -all"}, nil, StatusMismatch, ReasonNotListed, "update_existing", ""},
		{"unknown sending infra direct", Config{Mode: ModeDirect}, []string{"v=spf1 -all"}, nil, StatusSendingUnknown, "", "declare_sending_ips", ""},
		{"unknown sending infra relay", Config{Mode: ModeRelay}, nil, nil, StatusSendingUnknown, "", "follow_relay_provider", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Analyze(tc.cfg, "example.org", tc.records, tc.err)
			if res.Status != tc.status || res.Reason != tc.reason {
				t.Fatalf("status/reason = %s/%s want %s/%s", res.Status, res.Reason, tc.status, tc.reason)
			}
			if res.Expected.Action != tc.action {
				t.Fatalf("action = %q want %q", res.Expected.Action, tc.action)
			}
			if tc.value != "" && res.Expected.Value != tc.value {
				t.Fatalf("value = %q want %q", res.Expected.Value, tc.value)
			}
			// Never recommend two records: any generated value is exactly one v=spf1.
			if n := strings.Count(strings.ToLower(res.Expected.Value), "v=spf1"); n > 1 {
				t.Fatalf("recommended %d SPF records", n)
			}
			if res.Expected.Value != "" && res.Expected.Name != "example.org" {
				t.Fatalf("record name = %q", res.Expected.Name)
			}
		})
	}
}

func TestAnalyzeWarningsAndBounds(t *testing.T) {
	d4 := directCfg(t, "192.0.2.10")
	res := Analyze(d4, "example.org", []string{"v=spf1 include:a.example include:b.example mx a ptr exists:x.example include:c.example redirect=d.example"}, nil)
	for _, w := range []string{WarnUnevaluated, WarnLookupLimit, WarnDeprecatedPTR} {
		if !contains(res.Warnings, w) {
			t.Errorf("missing warning %s in %v", w, res.Warnings)
		}
	}
	if !res.Unevaluated || res.Status != StatusMismatch {
		t.Fatalf("unevaluated include must not verify: %+v", res)
	}
	if r := Analyze(d4, "example.org", []string{"v=spf1 +all"}, nil); !contains(r.Warnings, WarnPermitsAll) {
		t.Fatal("+all must warn")
	}
	many := make([]string, MaxTXTRecords+1)
	for i := range many {
		many[i] = fmt.Sprintf("noise-%d", i)
	}
	if r := Analyze(d4, "example.org", many, nil); r.Status != StatusInvalid || r.Reason != ReasonTooManyTXT {
		t.Fatalf("too many TXT records: %+v", r)
	}
	huge := "v=spf1 " + strings.Repeat("ip4:1.2.3.4 ", 500)
	if r := Analyze(d4, "example.org", []string{huge}, nil); r.Status != StatusInvalid || r.Reason != ReasonTooLong {
		t.Fatalf("oversized record: %+v", r)
	}
	// Published record text never appears in Reason/Warnings.
	r := Analyze(d4, "example.org", []string{"v=spf1 include:secret-tenant-host.example -all"}, nil)
	if strings.Contains(r.Reason+strings.Join(r.Warnings, ","), "secret-tenant-host") {
		t.Fatal("record text leaked into bounded fields")
	}
	// A record so full that merging exceeds the bound is refused, not truncated.
	near := "v=spf1 " + strings.Repeat("include:"+strings.Repeat("a", 25)+" ", 60)
	if r := Analyze(d4, "example.org", []string{near}, nil); r.Reason != ReasonMergeTooLong || r.Expected.Value != "" {
		t.Fatalf("merge overflow: %+v", r)
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

func TestSPFDoesNotMisinterpretLookalikes(t *testing.T) {
	d4 := directCfg(t, "192.0.2.10")
	for _, txt := range []string{"v=spf10 ip4:192.0.2.10", "v=spf1ip4:192.0.2.10", "spf2.0/pra ip4:192.0.2.10", "x v=spf1 ip4:192.0.2.10"} {
		if r := Analyze(d4, "example.org", []string{txt}, nil); r.Status != StatusNotConfigured {
			t.Errorf("%q treated as SPF: %s", txt, r.Status)
		}
	}
}

func TestParseSendingIPs(t *testing.T) {
	ok, err := ParseSendingIPs(" 8.8.8.8 , 2606:4700:4700::1111,8.8.8.8, ::ffff:1.1.1.1 ")
	if err != nil || len(ok) != 3 {
		t.Fatalf("ips=%v err=%v", ok, err)
	}
	if ips, err := ParseSendingIPs(""); err != nil || ips != nil {
		t.Fatalf("empty must mean undeclared: %v %v", ips, err)
	}
	bad := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.1.1", "100.64.0.1", "0.0.0.0", "::",
		"224.0.0.1", "255.255.255.255", "192.0.2.1", "198.51.100.2", "203.0.113.4", "2001:db8::1", "fc00::1", "fe80::1",
		"ff02::1", "8.8.8.8/32", "not-an-ip", "8.8.8.8,", "1.2.3", "8.8.8.8%eth0", "240.0.0.1", "198.18.0.1",
	}
	for _, b := range bad {
		if _, err := ParseSendingIPs(b); err == nil {
			t.Errorf("%q must be rejected", b)
		}
	}
	tooMany := strings.Repeat("8.8.8.8,", MaxSendingIPs) + "8.8.4.4"
	if _, err := ParseSendingIPs(tooMany); err == nil {
		t.Fatal("more than the maximum must be rejected")
	}
	if _, err := NewService(&fakeStore{}, &fakeDNS{}, Config{Mode: ModeDirect, IPs: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}, nil); err == nil {
		t.Fatal("a private address must be refused by the service constructor")
	}
}

// --- Service behaviour -------------------------------------------------------

type fakeStore struct {
	mu      sync.Mutex
	domains map[string]database.Domain // key tenant|id
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
	fn    func(ctx context.Context, name string) ([]string, error)
}

func (d *fakeDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	d.calls.Add(1)
	if d.fn == nil {
		return nil, notFound()
	}
	return d.fn(ctx, name)
}

type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) SPFResult(mode, outcome string) {
	r.mu.Lock()
	r.got = append(r.got, mode+"/"+outcome)
	r.mu.Unlock()
}

func newSvc(t *testing.T, cfg Config, st *fakeStore, dns *fakeDNS, obs Observer) *Service {
	t.Helper()
	s, err := NewService(st, dns, cfg, obs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServiceOwnershipAndTenantIsolation(t *testing.T) {
	st, dns := &fakeStore{}, &fakeDNS{}
	st.add("tenantA", "dom-a", "a.example", true)
	st.add("tenantA", "dom-pending", "pending.example", false)
	svc := newSvc(t, Config{Mode: ModeDirect, IPs: []netip.Addr{v4}}, st, dns, nil)
	ctx := context.Background()

	if _, err := svc.Verify(ctx, "tenantB", "dom-a"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant verify: %v", err)
	}
	if _, err := svc.Describe(ctx, "tenantB", "dom-a"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant describe: %v", err)
	}
	if _, err := svc.Verify(ctx, "tenantA", "dom-pending"); !errors.Is(err, ErrDomainNotVerified) {
		t.Fatalf("unverified domain must not be probed: %v", err)
	}
	if dns.calls.Load() != 0 {
		t.Fatal("no DNS query may happen for another tenant's or an unverified domain")
	}
	if _, err := svc.Describe(ctx, "tenantA", "dom-pending"); err != nil {
		t.Fatalf("describe needs ownership of the record, not verification: %v", err)
	}
}

func TestServiceUnknownInfraMakesNoDNSQuery(t *testing.T) {
	st, dns := &fakeStore{}, &fakeDNS{}
	st.add("t", "d", "a.example", true)
	obs := &recorder{}
	svc := newSvc(t, Config{Mode: ModeDirect}, st, dns, obs)
	res, err := svc.Verify(context.Background(), "t", "d")
	if err != nil || res.Status != StatusSendingUnknown || dns.calls.Load() != 0 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, dns.calls.Load())
	}
	if len(obs.got) != 1 || obs.got[0] != "direct/sending_infrastructure_unknown" {
		t.Fatalf("observer: %v", obs.got)
	}
}

func TestServiceVerifyDoesExactlyOneLookupAndObserves(t *testing.T) {
	st := &fakeStore{}
	st.add("t", "d", "a.example", true)
	dns := &fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		if name != "a.example" {
			t.Errorf("looked up %q; SPF identity is the domain itself", name)
		}
		return []string{"v=spf1 include:_spf.google.com ~all"}, nil
	}}
	obs := &recorder{}
	svc := newSvc(t, Config{Mode: ModeDirect, IPs: []netip.Addr{v4}}, st, dns, obs)
	res, err := svc.Verify(context.Background(), "t", "d")
	if err != nil || res.Status != StatusMismatch {
		t.Fatalf("%+v %v", res, err)
	}
	if dns.calls.Load() != 1 {
		t.Fatalf("include/redirect must not be followed: %d lookups", dns.calls.Load())
	}
	if len(obs.got) != 1 || obs.got[0] != "direct/mismatch" {
		t.Fatalf("observer: %v", obs.got)
	}
}

func TestServiceFailureInjection(t *testing.T) {
	st := &fakeStore{}
	st.add("t", "d", "a.example", true)
	cfg := Config{Mode: ModeDirect, IPs: []netip.Addr{v4}}

	// A hanging resolver is cut off by the service timeout, and the goroutine ends.
	before := runtime.NumGoroutine()
	hang := &fakeDNS{fn: func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() }}
	svc := newSvc(t, cfg, st, hang, nil)
	svc.timeout = 50 * time.Millisecond
	start := time.Now()
	res, err := svc.Verify(context.Background(), "t", "d")
	if err != nil || res.Status != StatusTemporaryError {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("verification not bounded by its timeout")
	}

	// Caller cancellation is reported as such, never as a domain finding.
	ctx, cancel := context.WithCancel(context.Background())
	blocked := &fakeDNS{fn: func(ctx context.Context, _ string) ([]string, error) { cancel(); <-ctx.Done(); return nil, ctx.Err() }}
	if _, err := newSvc(t, cfg, st, blocked, nil).Verify(ctx, "t", "d"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}

	// SERVFAIL, malformed, oversized, multiple: MailX stays healthy and classifies.
	for name, tc := range map[string]struct {
		recs []string
		err  error
		want Status
	}{
		"servfail":  {nil, &net.DNSError{Err: "server failure", IsTemporary: true}, StatusTemporaryError},
		"malformed": {[]string{"v=spf1 ip4:not-an-ip"}, nil, StatusInvalid},
		"oversized": {[]string{"v=spf1 " + strings.Repeat("a", 100000)}, nil, StatusInvalid},
		"multiple":  {[]string{"v=spf1 -all", "V=SPF1 ~all"}, nil, StatusConflict},
	} {
		d := &fakeDNS{fn: func(context.Context, string) ([]string, error) { return tc.recs, tc.err }}
		if r, err := newSvc(t, cfg, st, d, nil).Verify(context.Background(), "t", "d"); err != nil || r.Status != tc.want {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}

	// Store failure surfaces as an error, not as a status.
	st.err = errors.New("db down")
	if _, err := newSvc(t, cfg, st, &fakeDNS{}, nil).Verify(context.Background(), "t", "d"); err == nil {
		t.Fatal("store failure must be an error")
	}
	st.err = nil

	// A panicking observer cannot break verification.
	svc = newSvc(t, cfg, st, &fakeDNS{}, panicObserver{})
	if _, err := svc.Verify(context.Background(), "t", "d"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	if leaked := runtime.NumGoroutine() - before; leaked > 2 {
		t.Fatalf("goroutine leak: %d", leaked)
	}
}

type panicObserver struct{}

func (panicObserver) SPFResult(string, string) { panic("observer bug") }

func TestServiceConcurrencyIsolatedAndBounded(t *testing.T) {
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
		time.Sleep(2 * time.Millisecond)
		switch {
		case strings.HasPrefix(name, "good"):
			return []string{"v=spf1 ip4:8.8.8.8 -all"}, nil
		case strings.HasPrefix(name, "missing"):
			return nil, notFound()
		case strings.HasPrefix(name, "bad"):
			return []string{"v=spf1 ip4:oops"}, nil
		case strings.HasPrefix(name, "slow"):
			return nil, &net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}
		}
		return []string{"v=spf1 ip4:8.8.8.8 -all", "v=spf1 -all"}, nil
	}}
	want := map[string]Status{}
	for tn := 0; tn < 8; tn++ {
		for i, kind := range []string{"good", "missing", "bad", "slow", "dup"} {
			tenant, id := fmt.Sprintf("t%d", tn), fmt.Sprintf("d%d", i)
			st.add(tenant, id, fmt.Sprintf("%s-%d.example", kind, tn), true)
			want[tenant+"|"+id] = map[string]Status{"good": StatusVerified, "missing": StatusNotConfigured, "bad": StatusInvalid,
				"slow": StatusTemporaryError, "dup": StatusConflict}[kind]
		}
	}
	obs := &recorder{}
	svc := newSvc(t, Config{Mode: ModeDirect, IPs: []netip.Addr{v4}}, st, dns, obs)
	var wg sync.WaitGroup
	for rep := 0; rep < 6; rep++ {
		for key, status := range want {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tenant, id, _ := strings.Cut(key, "|")
				res, err := svc.Verify(context.Background(), tenant, id)
				if err != nil || res.Status != status {
					t.Errorf("%s: %+v %v (one broken domain must not affect others)", key, res, err)
				}
			}()
		}
	}
	wg.Wait()
	if peak.Load() > maxConcurrentDNS {
		t.Fatalf("DNS concurrency %d exceeded bound %d", peak.Load(), maxConcurrentDNS)
	}
	if len(obs.got) != len(want)*6 {
		t.Fatalf("observed %d of %d", len(obs.got), len(want)*6)
	}
}

func TestServiceBusyWhenSaturatedAndContextEnds(t *testing.T) {
	st := &fakeStore{}
	st.add("t", "d", "a.example", true)
	release := make(chan struct{})
	dns := &fakeDNS{fn: func(ctx context.Context, _ string) ([]string, error) { <-release; return nil, notFound() }}
	svc := newSvc(t, Config{Mode: ModeDirect, IPs: []netip.Addr{v4}}, st, dns, nil)
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentDNS; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = svc.Verify(context.Background(), "t", "d") }()
	}
	for len(svc.slots) < maxConcurrentDNS {
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
