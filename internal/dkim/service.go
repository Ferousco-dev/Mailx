package dkim

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
)

// Signing outcomes: a bounded set, safe as metric labels and in logs.
const (
	OutcomeSigned         = "signed"
	OutcomeNoKey          = "unsigned_no_key" // the domain has no active key: DKIM not set up
	OutcomeKeyUnavailable = "key_unavailable"
	OutcomeKeyDecryptFail = "key_decrypt_failed"
	OutcomeKeyInvalid     = "key_invalid"
	OutcomeSignFailed     = "sign_failed"
	OutcomeDomainMismatch = "domain_mismatch"
)

// ErrDNSUnavailable means the publication check could not run (DNS failure,
// not "record absent").
var ErrDNSUnavailable = errors.New("dkim: DNS lookup unavailable")

// SignError is the bounded error for a signing failure. It never contains key
// material, ciphertext, message data or raw crypto errors.
type SignError struct{ Outcome string }

func (e *SignError) Error() string { return "dkim signing failed: " + e.Outcome }

// Store is the persistence the service needs.
type Store interface {
	GetDomain(ctx context.Context, tenantID, id string) (database.Domain, error)
	CreateDKIMKey(ctx context.Context, tenantID, domainID string, in database.NewDKIMKey) (database.DKIMKey, error)
	ListDKIMKeys(ctx context.Context, tenantID, domainID string) ([]database.DKIMKey, error)
	ActivateDKIMKey(ctx context.Context, tenantID, domainID string, now time.Time) (database.DKIMKey, error)
	ActiveSigningKey(ctx context.Context, tenantID, domainName string) (database.DKIMKey, error)
}

// TXTResolver is the DNS capability publication checks need (same shape as the
// domain-ownership resolver, so the same implementation is reused).
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// Observer receives one bounded event per signing attempt.
type Observer interface {
	SignResult(algorithm, outcome string)
}

// Service manages key lifecycle and signs messages. It holds no key material:
// private keys are decrypted per signature and dropped.
type Service struct {
	store    Store
	box      *secretbox.Box
	resolver TXTResolver
	observer Observer
	timeout  time.Duration
	now      func() time.Time
	genKey   func() (*rsa.PrivateKey, error)
	// genSlots bounds concurrent RSA key generation (CPU-heavy and not
	// cancellable) so a burst of requests cannot saturate the process.
	genSlots chan struct{}
}

const maxConcurrentKeyGenerations = 2

func NewService(store Store, box *secretbox.Box, resolver TXTResolver, observer Observer) (*Service, error) {
	if store == nil || box == nil || resolver == nil {
		return nil, errors.New("dkim: store, secret box and resolver are required")
	}
	return &Service{store: store, box: box, resolver: resolver, observer: observer, timeout: 5 * time.Second,
		now: func() time.Time { return time.Now().UTC() }, genKey: GenerateKey,
		genSlots: make(chan struct{}, maxConcurrentKeyGenerations)}, nil
}

// aad binds a ciphertext to its tenant, domain and selector so a row's private
// key cannot be swapped into another row and still decrypt.
func aad(tenantID, domainID, selector string) []byte {
	return []byte("mailx-dkim-v1\x00" + tenantID + "\x00" + domainID + "\x00" + selector)
}

func newSelector(now time.Time) (string, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("dkim: cannot generate selector")
	}
	return "mx" + now.Format("20060102") + hex.EncodeToString(b[:]), nil
}

// CreateKey generates a key for a verified domain and stores it PENDING (not
// used for signing until its DNS record is verified). If the domain already
// has an active key this is a rotation: the active key keeps signing until the
// new one is activated. Only one pending key may exist (ErrDKIMKeyPending).
func (s *Service) CreateKey(ctx context.Context, tenantID, domainID string) (database.DKIMKey, database.Domain, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, domainID)
	if err != nil {
		return database.DKIMKey{}, database.Domain{}, err
	}
	if dom.DeletedAt != nil || dom.VerificationStatus != database.DomainVerified {
		return database.DKIMKey{}, dom, database.ErrDomainNotVerified
	}
	// Cheap checks first: a domain that already has a pending key must not pay
	// for another RSA generation only to be told 409. (The unique index remains
	// the authority for races.)
	existing, err := s.store.ListDKIMKeys(ctx, tenantID, dom.ID)
	if err != nil {
		return database.DKIMKey{}, dom, err
	}
	for _, k := range existing {
		if k.Status == database.DKIMPending {
			return database.DKIMKey{}, dom, database.ErrDKIMKeyPending
		}
	}
	select {
	case s.genSlots <- struct{}{}:
	case <-ctx.Done():
		return database.DKIMKey{}, dom, ctx.Err()
	}
	priv, err := s.genKey()
	<-s.genSlots
	if err != nil {
		return database.DKIMKey{}, dom, err
	}
	der, err := MarshalPrivate(priv)
	if err != nil {
		return database.DKIMKey{}, dom, err
	}
	pub, err := PublicKeyBase64(&priv.PublicKey)
	if err != nil {
		return database.DKIMKey{}, dom, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		selector, err := newSelector(s.now())
		if err != nil {
			return database.DKIMKey{}, dom, err
		}
		ct, nonce, err := s.box.Encrypt(der, aad(tenantID, dom.ID, selector))
		if err != nil {
			return database.DKIMKey{}, dom, err
		}
		key, err := s.store.CreateDKIMKey(ctx, tenantID, dom.ID, database.NewDKIMKey{
			Selector: selector, Algorithm: Algorithm, KeyBits: priv.N.BitLen(), PublicKey: pub,
			PrivateCiphertext: ct, PrivateNonce: nonce})
		if errors.Is(err, database.ErrConflict) { // selector collision: pick another
			continue
		}
		return key, dom, err
	}
	return database.DKIMKey{}, dom, database.ErrConflict
}

