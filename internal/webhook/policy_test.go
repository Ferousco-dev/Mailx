package webhook

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

type fakeIPResolver struct {
	answers map[string][]netip.Addr
	err     error
}

func (f fakeIPResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.answers[host], nil
}

func TestURLPolicyRejectsSSRFAndMalformedDestinations(t *testing.T) {
	resolver := fakeIPResolver{answers: map[string][]netip.Addr{
		"public.example":  {netip.MustParseAddr("8.8.8.8")},
		"private.example": {netip.MustParseAddr("10.0.0.1")},
		"mixed.example":   {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
		"localhost":       {netip.MustParseAddr("127.0.0.1")},
	}}
	policy := URLPolicy{Resolver: resolver}
	if got, err := policy.Validate(context.Background(), "https://public.example/hook"); err != nil || got == "" {
		t.Fatalf("public HTTPS rejected: %v", err)
	}
	bad := []string{
		"http://public.example/hook", "https://user:pass@public.example", "https://public.example/#x",
		"ftp://public.example", "https://127.0.0.1", "https://0.0.0.0", "https://10.0.0.1",
		"https://169.254.169.254/latest/meta-data", "https://[::1]", "https://[fc00::1]",
		"https://[fe80::1]", "https://localhost", "https://private.example", "https://mixed.example",
		"https://public.example:99999", "https://public.example\n.internal",
	}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			if _, err := policy.Validate(context.Background(), raw); err == nil {
				t.Fatal("dangerous URL accepted")
			}
		})
	}
}

func TestDevelopmentOverrideIsExplicit(t *testing.T) {
	policy := URLPolicy{AllowHTTP: true, AllowPrivate: true, Resolver: fakeIPResolver{answers: map[string][]netip.Addr{"localhost": {netip.MustParseAddr("127.0.0.1")}}}}
	if _, err := policy.Validate(context.Background(), "http://localhost:8080/hook"); err != nil {
		t.Fatalf("explicit development override rejected: %v", err)
	}
}

func TestDialTimeDNSRebindingFailsClosed(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]netip.Addr{{netip.MustParseAddr("8.8.8.8")}, {netip.MustParseAddr("127.0.0.1")}}}
	policy := URLPolicy{Resolver: resolver}
	if _, err := policy.Validate(context.Background(), "https://rebind.example/hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.DialContext(context.Background(), "tcp", "rebind.example:443"); err == nil {
		t.Fatal("dial-time private rebound address accepted")
	}
}

type sequenceResolver struct {
	answers [][]netip.Addr
	index   int
}

func (r *sequenceResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	if r.index >= len(r.answers) {
		return nil, errors.New("no answer")
	}
	answer := r.answers[r.index]
	r.index++
	return answer, nil
}

type failingResolver struct{ err error }

func (f failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, f.err
}

func TestValidateDoesNotExposeResolverDetail(t *testing.T) {
	secretDetail := "lookup internal.example on 10.9.8.7:53: server misbehaving"
	p := URLPolicy{Resolver: failingResolver{&net.DNSError{Err: secretDetail, Server: "10.9.8.7:53"}}}
	_, err := p.Validate(context.Background(), "https://hooks.example.com/x")
	if !errors.Is(err, ErrDNSUnavailable) || strings.Contains(err.Error(), "10.9.8.7") || strings.Contains(err.Error(), "misbehaving") {
		t.Fatalf("resolver failure must map to a stable public error, got %v", err)
	}
	notFound := URLPolicy{Resolver: failingResolver{&net.DNSError{Err: "no such host", Name: "nope.invalid", Server: "10.9.8.7:53", IsNotFound: true}}}
	_, err = notFound.Validate(context.Background(), "https://nope.invalid/x")
	if err == nil || errors.Is(err, ErrDNSUnavailable) || strings.Contains(err.Error(), "10.9.8.7") {
		t.Fatalf("NXDOMAIN is a validation failure without resolver detail, got %v", err)
	}
}
