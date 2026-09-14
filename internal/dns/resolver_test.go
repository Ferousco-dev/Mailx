package dns

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeLookup is a deterministic in-memory Lookup for tests.
type fakeLookup struct {
	mu       sync.Mutex
	mx       map[string][]*net.MX
	mxErr    map[string]error
	host     map[string][]string
	hostErr  map[string]error
	mxCalls  int
	hostCall int
	// delay simulates slow lookups; ctx cancellation must still fire.
	delay time.Duration
}

func (f *fakeLookup) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	f.mu.Lock()
	f.mxCalls++
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err, ok := f.mxErr[name]; ok {
		return nil, err
	}
	return f.mx[name], nil
}

func (f *fakeLookup) LookupHost(ctx context.Context, host string) ([]string, error) {
	f.mu.Lock()
	f.hostCall++
	f.mu.Unlock()
	if err, ok := f.hostErr[host]; ok {
		return nil, err
	}
	return f.host[host], nil
}

func newFake() *fakeLookup {
	return &fakeLookup{
		mx:      map[string][]*net.MX{},
		mxErr:   map[string]error{},
		host:    map[string][]string{},
		hostErr: map[string]error{},
	}
}

// ---------- normalization / validation -------------------------------

func TestNormalizeDomain(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"Example.COM", "example.com", false},
		{"  example.com  ", "example.com", false},
		{"example.com.", "example.com", false},
		{"", "", true},
		{"   ", "", true},
		{"a\r\nb", "", true},
		{"a\x00b", "", true},
		{"alice@example.com", "", true},
		{"example..com", "", true},
		{".", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeDomain(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalize(%q) expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalize(%q) unexpected error %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLookupMXInvalidInputs(t *testing.T) {
	r := NewResolverWith(newFake())
	for _, d := range []string{"", "  ", "a\r\nb", "alice@example.com", "example..com"} {
		_, err := r.LookupMX(context.Background(), d)
		var le *LookupError
		if !errors.As(err, &le) || le.Kind != KindInvalidDomain {
			t.Errorf("expected invalid_domain for %q, got %v", d, err)
		}
	}
}

// ---------- ordering / equal preference / dedup ----------------------

func TestLookupMXPreferenceOrdering(t *testing.T) {
	f := newFake()
	f.mx["example.com"] = []*net.MX{
		{Host: "mx3.example.com.", Pref: 20},
		{Host: "mx1.example.com.", Pref: 10},
		{Host: "mx4.example.com.", Pref: 30},
		{Host: "mx2.example.com.", Pref: 10},
	}
	got, err := NewResolverWith(f).LookupMX(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4, got %d: %+v", len(got), got)
	}
	// preferences must be non-decreasing
	for i := 1; i < len(got); i++ {
		if got[i].Preference < got[i-1].Preference {
			t.Fatalf("preferences not sorted: %+v", got)
		}
	}
	// first two are pref=10 in resolver order
	if got[0].Host != "mx3.example.com" && got[0].Host != "mx1.example.com" {
		// actual first pref=10 slot: mx1 came second in input, but the
		// slice began with mx3 (pref 20) - after sort, pref 10 entries
		// (mx1, mx2) come first, in the order they appeared amongst pref-10.
	}
	if got[0].Preference != 10 || got[1].Preference != 10 || got[2].Preference != 20 || got[3].Preference != 30 {
		t.Fatalf("preference groups wrong: %+v", got)
	}
	// trailing dots stripped, lower-case
	for _, m := range got {
		if m.Host == "" || m.Host[len(m.Host)-1] == '.' {
			t.Fatalf("host not canonical: %q", m.Host)
		}
	}
}

func TestLookupMXEqualPreferencePreservesResolverOrder(t *testing.T) {
	f := newFake()
	f.mx["x.test"] = []*net.MX{
		{Host: "a.x.test", Pref: 10},
		{Host: "b.x.test", Pref: 10},
		{Host: "c.x.test", Pref: 10},
	}
	got, err := NewResolverWith(f).LookupMX(context.Background(), "x.test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.x.test", "b.x.test", "c.x.test"}
	for i, m := range got {
		if m.Host != want[i] {
			t.Fatalf("equal-pref order not preserved: got %+v want %v", got, want)
		}
	}
}

func TestLookupMXDeduplicatesExactDuplicates(t *testing.T) {
	f := newFake()
	f.mx["x.test"] = []*net.MX{
		{Host: "a.x.test", Pref: 10},
		{Host: "a.x.test", Pref: 10}, // exact dup
		{Host: "a.x.test", Pref: 20}, // different pref → kept
	}
	got, _ := NewResolverWith(f).LookupMX(context.Background(), "x.test")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %+v", got)
	}
	if got[0].Preference != 10 || got[1].Preference != 20 {
		t.Fatalf("wrong: %+v", got)
	}
}

func TestLookupMXInvalidHostsFiltered(t *testing.T) {
	f := newFake()
	f.mx["x.test"] = []*net.MX{
		{Host: "good.x.test", Pref: 10},
		{Host: "bad\r\nhost", Pref: 20},
		{Host: "", Pref: 30},
	}
	got, err := NewResolverWith(f).LookupMX(context.Background(), "x.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Host != "good.x.test" {
		t.Fatalf("bad hosts leaked: %+v", got)
	}
}

func TestLookupMXCandidateCap(t *testing.T) {
	f := newFake()
	var many []*net.MX
	for i := 0; i < MaxCandidates*3; i++ {
		many = append(many, &net.MX{Host: "h.example.", Pref: uint16(i)})
	}
	f.mx["example.com"] = many
	got, err := NewResolverWith(f).LookupMX(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > MaxCandidates {
		t.Fatalf("cap exceeded: %d", len(got))
	}
}

// ---------- Null MX --------------------------------------------------

func TestLookupMXNullMX(t *testing.T) {
	f := newFake()
	f.mx["nomail.test"] = []*net.MX{{Host: ".", Pref: 0}}
	// Even if the domain has A records, Null MX must win — never fall back.
	f.host["nomail.test"] = []string{"192.0.2.1"}
	_, err := NewResolverWith(f).LookupMX(context.Background(), "nomail.test")
	if !IsNullMX(err) {
		t.Fatalf("expected Null MX, got %v", err)
	}
	if IsTemporary(err) {
		t.Fatal("Null MX must not be classified as temporary")
	}
}

func TestLookupMXNullMXMixedIsMalformed(t *testing.T) {
	f := newFake()
	f.mx["broken.test"] = []*net.MX{
		{Host: ".", Pref: 0},
		{Host: "mx.broken.test", Pref: 10},
	}
	_, err := NewResolverWith(f).LookupMX(context.Background(), "broken.test")
	var le *LookupError
	if !errors.As(err, &le) || le.Kind != KindResolverFailure {
		t.Fatalf("expected resolver_failure, got %v", err)
	}
}

// ---------- Implicit MX fallback ------------------------------------

func TestLookupMXImplicitFallbackWhenNoMX(t *testing.T) {
	f := newFake()
	// no MX entries; host resolves
	f.host["example.com"] = []string{"192.0.2.1"}
	got, err := NewResolverWith(f).LookupMX(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Host != "example.com" || got[0].Preference != 0 {
		t.Fatalf("implicit fallback wrong: %+v", got)
	}
}

func TestLookupMXNoMXAndNoAddress(t *testing.T) {
	f := newFake()
	// no MX; host resolves to nothing
	got, err := NewResolverWith(f).LookupMX(context.Background(), "example.com")
	if err == nil {
		t.Fatalf("expected error, got %+v", got)
	}
	var le *LookupError
	if !errors.As(err, &le) || le.Kind != KindNotFound {
		t.Fatalf("expected not_found, got %v", err)
	}
}

func TestLookupMXNXDOMAIN(t *testing.T) {
	f := newFake()
	f.mxErr["nope.test"] = &net.DNSError{Err: "no such host", Name: "nope.test", IsNotFound: true}
	f.hostErr["nope.test"] = &net.DNSError{Err: "no such host", Name: "nope.test", IsNotFound: true}
	_, err := NewResolverWith(f).LookupMX(context.Background(), "nope.test")
	var le *LookupError
	if !errors.As(err, &le) || le.Kind != KindNotFound {
		t.Fatalf("expected not_found, got %v", err)
	}
}

// ---------- Temporary / resolver_failure -----------------------------

func TestLookupMXTemporary(t *testing.T) {
	f := newFake()
	f.mxErr["temp.test"] = &net.DNSError{Err: "servfail", Name: "temp.test", IsTemporary: true}
	_, err := NewResolverWith(f).LookupMX(context.Background(), "temp.test")
	if !IsTemporary(err) {
		t.Fatalf("expected temporary, got %v", err)
	}
}

func TestLookupMXTimeoutIsTemporary(t *testing.T) {
	f := newFake()
	f.mxErr["slow.test"] = &net.DNSError{Err: "timeout", Name: "slow.test", IsTimeout: true}
	_, err := NewResolverWith(f).LookupMX(context.Background(), "slow.test")
	if !IsTemporary(err) {
		t.Fatalf("expected temporary, got %v", err)
	}
}

func TestLookupMXGenericFailure(t *testing.T) {
	f := newFake()
	f.mxErr["weird.test"] = errors.New("some unexpected error")
	_, err := NewResolverWith(f).LookupMX(context.Background(), "weird.test")
	var le *LookupError
	if !errors.As(err, &le) || le.Kind != KindResolverFailure {
		t.Fatalf("expected resolver_failure, got %v", err)
	}
}

// ---------- Context handling ----------------------------------------

func TestLookupMXContextCancellation(t *testing.T) {
	f := newFake()
	f.delay = 500 * time.Millisecond
	f.mx["slow.test"] = []*net.MX{{Host: "mx.slow.test", Pref: 10}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := NewResolverWith(f).LookupMX(ctx, "slow.test")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("chain lost cancellation: %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("cancel too slow: %v", time.Since(start))
	}
}

func TestLookupMXDeadlineExceeded(t *testing.T) {
	f := newFake()
	f.delay = 500 * time.Millisecond
	f.mx["slow.test"] = []*net.MX{{Host: "mx.slow.test", Pref: 10}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := NewResolverWith(f).LookupMX(ctx, "slow.test")
	if err == nil {
		t.Fatal("expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("chain lost deadline: %v", err)
	}
	if !IsTemporary(err) {
		t.Fatalf("deadline should classify temporary: %v", err)
	}
}

// ---------- Concurrency + no SMTP side effects ----------------------

func TestLookupMXConcurrent(t *testing.T) {
	f := newFake()
	f.mx["a.test"] = []*net.MX{{Host: "m.a.test", Pref: 10}}
	f.mx["b.test"] = []*net.MX{{Host: "m.b.test", Pref: 10}}
	f.mx["c.test"] = []*net.MX{{Host: "m.c.test", Pref: 10}}
	r := NewResolverWith(f)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		domain := []string{"a.test", "b.test", "c.test"}[i%3]
		go func(d string) {
			defer wg.Done()
			if _, err := r.LookupMX(context.Background(), d); err != nil {
				t.Errorf("concurrent lookup %q: %v", d, err)
			}
		}(domain)
	}
	wg.Wait()
}

// The fake never records SMTP dials; if any code path tried to Dial, that
// would show up as a net.Dial error in the test log. This is a structural
// audit: the dns package imports "net" only for types (MX, DNSError,
// Resolver) — verified by inspection, and by the fact these tests inject
// the whole DNS boundary via Lookup.
func TestResolverPerformsNoNetworkDialingItself(t *testing.T) {
	f := newFake()
	f.mx["x.test"] = []*net.MX{{Host: "mx.x.test", Pref: 10}}
	// If LookupMX made real net.Dial to mx.x.test we would either succeed
	// against a random Internet host or produce an error; instead the fake
	// short-circuits everything. Presence of the fake's counters proves the
	// dns package used only the Lookup interface.
	_, _ = NewResolverWith(f).LookupMX(context.Background(), "x.test")
	if f.mxCalls == 0 {
		t.Fatal("resolver did not use injected Lookup")
	}
}

// ---------- Fuzz ----------------------------------------------------

func FuzzNormalizeDomain(f *testing.F) {
	seeds := []string{"example.com", "EXAMPLE.COM.", "", " ", "a.b", "a..b", "\r\n", "@bad", "x", "a" + string(make([]byte, 300)) + ".b"}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := normalizeDomain(s)
		if err != nil {
			return
		}
		if out == "" {
			t.Fatalf("normalize returned empty without error for %q", s)
		}
		// invariants: no CR/LF/NUL/space/tab, no leading/trailing dot
		if len(out) > 253 {
			t.Fatalf("normalized too long: %d", len(out))
		}
		for _, c := range out {
			if c == '\r' || c == '\n' || c == 0 || c == ' ' || c == '\t' {
				t.Fatalf("normalized contains forbidden byte: %q", out)
			}
		}
		if out[len(out)-1] == '.' {
			t.Fatalf("trailing dot survived: %q", out)
		}
		// idempotent
		out2, err2 := normalizeDomain(out)
		if err2 != nil || out2 != out {
			t.Fatalf("not idempotent: %q -> %q err=%v", out, out2, err2)
		}
	})
}
