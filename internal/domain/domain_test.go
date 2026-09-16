package domain

import (
	"context"
	"errors"
	"net"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func TestNormalize(t *testing.T) {
	valid := map[string]string{
		"example.com": "example.com", " EXAMPLE.COM. ": "example.com",
		"mail.example.co.uk": "mail.example.co.uk", "a-b.example.com": "a-b.example.com",
	}
	for input, want := range valid {
		if got, err := Normalize(input); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	invalid := []string{"", ".", "com", "co.uk", "localhost", "example..com", ".example.com",
		"example.com..", "-a.example.com", "a-.example.com", "a_b.example.com", "*.example.com",
		"127.0.0.1", "[::1]", "café.com", "xn--caf-dma.com", "example.invalid",
		string(make([]byte, 254)) + ".com", string(make([]byte, 64)) + ".com"}
	for _, input := range invalid {
		if _, err := Normalize(input); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Normalize(%q) error = %v, want ErrInvalidName", input, err)
		}
	}
}

func FuzzNormalizeNeverPanics(f *testing.F) {
	for _, seed := range []string{"example.com", "EXAMPLE.COM.", "*.example.com", "café.com", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) { _, _ = Normalize(input) })
}

func TestGeneratedTokensAreWellFormedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	pattern := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	for i := 0; i < 1000; i++ {
		token, err := generateToken()
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(token) || seen[token] {
			t.Fatalf("invalid or duplicate generated token %q", token)
		}
		seen[token] = true
	}
}

func TestExactTXTMatch(t *testing.T) {
	expected := "mailx-verification=abc123"
	if !exactTXTMatch([]string{"google-site-verification=x", expected, "other=y"}, expected) {
		t.Fatal("exact value among multiple records did not match")
	}
	for _, value := range []string{"prefix-" + expected, expected + "-extra", "abc123", " " + expected, "MAILX-verification=abc123"} {
		if exactTXTMatch([]string{value}, expected) {
			t.Fatalf("false positive for %q", value)
		}
	}
}

type fakeStore struct {
	mu       sync.Mutex
	resource database.Domain
}

func (s *fakeStore) CreateDomain(context.Context, string, string, string) (database.Domain, error) {
	return database.Domain{}, nil
}
func (s *fakeStore) GetDomain(_ context.Context, tenantID, id string) (database.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resource.TenantID != tenantID || s.resource.ID != id {
		return database.Domain{}, database.ErrNotFound
	}
	return s.resource, nil
}
func (s *fakeStore) ListDomains(context.Context, string, int, *database.DomainCursor) ([]database.Domain, error) {
	return nil, nil
}
func (s *fakeStore) RecordDomainCheck(_ context.Context, _, _ string, verified bool, checkedAt time.Time) (database.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resource.LastCheckedAt = &checkedAt
	if verified {
		s.resource.VerificationStatus = database.DomainVerified
		s.resource.VerifiedAt = &checkedAt
	}
	return s.resource, nil
}
func (s *fakeStore) DeleteDomain(context.Context, string, string, time.Time) error { return nil }

type fakeTXT struct {
	records []string
	err     error
	waits   bool
}

func (f fakeTXT) LookupTXT(ctx context.Context, _ string) ([]string, error) {
	if f.waits {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.records, f.err
}

func pendingResource() database.Domain {
	return database.Domain{ID: "dom", TenantID: "tenant", Name: "example.com", VerificationToken: "token", VerificationStatus: database.DomainPending}
}

func TestVerifyOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		resolver   fakeTXT
		wantStatus database.DomainStatus
		wantErr    error
	}{
		{"exact", fakeTXT{records: []string{"other", RecordValue("token")}}, database.DomainVerified, nil},
		{"wrong", fakeTXT{records: []string{"wrong"}}, database.DomainPending, nil},
		{"empty", fakeTXT{}, database.DomainPending, nil},
		{"nxdomain", fakeTXT{err: &net.DNSError{IsNotFound: true}}, database.DomainPending, nil},
		{"servfail", fakeTXT{err: &net.DNSError{IsTemporary: true}}, "", ErrDNSUnavailable},
		{"timeout", fakeTXT{waits: true}, "", ErrDNSUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{resource: pendingResource()}
			svc := NewService(store, tc.resolver)
			svc.timeout = 2 * time.Millisecond
			got, err := svc.Verify(context.Background(), "tenant", "dom")
			if !errors.Is(err, tc.wantErr) || (err == nil && got.VerificationStatus != tc.wantStatus) {
				t.Fatalf("got status=%q err=%v, want status=%q err=%v", got.VerificationStatus, err, tc.wantStatus, tc.wantErr)
			}
		})
	}
}

func TestAlreadyVerifiedDoesNotQueryDNS(t *testing.T) {
	resource := pendingResource()
	resource.VerificationStatus = database.DomainVerified
	now := time.Now()
	resource.VerifiedAt = &now
	store := &fakeStore{resource: resource}
	got, err := NewService(store, fakeTXT{err: errors.New("must not be called")}).Verify(context.Background(), "tenant", "dom")
	if err != nil || got.VerificationStatus != database.DomainVerified {
		t.Fatalf("already verified should be idempotent: %+v, %v", got, err)
	}
}
