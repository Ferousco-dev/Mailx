package bimi

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func notFound() error { return &net.DNSError{Err: "no such host", IsNotFound: true} }

type fakeStore struct {
	mu      sync.Mutex
	domains map[string]database.Domain
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
	d, ok := s.domains[tenant+"|"+id]
	if !ok {
		return database.Domain{}, database.ErrNotFound
	}
	return d, nil
}

type fakeDNS struct {
	fn func(ctx context.Context, name string) ([]string, error)
}

func (f fakeDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return f.fn(ctx, name)
}

type fakeDMARC struct {
	view DMARCPrereq
	err  error
}

func (f fakeDMARC) DMARCView(context.Context, string, string) (DMARCPrereq, error) {
	return f.view, f.err
}

func newTestService(t *testing.T, store *fakeStore, dns TXTResolver, dmarcState DMARCState) *Service {
	t.Helper()
	svc, err := NewService(store, dns, dmarcState, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestServiceDescribeDoesNoDNS(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	dns := fakeDNS{fn: func(context.Context, string) ([]string, error) {
		t.Fatal("Describe must not query DNS")
		return nil, nil
	}}
	svc := newTestService(t, store, dns, nil)
	res, err := svc.Describe(context.Background(), "t1", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessUnchecked {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestServiceVerifyRequiresVerifiedDomain(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", false)
	svc := newTestService(t, store, fakeDNS{fn: func(context.Context, string) ([]string, error) { return nil, notFound() }}, nil)
	_, err := svc.Verify(context.Background(), "t1", "d1", false)
	if !errors.Is(err, ErrDomainNotVerified) {
		t.Fatalf("got %v", err)
	}
}

func TestServiceVerifyNotConfigured(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	svc := newTestService(t, store, fakeDNS{fn: func(context.Context, string) ([]string, error) { return nil, notFound() }},
		fakeDMARC{view: DMARCPrereq{Checked: true, EffectivePolicy: "reject", OrgDomain: "example.com"}})
	res, err := svc.Verify(context.Background(), "t1", "d1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessNotConfigured {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestServiceVerifyFoundAndReady(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	dns := fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		if name == "default._bimi.example.com" {
			return []string{"v=BIMI1; l=https://example.com/logo.svg"}, nil
		}
		return nil, notFound()
	}}
	svc := newTestService(t, store, dns, fakeDMARC{view: DMARCPrereq{Checked: true, EffectivePolicy: "reject", OrgDomain: "example.com"}})
	res, err := svc.Verify(context.Background(), "t1", "d1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessReady {
		t.Fatalf("got %q", res.Readiness)
	}
	if res.DNS.Record.Location != "https://example.com/logo.svg" {
		t.Fatalf("unexpected record: %+v", res.DNS.Record)
	}
}

func TestServiceVerifyFallsBackToOrgDomain(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "mail.example.com", true)
	dns := fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		switch name {
		case "default._bimi.mail.example.com":
			return nil, notFound()
		case "default._bimi.example.com":
			return []string{"v=BIMI1; l=https://example.com/logo.svg"}, nil
		}
		return nil, notFound()
	}}
	svc := newTestService(t, store, dns, fakeDMARC{view: DMARCPrereq{Checked: true, EffectivePolicy: "reject", OrgDomain: "example.com"}})
	res, err := svc.Verify(context.Background(), "t1", "d1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessReady || res.DNS.Source != "organizational_domain" {
		t.Fatalf("expected org-domain fallback to find the record: %+v", res)
	}
}

func TestServiceVerifyDMARCPrereqFailed(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	dns := fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		if name == "default._bimi.example.com" {
			return []string{"v=BIMI1; l=https://example.com/logo.svg"}, nil
		}
		return nil, notFound()
	}}
	svc := newTestService(t, store, dns, fakeDMARC{view: DMARCPrereq{Checked: true, EffectivePolicy: "none"}})
	res, err := svc.Verify(context.Background(), "t1", "d1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessDMARCPrereqFailed {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestServiceVerifyInvalidRecordMultipleAnswers(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	dns := fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		if name == "default._bimi.example.com" {
			return []string{"v=BIMI1; l=https://a/x.svg", "v=BIMI1; l=https://b/y.svg"}, nil
		}
		return nil, notFound()
	}}
	svc := newTestService(t, store, dns, fakeDMARC{view: DMARCPrereq{Checked: true, EffectivePolicy: "reject"}})
	res, err := svc.Verify(context.Background(), "t1", "d1", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessInvalidRecord {
		t.Fatalf("expected multiple BIMI records to be invalid, got %q", res.Readiness)
	}
}

func TestServiceVerifyDeclinedNeverChecksAssetsEvenIfRequested(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	dns := fakeDNS{fn: func(_ context.Context, name string) ([]string, error) {
		if name == "default._bimi.example.com" {
			return []string{"v=BIMI1; l="}, nil
		}
		return nil, notFound()
	}}
	svc := newTestService(t, store, dns, fakeDMARC{view: DMARCPrereq{Checked: true, EffectivePolicy: "reject"}})
	res, err := svc.Verify(context.Background(), "t1", "d1", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Readiness != ReadinessDeclined {
		t.Fatalf("got %q", res.Readiness)
	}
	if res.Logo.Checked {
		t.Fatal("a declined record must never trigger an asset fetch")
	}
}

func TestServiceVerifyTenantIsolation(t *testing.T) {
	store := &fakeStore{}
	store.add("t1", "d1", "example.com", true)
	svc := newTestService(t, store, fakeDNS{fn: func(context.Context, string) ([]string, error) { return nil, notFound() }}, nil)
	if _, err := svc.Verify(context.Background(), "t2", "d1", false); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant Verify must 404, got %v", err)
	}
}
