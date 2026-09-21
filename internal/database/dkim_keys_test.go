package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func dkimFixture(t *testing.T, db *DB, tenantID, name string) Domain {
	t.Helper()
	ctx := context.Background()
	d, err := db.CreateDomain(ctx, tenantID, name, "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	d, err = db.RecordDomainCheck(ctx, tenantID, d.ID, true, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newKey(sel string) NewDKIMKey {
	return NewDKIMKey{Selector: sel, Algorithm: "rsa-sha256", KeyBits: 2048, PublicKey: "cHVibGlj",
		PrivateCiphertext: []byte("cipher-" + sel), PrivateNonce: []byte("123456789012")}
}

func TestDKIMSchemaConstraintsEnforceLifecycleAndStrength(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	bad := map[string]string{
		"weak key size":       `INSERT INTO dkim_keys (id,tenant_id,domain_id,selector,algorithm,key_bits,public_key,private_ciphertext,private_nonce,status) VALUES ('w','%s','%s','s1','rsa-sha256',1024,'p','\x01','\x02','pending')`,
		"sha1 algorithm":      `INSERT INTO dkim_keys (id,tenant_id,domain_id,selector,algorithm,key_bits,public_key,private_ciphertext,private_nonce,status) VALUES ('w','%s','%s','s1','rsa-sha1',2048,'p','\x01','\x02','pending')`,
		"bad selector":        `INSERT INTO dkim_keys (id,tenant_id,domain_id,selector,algorithm,key_bits,public_key,private_ciphertext,private_nonce,status) VALUES ('w','%s','%s','Bad_Sel','rsa-sha256',2048,'p','\x01','\x02','pending')`,
		"pending without key": `INSERT INTO dkim_keys (id,tenant_id,domain_id,selector,algorithm,key_bits,public_key,status) VALUES ('w','%s','%s','s1','rsa-sha256',2048,'p','pending')`,
		"retired keeps key":   `INSERT INTO dkim_keys (id,tenant_id,domain_id,selector,algorithm,key_bits,public_key,private_ciphertext,private_nonce,status,retired_at) VALUES ('w','%s','%s','s1','rsa-sha256',2048,'p','\x01','\x02','retired',now())`,
		"tenant mismatch":     `INSERT INTO dkim_keys (id,tenant_id,domain_id,selector,algorithm,key_bits,public_key,private_ciphertext,private_nonce,status) VALUES ('w','no-such-tenant','%s','s1','rsa-sha256',2048,'p','\x01','\x02','pending')`,
	}
	for name, q := range bad {
		var err error
		if strings.Count(q, "%s") == 2 {
			_, err = db.pool.Exec(ctx, fmt.Sprintf(q, tn.ID, d.ID))
		} else {
			_, err = db.pool.Exec(ctx, fmt.Sprintf(q, d.ID))
		}
		if err == nil {
			t.Fatalf("%s must be rejected by the schema", name)
		}
	}
}

func TestDKIMKeyOperationsAreTenantScopedAndSerialized(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	da := dkimFixture(t, db, a.ID, "example.com")

	if _, err := db.CreateDKIMKey(ctx, b.ID, da.ID, newKey("s1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant create: %v", err)
	}
	pending, _ := db.CreateDomain(ctx, a.ID, "pending-domain.com", "tok")
	if _, err := db.CreateDKIMKey(ctx, a.ID, pending.ID, newKey("s1")); !errors.Is(err, ErrDomainNotVerified) {
		t.Fatalf("unverified domain: %v", err)
	}

	// Concurrent creation: exactly one pending key.
	var wg sync.WaitGroup
	res := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.CreateDKIMKey(ctx, a.ID, da.ID, newKey(fmt.Sprintf("s%d", i)))
			res <- err
		}()
	}
	wg.Wait()
	close(res)
	created, conflicts := 0, 0
	for err := range res {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrDKIMKeyPending):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if created != 1 || conflicts != 9 {
		t.Fatalf("created=%d conflicts=%d", created, conflicts)
	}
	if keys, err := db.ListDKIMKeys(ctx, b.ID, da.ID); err != nil || len(keys) != 0 {
		t.Fatalf("cross-tenant list must be empty: %v %v", keys, err)
	}
	if _, err := db.ActivateDKIMKey(ctx, b.ID, da.ID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant activate: %v", err)
	}
	// Signing lookup requires tenant + verified + not deleted.
	if _, err := db.ActiveSigningKey(ctx, a.ID, "example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending key must not be a signing key: %v", err)
	}
	if _, err := db.ActivateDKIMKey(ctx, a.ID, da.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	k, err := db.ActiveSigningKey(ctx, a.ID, "example.com")
	if err != nil || k.Status != DKIMActive || len(k.PrivateCiphertext) == 0 {
		t.Fatalf("%+v %v", k, err)
	}
	if _, err := db.ActiveSigningKey(ctx, b.ID, "example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B must not obtain tenant A's key: %v", err)
	}
	if list, _ := db.ListDKIMKeys(ctx, a.ID, da.ID); len(list) != 1 || len(list[0].PrivateCiphertext) != 0 {
		t.Fatalf("list must not expose private material: %+v", list)
	}
}

