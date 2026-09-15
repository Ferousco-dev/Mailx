package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestConcurrentAuthenticationAcrossTenants stresses many goroutines
// authenticating many keys across many tenants at once — proving no
// shared unsynchronized state and no cross-tenant bleed under race.
func TestConcurrentAuthenticationAcrossTenants(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	svc := NewService(db, []byte("test-pepper"))

	const tenants = 5
	const keysPerTenant = 4
	type entry struct {
		raw      string
		tenantID string
	}
	var entries []entry
	for i := 0; i < tenants; i++ {
		tenant, err := db.CreateTenant(ctx, "tenant")
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < keysPerTenant; j++ {
			gen, _, err := svc.Create(ctx, tenant.ID, "k", []string{string(ScopeEmailsSend)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry{raw: gen.Raw, tenantID: tenant.ID})
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(entries)*10)
	for round := 0; round < 10; round++ {
		for _, e := range entries {
			wg.Add(1)
			go func(e entry) {
				defer wg.Done()
				record, err := svc.Authenticate(ctx, e.raw)
				if err != nil {
					errs <- err
					return
				}
				if record.TenantID != e.tenantID {
					errs <- errors.New("cross-tenant bleed: got wrong tenant for a key")
				}
			}(e)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestConcurrentRevokeAndAuthenticateRace documents the revocation-race
// boundary from the v0.19 spec: revocation prevents NEW authentication
// after it durably commits; it is not expected to retroactively cancel a
// request that already completed authentication. This test just proves
// nothing panics/corrupts state under the race and that authentication
// reliably rejects the key once revocation has definitely completed.
func TestConcurrentRevokeAndAuthenticateRace(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	tenant := newTestTenant(t, db)
	svc := NewService(db, nil)
	gen, record, err := svc.Create(ctx, tenant.ID, "x", []string{string(ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Authenticate(ctx, gen.Raw)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		_ = svc.Revoke(ctx, record.KeyID)
	}()
	wg.Wait()

	if _, err := svc.Authenticate(ctx, gen.Raw); !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected the key to be reliably revoked after the race settles, got %v", err)
	}
}