// Keys lists a domain's keys (public metadata only).
func (s *Service) Keys(ctx context.Context, tenantID, domainID string) (database.Domain, []database.DKIMKey, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, domainID)
	if err != nil {
		return database.Domain{}, nil, err
	}
	keys, err := s.store.ListDKIMKeys(ctx, tenantID, domainID)
	return dom, keys, err
}

// VerifyPending checks that the pending key's public key is published in DNS
// and, if so, activates it (retiring the previous active key). published=false
// with a nil error means the record is absent or different: nothing changed.
func (s *Service) VerifyPending(ctx context.Context, tenantID, domainID string) (key database.DKIMKey, published bool, err error) {
	dom, keys, err := s.Keys(ctx, tenantID, domainID)
	if err != nil {
		return database.DKIMKey{}, false, err
	}
	var pending *database.DKIMKey
	for i := range keys {
		if keys[i].Status == database.DKIMPending {
			pending = &keys[i]
		}
	}
	if pending == nil {
		return database.DKIMKey{}, false, database.ErrNotFound
	}
	lctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	records, lerr := s.resolver.LookupTXT(lctx, DNSName(pending.Selector, dom.Name))
	if lerr != nil {
		var dnsErr *net.DNSError
		if errors.As(lerr, &dnsErr) && dnsErr.IsNotFound {
			return *pending, false, nil
		}
		return database.DKIMKey{}, false, fmt.Errorf("%w", ErrDNSUnavailable)
	}
	if !publishes(records, pending.PublicKey) {
		return *pending, false, nil
	}
	active, err := s.store.ActivateDKIMKey(ctx, tenantID, domainID, s.now())
	if err != nil {
		return database.DKIMKey{}, false, err
	}
	return active, true, nil
}

// publishes reports whether any TXT record is a DKIM key record whose p= equals
// the stored public key (whitespace inside the value is ignored).
func publishes(records []string, publicKeyB64 string) bool {
	for _, rec := range records {
		var version, kind, p string
		kind = "rsa"
		for _, part := range strings.Split(rec, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			v = strings.Join(strings.Fields(v), "")
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "v":
				version = v
			case "k":
				kind = strings.ToLower(v)
			case "p":
				p = v
			}
		}
		if (version == "" || version == "DKIM1") && kind == "rsa" && p != "" && p == publicKeyB64 {
			return true
		}
	}
	return false
}

// SignMessage signs raw for the tenant's domain when the domain has an ACTIVE
// key. No active key means DKIM is not set up and the message is returned
// unchanged (Signed=false). If a key exists but cannot be used for ANY reason,
// it returns a *SignError and the caller MUST refuse the message: there is no
// unsigned fallback once a domain has an active key.
func (s *Service) SignMessage(ctx context.Context, tenantID, domainName string, raw []byte) (signed []byte, didSign bool, err error) {
	outcome := OutcomeSigned
	defer func() {
		if s.observer != nil {
			func() { defer func() { _ = recover() }(); s.observer.SignResult(Algorithm, outcome) }()
		}
	}()
	row, err := s.store.ActiveSigningKey(ctx, tenantID, domainName)
	switch {
	case errors.Is(err, database.ErrNotFound):
		outcome = OutcomeNoKey
		return raw, false, nil
	case err != nil:
		outcome = OutcomeKeyUnavailable
		return nil, false, &SignError{Outcome: outcome}
	}
	der, err := s.box.Decrypt(row.PrivateCiphertext, row.PrivateNonce, aad(row.TenantID, row.DomainID, row.Selector))
	if err != nil {
		outcome = OutcomeKeyDecryptFail
		return nil, false, &SignError{Outcome: outcome}
	}
	key, err := ParsePrivate(der)
	if err == nil {
		var pub string
		pub, err = PublicKeyBase64(&key.PublicKey)
		if err == nil && pub != row.PublicKey {
			err = errors.New("public key mismatch")
		}
	}
	if err != nil {
		outcome = OutcomeKeyInvalid
		return nil, false, &SignError{Outcome: outcome}
	}
	out, err := Sign(raw, Options{Domain: domainName, Selector: row.Selector, Key: key, Now: s.now()})
	switch {
	case errors.Is(err, ErrDomainMismatch):
		outcome = OutcomeDomainMismatch
		return nil, false, &SignError{Outcome: outcome}
	case err != nil:
		outcome = OutcomeSignFailed
		return nil, false, &SignError{Outcome: outcome}
	}
	return out, true, nil
}