func TestSenderAuthorizationQueryAndInsertMessageRecheck(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, b := newTestTenant(t, db), newTestTenant(t, db)
	d := dkimFixture(t, db, a.ID, "example.com")
	if _, err := db.VerifiedSenderDomain(ctx, a.ID, "example.com"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tenant, name string }{{b.ID, "example.com"}, {a.ID, "sub.example.com"}, {a.ID, "EXAMPLE.COM"}, {a.ID, "example.com."}} {
		if _, err := db.VerifiedSenderDomain(ctx, c.tenant, c.name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v must not be authorized (exact canonical match only): %v", c, err)
		}
	}
	// InsertMessage re-checks authorization in its own transaction.
	m := sampleNewMessage(t, a.ID)
	m.SenderDomain = "example.com"
	if _, err := db.InsertMessage(ctx, m); err != nil {
		t.Fatalf("authorized sender: %v", err)
	}
	if err := db.DeleteDomain(ctx, a.ID, d.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	m2 := sampleNewMessage(t, a.ID)
	m2.SenderDomain = "example.com"
	if _, err := db.InsertMessage(ctx, m2); !errors.Is(err, ErrSenderNotAuthorized) {
		t.Fatalf("deleted domain must fail the in-transaction re-check: %v", err)
	}
	var count int
	_ = db.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE id = $1`, m2.ID).Scan(&count)
	if count != 0 {
		t.Fatal("a rejected message must leave no row")
	}
}

func TestDKIMHotQueriesUseIndexes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	// Seed enough rows that a sequential scan would be the wrong plan: 6000
	// verified domains, each with an active key, under one tenant.
	tn := newTestTenant(t, db)
	if _, err := db.pool.Exec(ctx, `INSERT INTO domains (id, tenant_id, name, verification_status, verification_token, verified_at)
		SELECT 'bd'||g, $1, 'bulk'||g||'.example.com', 'verified', 't', now() FROM generate_series(1, 6000) g`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO dkim_keys (id, tenant_id, domain_id, selector, algorithm, key_bits, public_key,
			private_ciphertext, private_nonce, status, activated_at)
		SELECT 'bk'||g, $1, 'bd'||g, 'bulk'||g, 'rsa-sha256', 2048, 'p', '\x01', '\x02', 'active', now() FROM generate_series(1, 6000) g`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ANALYZE domains; ANALYZE dkim_keys`); err != nil {
		t.Fatal(err)
	}
	plan := explainAnalyze(t, db, `SELECT k.id FROM domains d JOIN dkim_keys k ON k.domain_id = d.id AND k.tenant_id = d.tenant_id
		WHERE d.tenant_id = $1 AND d.name = $2 AND d.deleted_at IS NULL AND d.verification_status = 'verified' AND k.status = 'active'`, tn.ID, "bulk4242.example.com")
	if strings.Contains(plan, "Seq Scan on domains") {
		t.Fatalf("signing lookup scanned domains sequentially:\n%s", plan)
	}
	if !strings.Contains(plan, "Index Scan using uq_domains_") || !strings.Contains(plan, "uq_dkim_one_active") {
		t.Fatalf("signing lookup should use a name-lookup index on domains and uq_dkim_one_active:\n%s", plan)
	}
	plan = explainAnalyze(t, db, `SELECT id FROM domains WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL AND verification_status = 'verified'`, tn.ID, "bulk4242.example.com")
	if !strings.Contains(plan, "Index Scan using uq_domains_") {
		t.Fatalf("sender authorization should use a name-lookup index on domains:\n%s", plan)
	}
	t.Logf("sender authorization plan:\n%s", plan)
}

func TestDKIMMigrationUpgradesExistingDomainData(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	// Downgrade just this migration, then upgrade: domain data survives, table returns empty.
	if err := db.MigrateDownOne(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	_ = db.pool.QueryRow(ctx, `SELECT to_regclass('dkim_keys') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("down migration must drop dkim_keys")
	}
	if got, err := db.GetDomain(ctx, tn.ID, d.ID); err != nil || got.Name != "example.com" {
		t.Fatalf("domain data must survive the downgrade: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = db.pool.QueryRow(ctx, `SELECT to_regclass('dkim_keys') IS NOT NULL`).Scan(&exists)
	if !exists {
		t.Fatal("up migration must create dkim_keys")
	}
	if _, err := db.CreateDKIMKey(ctx, tn.ID, d.ID, newKey("s1")); err != nil {
		t.Fatalf("keys must be creatable for pre-existing domains: %v", err)
	}
}
