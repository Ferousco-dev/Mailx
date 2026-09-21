package dkim

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/outbound"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
)

// testDB returns an isolated PostgreSQL schema (skipped only when no database
// is reachable, which CI and the repository's real-dependency runs provide).
func testDB(t testing.TB) (*database.DB, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("MAILX_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s@localhost:5432/mailx_test?sslmode=disable", os.Getenv("USER"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil || admin.Ping(ctx) != nil {
		if admin != nil {
			admin.Close()
		}
		t.Skip("no PostgreSQL available for DKIM integration tests")
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	schema := "mailx_dkim_test_" + hex.EncodeToString(raw)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cn := context.WithTimeout(context.Background(), 10*time.Second)
		defer cn()
		_, _ = admin.Exec(c, `DROP SCHEMA "`+schema+`" CASCADE`)
		admin.Close()
	})
	db, err := database.Open(ctx, database.Config{DSN: dsn + "&search_path=" + schema, MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// A second raw pool on the same schema lets tests inspect and corrupt rows.
	rawPool, err := pgxpool.New(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rawPool.Close)
	return db, rawPool
}

type fakeDNS struct {
	mu      sync.Mutex
	records map[string][]string
	err     error
}

func (f *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if r, ok := f.records[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeDNS) publish(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[string][]string{}
	}
	f.records[name] = []string{value}
}

type obsRec struct {
	mu  sync.Mutex
	got []string
}

func (o *obsRec) SignResult(alg, outcome string) {
	o.mu.Lock()
	o.got = append(o.got, alg+"/"+outcome)
	o.mu.Unlock()
}

func (o *obsRec) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.got) == 0 {
		return ""
	}
	return o.got[len(o.got)-1]
}

type rig struct {
	db  *database.DB
	raw *pgxpool.Pool
	svc *Service
	box *secretbox.Box
	dns *fakeDNS
	obs *obsRec
	ctx context.Context
}

func newRig(t testing.TB) *rig {
	t.Helper()
	db, raw := testDB(t)
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	box, _ := secretbox.New(mk)
	r := &rig{db: db, raw: raw, box: box, dns: &fakeDNS{}, obs: &obsRec{}, ctx: context.Background()}
	svc, err := NewService(db, box, r.dns, r.obs)
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

func (r *rig) tenant(t testing.TB, name string) database.Tenant {
	t.Helper()
	tn, err := r.db.CreateTenant(r.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func (r *rig) verifiedDomain(t testing.TB, tenantID, name string) database.Domain {
	t.Helper()
	d, err := r.db.CreateDomain(r.ctx, tenantID, name, "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	d, err = r.db.RecordDomainCheck(r.ctx, tenantID, d.ID, true, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// activate runs the full setup: create, publish the record, verify.
func (r *rig) activate(t testing.TB, tenantID string, d database.Domain) database.DKIMKey {
	t.Helper()
	k, _, err := r.svc.CreateKey(r.ctx, tenantID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.dns.publish(DNSName(k.Selector, d.Name), DNSValue(k.PublicKey))
	active, published, err := r.svc.VerifyPending(r.ctx, tenantID, d.ID)
	if err != nil || !published || active.Status != database.DKIMActive {
		t.Fatalf("activate: %+v published=%v err=%v", active, published, err)
	}
	return active
}

func (r *rig) message(t testing.TB, from string) []byte {
	t.Helper()
	b, err := outbound.Build(outbound.Request{From: from, To: []string{"bob@example.net"}, Subject: "hello", Text: "body",
		MessageID: "<m1@mailx.local>", Date: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(b.Raw)
}

func TestLifecycleCreatePublishActivateAndRotate(t *testing.T) {
	r := newRig(t)
	tn := r.tenant(t, "t1")
	d := r.verifiedDomain(t, tn.ID, "example.com")

	// Not set up yet: no key means unsigned, explicitly reported.
	if out, signed, err := r.svc.SignMessage(r.ctx, tn.ID, "example.com", r.message(t, "a@example.com")); err != nil || signed || !strings.HasPrefix(string(out), "From:") {
		t.Fatalf("no key must mean unsigned, no error: signed=%v err=%v", signed, err)
	}
	if r.obs.last() != "rsa-sha256/unsigned_no_key" {
		t.Fatalf("observer = %q", r.obs.last())
	}

	// Pending key does NOT sign, and verification fails until DNS has the record.
	k1, _, err := r.svc.CreateKey(r.ctx, tn.ID, d.ID)
	if err != nil || k1.Status != database.DKIMPending || k1.KeyBits != 2048 || k1.Algorithm != "rsa-sha256" {
		t.Fatalf("%+v %v", k1, err)
	}
	if _, signed, _ := r.svc.SignMessage(r.ctx, tn.ID, "example.com", r.message(t, "a@example.com")); signed {
		t.Fatal("a pending (unpublished) key must not sign")
	}
	if _, published, err := r.svc.VerifyPending(r.ctx, tn.ID, d.ID); err != nil || published {
		t.Fatalf("unpublished record must not activate: published=%v err=%v", published, err)
	}
	// A record with a DIFFERENT public key is not our key.
	other, _ := GenerateKey()
	otherPub, _ := PublicKeyBase64(&other.PublicKey)
	r.dns.publish(DNSName(k1.Selector, "example.com"), DNSValue(otherPub))
	if _, published, _ := r.svc.VerifyPending(r.ctx, tn.ID, d.ID); published {
		t.Fatal("a different published key must not activate ours")
	}
	r.dns.publish(DNSName(k1.Selector, "example.com"), DNSValue(k1.PublicKey))
	act1, published, err := r.svc.VerifyPending(r.ctx, tn.ID, d.ID)
	if err != nil || !published || act1.Status != database.DKIMActive {
		t.Fatalf("%+v %v %v", act1, published, err)
	}

	msg, signed, err := r.svc.SignMessage(r.ctx, tn.ID, "example.com", r.message(t, "a@example.com"))
	if err != nil || !signed {
		t.Fatalf("active key must sign: %v", err)
	}
	pub1 := decodePublic(t, act1.PublicKey)
	if err := verify(t, msg, "example.com", act1.Selector, pub1); err != nil {
		t.Fatalf("independent verification failed: %v", err)
	}

	// Rotation: new pending key; the old key keeps signing until activation.
	k2, _, err := r.svc.CreateKey(r.ctx, tn.ID, d.ID)
	if err != nil || k2.Selector == k1.Selector {
		t.Fatalf("rotation key: %+v %v", k2, err)
	}
	msg, _, _ = r.svc.SignMessage(r.ctx, tn.ID, "example.com", r.message(t, "a@example.com"))
	if err := verify(t, msg, "example.com", k1.Selector, pub1); err != nil {
		t.Fatalf("old key must keep signing during the transition: %v", err)
	}
	r.dns.publish(DNSName(k2.Selector, "example.com"), DNSValue(k2.PublicKey))
	act2, published, err := r.svc.VerifyPending(r.ctx, tn.ID, d.ID)
	if err != nil || !published || act2.Selector != k2.Selector {
		t.Fatalf("%+v %v %v", act2, published, err)
	}
	msg, _, _ = r.svc.SignMessage(r.ctx, tn.ID, "example.com", r.message(t, "a@example.com"))
	if err := verify(t, msg, "example.com", k2.Selector, decodePublic(t, act2.PublicKey)); err != nil {
		t.Fatalf("new key must sign after activation: %v", err)
	}

	// The retired key's private material is destroyed and it is not in use.
	var cipherNull bool
	if err := r.raw.QueryRow(r.ctx, `SELECT private_ciphertext IS NULL AND private_nonce IS NULL FROM dkim_keys WHERE selector = $1`, k1.Selector).Scan(&cipherNull); err != nil || !cipherNull {
		t.Fatalf("retired key must have no private material: %v", err)
	}
	var actives int
	_ = r.raw.QueryRow(r.ctx, `SELECT count(*) FROM dkim_keys WHERE domain_id = $1 AND status = 'active'`, d.ID).Scan(&actives)
	if actives != 1 {
		t.Fatalf("exactly one active key expected, got %d", actives)
	}
}

func decodePublic(t testing.TB, b64 string) *rsa.PublicKey {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	k, err := parsePublic(der)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeysRequireVerifiedDomainAndTenantOwnership(t *testing.T) {
	r := newRig(t)
	a, b := r.tenant(t, "a"), r.tenant(t, "b")
	pendingDomain, _ := r.db.CreateDomain(r.ctx, a.ID, "unverified.example", "tok")
	if _, _, err := r.svc.CreateKey(r.ctx, a.ID, pendingDomain.ID); !errors.Is(err, database.ErrDomainNotVerified) {
		t.Fatalf("unverified domain: %v", err)
	}
	da := r.verifiedDomain(t, a.ID, "a-domain.example")
	// Tenant B can neither create, list, verify nor sign for A's domain.
	if _, _, err := r.svc.CreateKey(r.ctx, b.ID, da.ID); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant create: %v", err)
	}
	r.activate(t, a.ID, da)
	if _, _, err := r.svc.Keys(r.ctx, b.ID, da.ID); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant list: %v", err)
	}
	if _, _, err := r.svc.VerifyPending(r.ctx, b.ID, da.ID); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("cross-tenant verify: %v", err)
	}
	// B has no authorization for the name, so its lookup finds no key: A's key is unreachable.
	if _, signed, _ := r.svc.SignMessage(r.ctx, b.ID, "a-domain.example", r.message(t, "x@a-domain.example")); signed {
		t.Fatal("tenant B signed with tenant A's key")
	}
}

func TestConcurrentCreateAndActivateKeepOneKeyStateSafe(t *testing.T) {
	r := newRig(t)
	tn := r.tenant(t, "t")
	d := r.verifiedDomain(t, tn.ID, "example.com")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := r.svc.CreateKey(r.ctx, tn.ID, d.ID)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, pending := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, database.ErrDKIMKeyPending):
			pending++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || pending != 7 {
		t.Fatalf("concurrent creation: %d created, %d pending-conflicts", ok, pending)
	}
	keys, _ := r.db.ListDKIMKeys(r.ctx, tn.ID, d.ID)
	r.dns.publish(DNSName(keys[0].Selector, "example.com"), DNSValue(keys[0].PublicKey))
	// Concurrent verification of the same pending key: one activation, no double active.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = r.svc.VerifyPending(r.ctx, tn.ID, d.ID) }()
	}
	wg.Wait()
	var actives, pendings int
	_ = r.raw.QueryRow(r.ctx, `SELECT count(*) FILTER (WHERE status='active'), count(*) FILTER (WHERE status='pending') FROM dkim_keys WHERE domain_id=$1`, d.ID).Scan(&actives, &pendings)
	if actives != 1 || pendings != 0 {
		t.Fatalf("after concurrent verification: %d active, %d pending", actives, pendings)
	}
}

// The private key is only ever stored as ciphertext bound to its row.
func TestPrivateKeyIsEncryptedAtRestAndBoundToItsRow(t *testing.T) {
	r := newRig(t)
	tn := r.tenant(t, "t")
	d := r.verifiedDomain(t, tn.ID, "example.com")
	k := r.activate(t, tn.ID, d)

	var ct, nonce []byte
	if err := r.raw.QueryRow(r.ctx, `SELECT private_ciphertext, private_nonce FROM dkim_keys WHERE id=$1`, k.ID).Scan(&ct, &nonce); err != nil {
		t.Fatal(err)
	}
	der, err := r.box.Decrypt(ct, nonce, aad(tn.ID, d.ID, k.Selector))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"BEGIN PRIVATE KEY", "BEGIN RSA PRIVATE KEY", base64.StdEncoding.EncodeToString(der)[:40]} {
		var found bool
		if err := r.raw.QueryRow(r.ctx, `SELECT EXISTS (SELECT 1 FROM dkim_keys WHERE position(convert_to($1,'UTF8') in private_ciphertext) > 0)`, needle).Scan(&found); err != nil || found {
			t.Fatalf("plaintext key material %q found in the row: %v", needle[:10], err)
		}
	}
	if strings.Contains(string(ct), string(der[:32])) {
		t.Fatal("ciphertext contains plaintext DER")
	}
	// Fresh nonces: encrypting twice never repeats a nonce or ciphertext.
	c1, n1, _ := r.box.Encrypt(der, aad(tn.ID, d.ID, k.Selector))
	c2, n2, _ := r.box.Encrypt(der, aad(tn.ID, d.ID, k.Selector))
	if string(n1) == string(n2) || string(c1) == string(c2) {
		t.Fatal("nonce or ciphertext repeated")
	}
	// Row binding: the same ciphertext is useless under another selector/domain.
	if _, err := r.box.Decrypt(ct, nonce, aad(tn.ID, d.ID, "othersel")); err == nil {
		t.Fatal("ciphertext must not decrypt under a different selector")
	}
	if _, err := r.box.Decrypt(ct, nonce, aad(tn.ID, "other-domain-id", k.Selector)); err == nil {
		t.Fatal("ciphertext must not decrypt for a different domain")
	}
}

// Failure modes never fall back to an unsigned message.
func TestSigningFailuresNeverFallBackToUnsigned(t *testing.T) {
	msg := func(r *rig) []byte { return r.message(t, "a@example.com") }
	cases := map[string]struct {
		break_  func(r *rig, tenantID, domainID string)
		outcome string
	}{
		"corrupt ciphertext": {func(r *rig, tn, d string) {
			_, err := r.raw.Exec(r.ctx, `UPDATE dkim_keys SET private_ciphertext = '\xdeadbeef' WHERE domain_id=$1 AND status='active'`, d)
			if err != nil {
				t.Fatal(err)
			}
		}, OutcomeKeyDecryptFail},
		"wrong master key": {func(r *rig, tn, d string) {
			k := make([]byte, 32)
			_, _ = rand.Read(k)
			r.svc.box, _ = secretbox.New(k)
		}, OutcomeKeyDecryptFail},
		"public key mismatch": {func(r *rig, tn, d string) {
			other, _ := GenerateKey()
			pub, _ := PublicKeyBase64(&other.PublicKey)
			if _, err := r.raw.Exec(r.ctx, `UPDATE dkim_keys SET public_key=$2 WHERE domain_id=$1 AND status='active'`, d, pub); err != nil {
				t.Fatal(err)
			}
		}, OutcomeKeyInvalid},
		"key store unavailable": {func(r *rig, tn, d string) { r.db.Close() }, OutcomeKeyUnavailable},
	}
	for name, tc := range cases {
		r := newRig(t)
		tn := r.tenant(t, "t")
		d := r.verifiedDomain(t, tn.ID, "example.com")
		r.activate(t, tn.ID, d)
		tc.break_(r, tn.ID, d.ID)
		out, signed, err := r.svc.SignMessage(r.ctx, tn.ID, "example.com", msg(r))
		var se *SignError
		if !errors.As(err, &se) || se.Outcome != tc.outcome || signed || out != nil {
			t.Fatalf("%s: outcome=%v signed=%v out=%v err=%v", name, se, signed, out != nil, err)
		}
		if r.obs.last() != "rsa-sha256/"+tc.outcome {
			t.Fatalf("%s: observer %q", name, r.obs.last())
		}
		if strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), "postgres") {
			t.Fatalf("%s: error leaks detail: %v", name, err)
		}
	}
}

// A key can only sign for the domain it belongs to; concurrent signing across
// domains and tenants never crosses keys.
func TestConcurrentSigningNeverCrossesKeysAcrossDomainsAndTenants(t *testing.T) {
	r := newRig(t)
	type site struct {
		tenant string
		domain database.Domain
		key    database.DKIMKey
	}
	var sites []site
	for i := 0; i < 3; i++ {
		tn := r.tenant(t, fmt.Sprintf("tenant-%d", i))
		for j := 0; j < 2; j++ {
			d := r.verifiedDomain(t, tn.ID, fmt.Sprintf("d%d-%d.example.com", i, j))
			sites = append(sites, site{tn.ID, d, r.activate(t, tn.ID, d)})
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for round := 0; round < 12; round++ {
		for _, s := range sites {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, signed, err := r.svc.SignMessage(r.ctx, s.tenant, s.domain.Name, r.message(t, "a@"+s.domain.Name))
				if err != nil || !signed {
					errs <- fmt.Errorf("%s: signed=%v err=%v", s.domain.Name, signed, err)
					return
				}
				if err := verify(t, out, s.domain.Name, s.key.Selector, decodePublic(t, s.key.PublicKey)); err != nil {
					errs <- fmt.Errorf("%s: %v", s.domain.Name, err)
				}
				// Never verifies under another site's key.
				for _, o := range sites {
					if o.domain.ID != s.domain.ID && verify(t, out, s.domain.Name, s.key.Selector, decodePublic(t, o.key.PublicKey)) == nil {
						errs <- fmt.Errorf("%s verifies under %s's key", s.domain.Name, o.domain.Name)
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Wrong-domain request through the service is refused even with a valid key.
	if _, signed, err := r.svc.SignMessage(r.ctx, sites[0].tenant, sites[0].domain.Name, r.message(t, "a@"+sites[1].domain.Name)); signed || err == nil {
		t.Fatalf("message for another domain must not be signed: signed=%v err=%v", signed, err)
	}
}

func TestDomainDeletionDestroysKeysAndReclaimCannotInheritThem(t *testing.T) {
	r := newRig(t)
	a, b := r.tenant(t, "a"), r.tenant(t, "b")
	da := r.verifiedDomain(t, a.ID, "example.com")
	r.activate(t, a.ID, da)
	if _, signed, _ := r.svc.SignMessage(r.ctx, a.ID, "example.com", r.message(t, "x@example.com")); !signed {
		t.Fatal("precondition: A signs")
	}
	if err := r.db.DeleteDomain(r.ctx, a.ID, da.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = r.raw.QueryRow(r.ctx, `SELECT count(*) FROM dkim_keys WHERE domain_id = $1`, da.ID).Scan(&rows)
	if rows != 0 {
		t.Fatalf("deleted domain still has %d key rows (private ciphertext must not survive)", rows)
	}
	if _, signed, _ := r.svc.SignMessage(r.ctx, a.ID, "example.com", r.message(t, "x@example.com")); signed {
		t.Fatal("a deleted domain must stop signing")
	}
	// Tenant B claims and verifies the same name: nothing of A's is reachable.
	db2 := r.verifiedDomain(t, b.ID, "example.com")
	if _, keys, err := r.svc.Keys(r.ctx, b.ID, db2.ID); err != nil || len(keys) != 0 {
		t.Fatalf("reclaimed domain must start with no keys: %v %v", keys, err)
	}
	if out, signed, err := r.svc.SignMessage(r.ctx, b.ID, "example.com", r.message(t, "x@example.com")); signed || err != nil || !strings.HasPrefix(string(out), "From:") {
		t.Fatalf("B must not sign with A's old key: signed=%v err=%v", signed, err)
	}
	var total int
	_ = r.raw.QueryRow(r.ctx, `SELECT count(*) FROM dkim_keys`).Scan(&total)
	if total != 0 {
		t.Fatalf("no keys should exist anywhere, found %d", total)
	}
}

func TestGenerateKeyIsRandomAndSelectorsAreValidAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := newSelector(time.Now())
		if err != nil || !selectorPattern.MatchString(s) || len(s) > 63 {
			t.Fatalf("selector %q: %v", s, err)
		}
		seen[s] = true
	}
	if len(seen) < 45 {
		t.Fatalf("selectors collide too often: %d distinct of 50", len(seen))
	}
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	if a.N.Cmp(b.N) == 0 || a.D.Cmp(b.D) == 0 {
		t.Fatal("independently generated keys must differ")
	}
}
