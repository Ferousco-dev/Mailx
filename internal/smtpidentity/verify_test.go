package smtpidentity

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
)

const host = "smtp.example.com"

var (
	ip4a = netip.MustParseAddr("8.8.8.8")
	ip4b = netip.MustParseAddr("8.8.4.4")
	ip6a = netip.MustParseAddr("2606:4700:4700::1111")
)

type fakeDNS struct {
	mu      sync.Mutex
	ptr     map[string][]string
	ptrErr  map[string]error
	fwd     map[string][]netip.Addr
	fwdErr  map[string]error
	txt     map[string][]string
	txtErr  error
	calls   atomic.Int64
	ptrHook func(ctx context.Context, addr string) ([]string, error)
}

func (f *fakeDNS) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	f.calls.Add(1)
	if f.ptrHook != nil {
		return f.ptrHook(ctx, addr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ptrErr[addr]; err != nil {
		return nil, err
	}
	if n, ok := f.ptr[addr]; ok {
		return n, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fakeDNS) LookupNetIP(_ context.Context, _, h string) ([]netip.Addr, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fwdErr[h]; err != nil {
		return nil, err
	}
	if a, ok := f.fwd[h]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.txtErr != nil {
		return nil, f.txtErr
	}
	if r, ok := f.txt[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (f *fakeDNS) set(ip netip.Addr, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ptr == nil {
		f.ptr = map[string][]string{}
	}
	f.ptr[ip.String()] = names
}

func newFake() *fakeDNS {
	return &fakeDNS{ptrErr: map[string]error{}, fwd: map[string][]netip.Addr{}, fwdErr: map[string]error{}, txt: map[string][]string{}}
}

func check(t *testing.T, f *fakeDNS, ips ...netip.Addr) Report {
	t.Helper()
	rep, err := (&Checker{Resolver: f}).Check(context.Background(), host, ips)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func servfail() error { return &net.DNSError{Err: "server misbehaving", IsTemporary: true} }

func TestPTRMatrix(t *testing.T) {
	t.Run("A ready", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host+".")
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a); r.Readiness != ReadinessReady || r.IPs[0].State != IPReady || r.IPs[0].Family != "ipv4" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("B missing PTR", func(t *testing.T) {
		f := newFake()
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a); r.Readiness != ReadinessNotReady || r.IPs[0].State != IPMissingPTR {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("B2 empty PTR answer", func(t *testing.T) {
		f := newFake()
		f.set(ip4a)
		if r := check(t, f, ip4a); r.IPs[0].State != IPMissingPTR {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("C PTR names a different host", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, "static-8-8-8-8.provider.net.")
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a); r.Readiness != ReadinessNotReady || r.IPs[0].State != IPPTRMismatch {
			t.Fatalf("EHLO and PTR unrelated must never be ready: %+v", r)
		}
	})
	t.Run("D forward mismatch", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host)
		f.fwd[host] = []netip.Addr{ip4b}
		if r := check(t, f, ip4a); r.IPs[0].State != IPForwardMismatch || r.Readiness != ReadinessNotReady {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("D2 hostname has no address at all", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host)
		if r := check(t, f, ip4a); r.IPs[0].State != IPForwardMismatch {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("E multiple addresses including the IP is membership", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host)
		f.fwd[host] = []netip.Addr{ip4b, netip.MustParseAddr("9.9.9.9"), ip4a, ip6a}
		if r := check(t, f, ip4a); r.IPs[0].State != IPReady {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("F multiple PTR names: membership with a warning", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, "other.provider.net.", host+".", "third.example.org.")
		f.fwd[host] = []netip.Addr{ip4a}
		r := check(t, f, ip4a)
		if r.IPs[0].State != IPReady || !contains(r.IPs[0].Warnings, WarnMultiplePTR) || len(r.IPs[0].PTRNames) != 3 {
			t.Fatalf("%+v", r.IPs[0])
		}
		f.set(ip4a, "other.provider.net.", "third.example.org.") // none is ours: mismatch, no arbitrary pick
		if r = check(t, f, ip4a); r.IPs[0].State != IPPTRMismatch {
			t.Fatalf("%+v", r.IPs[0])
		}
	})
	t.Run("G malformed PTR", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, "bad name.example.com.", "-x.example.com", "nodots", "a..b", strings.Repeat("a", 300)+".com", "\x00.com", "under_score.example.com")
		if r := check(t, f, ip4a); r.IPs[0].State != IPMalformedPTR || len(r.IPs[0].PTRNames) != 0 {
			t.Fatalf("%+v", r.IPs[0])
		}
		f.set(ip4a, "garbage name", host)
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a); r.IPs[0].State != IPReady || !contains(r.IPs[0].Warnings, WarnMalformedIgnored) {
			t.Fatalf("a valid PTR beside garbage still counts, with a warning: %+v", r.IPs[0])
		}
	})
	t.Run("H SERVFAIL is temporary, never a verdict", func(t *testing.T) {
		f := newFake()
		f.ptrErr[ip4a.String()] = servfail()
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a); r.IPs[0].State != IPTemporary || r.Readiness != ReadinessUnknown {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("H2 forward SERVFAIL after a matching PTR is temporary", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host)
		f.fwdErr[host] = servfail()
		if r := check(t, f, ip4a); r.IPs[0].State != IPTemporary || r.Readiness != ReadinessUnknown {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("I timeout", func(t *testing.T) {
		f := newFake()
		f.ptrErr[ip4a.String()] = &net.DNSError{Err: "i/o timeout", IsTimeout: true, IsTemporary: true}
		if r := check(t, f, ip4a); r.IPs[0].State != IPTemporary {
			t.Fatalf("%+v", r)
		}
		f.ptrErr[ip4a.String()] = context.DeadlineExceeded
		if r := check(t, f, ip4a); r.IPs[0].State != IPTemporary {
			t.Fatalf("%+v", r)
		}
		f.ptrErr[ip4a.String()] = errors.New("boom")
		if r := check(t, f, ip4a); r.IPs[0].State != IPTemporary {
			t.Fatalf("unknown resolver errors are temporary too: %+v", r)
		}
	})
	t.Run("K IPv6 PTR and AAAA confirmation", func(t *testing.T) {
		f := newFake()
		f.set(ip6a, host)
		f.fwd[host] = []netip.Addr{ip6a}
		if r := check(t, f, ip6a); r.IPs[0].State != IPReady || r.IPs[0].Family != "ipv6" {
			t.Fatalf("%+v", r)
		}
		f.fwd[host] = []netip.Addr{ip4a} // A only: the IPv6 address is not confirmed
		if r := check(t, f, ip6a); r.IPs[0].State != IPForwardMismatch {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("L mixed families", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host)
		f.set(ip6a, host)
		f.fwd[host] = []netip.Addr{ip4a, ip6a}
		if r := check(t, f, ip6a, ip4a); r.Readiness != ReadinessReady || len(r.IPs) != 2 || r.IPs[0].Family != "ipv4" {
			t.Fatalf("deterministic order, both ready: %+v", r)
		}
	})
	t.Run("IPv4-mapped IPv6 forward answers match the IPv4", func(t *testing.T) {
		f := newFake()
		f.set(ip4a, host)
		f.fwd[host] = []netip.Addr{netip.MustParseAddr("::ffff:8.8.8.8")}
		if r := check(t, f, ip4a); r.IPs[0].State != IPReady {
			t.Fatalf("%+v", r)
		}
	})
}

func TestMultipleIPReadiness(t *testing.T) {
	healthy := func(f *fakeDNS, ip netip.Addr) { f.set(ip, host) }
	t.Run("all healthy", func(t *testing.T) {
		f := newFake()
		healthy(f, ip4a)
		healthy(f, ip4b)
		f.fwd[host] = []netip.Addr{ip4a, ip4b}
		if r := check(t, f, ip4a, ip4b); r.Readiness != ReadinessReady {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("one healthy one missing PTR is not ready", func(t *testing.T) {
		f := newFake()
		healthy(f, ip4a)
		f.fwd[host] = []netip.Addr{ip4a, ip4b}
		r := check(t, f, ip4a, ip4b)
		if r.Readiness != ReadinessNotReady || r.IPs[0].State == r.IPs[1].State {
			t.Fatalf("a healthy IP must not hide a broken one: %+v", r)
		}
	})
	t.Run("one healthy one temporary error is unknown", func(t *testing.T) {
		f := newFake()
		healthy(f, ip4a)
		f.fwd[host] = []netip.Addr{ip4a, ip4b}
		f.ptrErr[ip4b.String()] = servfail()
		if r := check(t, f, ip4a, ip4b); r.Readiness != ReadinessUnknown {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a definite failure beats a temporary one", func(t *testing.T) {
		f := newFake()
		f.ptrErr[ip4a.String()] = servfail()
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a, ip4b); r.Readiness != ReadinessNotReady { // ip4b has no PTR
			t.Fatalf("%+v", r)
		}
	})
	t.Run("IPv4 healthy IPv6 unhealthy", func(t *testing.T) {
		f := newFake()
		healthy(f, ip4a)
		f.fwd[host] = []netip.Addr{ip4a}
		r := check(t, f, ip4a, ip6a)
		if r.Readiness != ReadinessNotReady || r.IPs[0].State != IPReady || r.IPs[1].State != IPMissingPTR {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("duplicates collapse", func(t *testing.T) {
		f := newFake()
		healthy(f, ip4a)
		f.fwd[host] = []netip.Addr{ip4a}
		if r := check(t, f, ip4a, ip4a, netip.MustParseAddr("::ffff:8.8.8.8")); len(r.IPs) != 1 || r.Readiness != ReadinessReady {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("hostname resolves to configured IPs plus unrelated addresses", func(t *testing.T) {
		f := newFake()
		healthy(f, ip4a)
		f.fwd[host] = []netip.Addr{netip.MustParseAddr("1.1.1.1"), ip4a, netip.MustParseAddr("9.9.9.9")}
		if r := check(t, f, ip4a); r.Readiness != ReadinessReady {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("too many configured IPs is refused before any DNS", func(t *testing.T) {
		f := newFake()
		var ips []netip.Addr
		for i := 0; i <= MaxIPs; i++ {
			ips = append(ips, netip.AddrFrom4([4]byte{8, 8, byte(i), 8}))
		}
		if _, err := (&Checker{Resolver: f}).Check(context.Background(), host, ips); !errors.Is(err, ErrTooManyIPs) || f.calls.Load() != 0 {
			t.Fatalf("err=%v calls=%d", err, f.calls.Load())
		}
	})
	t.Run("not configured", func(t *testing.T) {
		f := newFake()
		for _, tc := range []struct {
			h   string
			ips []netip.Addr
		}{{"", []netip.Addr{ip4a}}, {host, nil}} {
			rep, err := (&Checker{Resolver: f}).Check(context.Background(), tc.h, tc.ips)
			if err != nil || rep.Readiness != ReadinessNotConfigured || f.calls.Load() != 0 {
				t.Fatalf("%+v %v calls=%d", rep, err, f.calls.Load())
			}
		}
	})
}

func TestBoundsAndHostileAnswers(t *testing.T) {
	f := newFake()
	names := make([]string, MaxPTRNames+1)
	for i := range names {
		names[i] = fmt.Sprintf("h%d.example.com.", i)
	}
	f.set(ip4a, names...)
	f.fwd[host] = []netip.Addr{ip4a}
	if r := check(t, f, ip4a); r.IPs[0].State != IPOversized || r.Readiness != ReadinessNotReady {
		t.Fatalf("too many PTR values must fail safe: %+v", r.IPs[0])
	}
	// A huge forward answer is truncated and, if the IP is beyond the bound, never "ready".
	f = newFake()
	f.set(ip4a, host)
	var many []netip.Addr
	for i := 0; i < MaxForwardAddresses+50; i++ {
		many = append(many, netip.AddrFrom4([4]byte{10, 0, byte(i / 250), byte(i%250 + 1)}))
	}
	f.fwd[host] = append(many, ip4a)
	if r := check(t, f, ip4a); r.IPs[0].State == IPReady {
		t.Fatalf("an address beyond the bound must not be reported as confirmed: %+v", r.IPs[0])
	}
	// Hostile PTR names never appear in a report.
	f = newFake()
	f.set(ip4a, "evil\r\nX-Injected: 1.example.com", host)
	f.fwd[host] = []netip.Addr{ip4a}
	r := check(t, f, ip4a)
	for _, n := range r.IPs[0].PTRNames {
		if strings.ContainsAny(n, "\r\n ") {
			t.Fatalf("hostile PTR leaked: %q", n)
		}
	}
}

func TestFailureInjection(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newFake()
	f.set(ip4a, host)
	f.fwd[host] = []netip.Addr{ip4a}
	// Hanging reverse lookups are cut off by the checker timeout.
	f.ptrHook = func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() }
	start := time.Now()
	rep, err := (&Checker{Resolver: f, Timeout: 50 * time.Millisecond}).Check(context.Background(), host, []netip.Addr{ip4a, ip4b, ip6a})
	if err != nil || time.Since(start) > 2*time.Second || rep.Readiness != ReadinessUnknown {
		t.Fatalf("%v %v %+v", err, time.Since(start), rep)
	}
	// Cancellation: safe termination, nothing concluded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.ptrHook = nil
	rep, err = (&Checker{Resolver: f}).Check(ctx, host, []netip.Addr{ip4a})
	if err != nil || rep.Readiness == ReadinessReady {
		t.Fatalf("a canceled check must not report ready: %v %+v", err, rep)
	}
	time.Sleep(50 * time.Millisecond)
	if leaked := runtime.NumGoroutine() - before; leaked > 2 {
		t.Fatalf("goroutine leak: %d", leaked)
	}
}

func TestConcurrentChecksAreBoundedAndIsolated(t *testing.T) {
	var inflight, peak atomic.Int64
	f := newFake()
	var ips []netip.Addr
	for i := 1; i <= MaxIPs; i++ {
		ip := netip.AddrFrom4([4]byte{8, 8, byte(i), 8})
		ips = append(ips, ip)
		f.ptr = map[string][]string{}
	}
	f.ptrHook = func(_ context.Context, addr string) ([]string, error) {
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
		case strings.HasSuffix(addr, ".1.8"), strings.HasSuffix(addr, ".5.8"):
			return []string{host + "."}, nil
		case strings.HasSuffix(addr, ".2.8"):
			return nil, servfail()
		case strings.HasSuffix(addr, ".3.8"):
			return []string{"other.example.net."}, nil
		}
		return nil, &net.DNSError{IsNotFound: true}
	}
	f.fwd[host] = ips
	var wg sync.WaitGroup
	var first Report
	var mu sync.Mutex
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := (&Checker{Resolver: f}).Check(context.Background(), host, ips)
			if err != nil || r.Readiness != ReadinessNotReady {
				t.Errorf("%v %+v", err, r.Readiness)
			}
			mu.Lock()
			defer mu.Unlock()
			if first.IPs == nil {
				first = r
			} else if fmt.Sprint(first.IPs) != fmt.Sprint(r.IPs) {
				t.Error("aggregation is not deterministic")
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 12*maxConcurrentPTR {
		t.Fatalf("per-check concurrency bound exceeded: %d", peak.Load())
	}
}

func TestLocalInterfaceFactIsInformationalOnly(t *testing.T) {
	f := newFake()
	f.set(ip4a, host)
	f.fwd[host] = []netip.Addr{ip4a}
	rep, _ := (&Checker{Resolver: f, LocalAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("10.0.0.5")} }}).Check(context.Background(), host, []netip.Addr{ip4a})
	if rep.Readiness != ReadinessReady || rep.IPs[0].LocalInterface || !contains(rep.IPs[0].Warnings, WarnNoLocalInterface) {
		t.Fatalf("NAT hosts are normal: DNS-ready stays ready, with a note: %+v", rep)
	}
	rep, _ = (&Checker{Resolver: f, LocalAddrs: func() []netip.Addr { return []netip.Addr{ip4a} }}).Check(context.Background(), host, []netip.Addr{ip4a})
	if !rep.IPs[0].LocalInterface || contains(rep.IPs[0].Warnings, WarnNoLocalInterface) {
		t.Fatalf("%+v", rep)
	}
}

func TestHeloSPFIsAdvisory(t *testing.T) {
	f := newFake()
	f.set(ip4a, host)
	f.fwd[host] = []netip.Addr{ip4a}
	r := check(t, f, ip4a)
	if r.Readiness != ReadinessReady || r.HeloSPF.Status != "not_configured" || !contains(r.Warnings, WarnHeloSPFNotReady) || r.HeloSPF.Recommended != "v=spf1 ip4:8.8.8.8 -all" {
		t.Fatalf("missing HELO SPF is advice, not a readiness failure: %+v", r)
	}
	f.txt[host] = []string{"v=spf1 ip4:8.8.8.8 -all"}
	if r = check(t, f, ip4a); r.HeloSPF.Status != "verified" || contains(r.Warnings, WarnHeloSPFNotReady) {
		t.Fatalf("%+v", r)
	}
	f.txt[host] = []string{"v=spf1 ip4:8.8.8.8 -all", "v=spf1 -all"}
	if r = check(t, f, ip4a); r.HeloSPF.Status != "conflict" || r.Readiness != ReadinessReady {
		t.Fatalf("%+v", r)
	}
}

func FuzzNormalizePTR(f *testing.F) {
	for _, s := range []string{"smtp.example.com.", "", ".", "a..b", "\x00", strings.Repeat("a", 400), "UP.CASE.COM", "a.b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, ok := normalizePTR(s); ok && (got != strings.ToLower(got) || strings.ContainsAny(got, " \r\n\x00") || strings.Count(got, ".") < 1) {
			t.Fatalf("%q -> %q", s, got)
		}
	})
}

func FuzzAggregate(f *testing.F) {
	f.Add(uint8(0), uint8(1), uint8(6))
	f.Fuzz(func(t *testing.T, a, b, c uint8) {
		states := []IPState{IPReady, IPMissingPTR, IPPTRMismatch, IPForwardMismatch, IPMalformedPTR, IPOversized, IPTemporary}
		rs := []IPReport{{State: states[int(a)%7]}, {State: states[int(b)%7]}, {State: states[int(c)%7]}}
		got := aggregate(rs)
		allReady := rs[0].State == IPReady && rs[1].State == IPReady && rs[2].State == IPReady
		if (got == ReadinessReady) != allReady {
			t.Fatalf("%v -> %s", rs, got)
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
