// Package domain implements tenant-owned DNS domain resources and ownership
// verification. Verification proves control of one DNS name at one point in
// time; it does not imply DKIM, SPF, DMARC, or deliverability readiness.
package domain

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"golang.org/x/net/publicsuffix"
)

const (
	recordLabel       = "_mailx-verification"
	recordValuePrefix = "mailx-verification="
	challengeBytes    = 32
	defaultTimeout    = 5 * time.Second
)

var (
	ErrInvalidName       = errors.New("domain: invalid name")
	ErrAlreadyExists     = errors.New("domain: already exists")
	ErrOwnershipConflict = errors.New("domain: ownership conflict")
	ErrDNSUnavailable    = errors.New("domain: DNS lookup unavailable")
)

// Normalize canonicalizes a public ASCII DNS name before any ownership
// comparison. Unicode/A-label IDNs and non-ICANN/internal suffixes are
// deliberately rejected in v0.21 rather than accepted inconsistently.
func Normalize(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	name = strings.TrimSuffix(name, ".")
	name = strings.ToLower(name)
	if name == "" || len(name) > 253 || strings.Contains(name, "*") || net.ParseIP(name) != nil {
		return "", ErrInvalidName
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", ErrInvalidName
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.HasPrefix(label, "xn--") {
			return "", ErrInvalidName
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return "", ErrInvalidName
			}
		}
	}
	suffix, icann := publicsuffix.PublicSuffix(name)
	if !icann || suffix == name {
		return "", ErrInvalidName
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(name); err != nil {
		return "", ErrInvalidName
	}
	return name, nil
}

// FromDomain extracts the canonical domain of an RFC 5322 mailbox ("Name
// <user@host>", "<user@host>" or "user@host") using the standard-library
// address parser and the SAME Normalize used for domain ownership, so sender
// authorization and ownership can never disagree about what a domain is. It
// never matches by suffix: only the parsed domain of the actual mailbox counts.
func FromDomain(mailbox string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(mailbox))
	if err != nil {
		return "", ErrInvalidName
	}
	at := strings.LastIndexByte(addr.Address, '@')
	if at <= 0 || at == len(addr.Address)-1 {
		return "", ErrInvalidName
	}
	return Normalize(addr.Address[at+1:])
}

func generateToken() (string, error) {
	b := make([]byte, challengeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("domain: generate verification token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func RecordName(name string) string   { return recordLabel + "." + name }
func RecordValue(token string) string { return recordValuePrefix + token }

func exactTXTMatch(records []string, expected string) bool {
	for _, record := range records {
		if record == expected {
			return true
		}
	}
	return false
}

// TXTResolver is the one network capability ownership verification needs.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type netTXTResolver struct{ resolver *net.Resolver }

func (r netTXTResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return r.resolver.LookupTXT(ctx, name)
}

func NewNetTXTResolver() TXTResolver { return netTXTResolver{resolver: net.DefaultResolver} }

type Store interface {
	CreateDomain(ctx context.Context, tenantID, name, token string) (database.Domain, error)
	GetDomain(ctx context.Context, tenantID, id string) (database.Domain, error)
	ListDomains(ctx context.Context, tenantID string, limit int, after *database.DomainCursor) ([]database.Domain, error)
	RecordDomainCheck(ctx context.Context, tenantID, id string, verified bool, checkedAt time.Time) (database.Domain, error)
	DeleteDomain(ctx context.Context, tenantID, id string, deletedAt time.Time) error
}

type Service struct {
	store    Store
	resolver TXTResolver
	timeout  time.Duration
	now      func() time.Time
}

func NewService(store Store, resolver TXTResolver) *Service {
	return &Service{store: store, resolver: resolver, timeout: defaultTimeout, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, tenantID, rawName string) (database.Domain, error) {
	name, err := Normalize(rawName)
	if err != nil {
		return database.Domain{}, err
	}
	token, err := generateToken()
	if err != nil {
		return database.Domain{}, err
	}
	created, err := s.store.CreateDomain(ctx, tenantID, name, token)
	if errors.Is(err, database.ErrConflict) {
		return database.Domain{}, ErrAlreadyExists
	}
	return created, err
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (database.Domain, error) {
	return s.store.GetDomain(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, tenantID string, limit int, after *database.DomainCursor) ([]database.Domain, error) {
	return s.store.ListDomains(ctx, tenantID, limit, after)
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.DeleteDomain(ctx, tenantID, id, s.now())
}

func (s *Service) Verify(ctx context.Context, tenantID, id string) (database.Domain, error) {
	resource, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil || resource.VerificationStatus == database.DomainVerified {
		return resource, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	records, lookupErr := s.resolver.LookupTXT(lookupCtx, RecordName(resource.Name))
	checkedAt := s.now()
	if lookupErr != nil {
		var dnsErr *net.DNSError
		if errors.As(lookupErr, &dnsErr) && dnsErr.IsNotFound {
			return s.store.RecordDomainCheck(ctx, tenantID, id, false, checkedAt)
		}
		return database.Domain{}, fmt.Errorf("%w: %v", ErrDNSUnavailable, lookupErr)
	}
	verified := exactTXTMatch(records, RecordValue(resource.VerificationToken))
	updated, err := s.store.RecordDomainCheck(ctx, tenantID, id, verified, checkedAt)
	if verified && errors.Is(err, database.ErrConflict) {
		return database.Domain{}, ErrOwnershipConflict
	}
	return updated, err
}
