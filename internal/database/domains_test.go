package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDomainLifecycleAndTenantIsolation(t *testing.T) {
	db := newTestDB(t)
	a := newTestTenant(t, db)
	b, err := db.CreateTenant(context.Background(), "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	da, err := db.CreateDomain(context.Background(), a.ID, "example.com", "token-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateDomain(context.Background(), a.ID, "example.com", "duplicate"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same-tenant duplicate error = %v", err)
	}
	dbDomain, err := db.CreateDomain(context.Background(), b.ID, "example.com", "token-b")
	if err != nil {
		t.Fatalf("another tenant's pending claim must be allowed: %v", err)
	}
	if _, err := db.GetDomain(context.Background(), b.ID, da.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant get error = %v", err)
	}
	checked := time.Now().UTC()
	if _, err := db.RecordDomainCheck(context.Background(), b.ID, dbDomain.ID, true, checked); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordDomainCheck(context.Background(), a.ID, da.ID, true, checked); !errors.Is(err, ErrConflict) {
		t.Fatalf("second verified owner error = %v", err)
	}
	if err := db.DeleteDomain(context.Background(), b.ID, dbDomain.ID, checked.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetDomain(context.Background(), b.ID, dbDomain.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted domain remained visible: %v", err)
	}
	if _, err := db.RecordDomainCheck(context.Background(), a.ID, da.ID, true, checked.Add(2*time.Second)); err != nil {
		t.Fatalf("deletion should release verified ownership: %v", err)
	}
}

func TestConcurrentDomainCreateAndVerify(t *testing.T) {
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	const n = 20
	var wg sync.WaitGroup
	ids := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := db.CreateDomain(context.Background(), tenant.ID, "example.com", "token")
			if err == nil {
				ids <- d.ID
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected create error: %v", err)
			}
		}()
	}
	wg.Wait()
	close(ids)
	var id string
	for created := range ids {
		if id != "" {
			t.Fatal("more than one concurrent create succeeded")
		}
		id = created
	}
	if id == "" {
		t.Fatal("no create succeeded")
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.RecordDomainCheck(context.Background(), tenant.ID, id, true, time.Now().UTC()); err != nil {
				t.Errorf("concurrent verify: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := db.GetDomain(context.Background(), tenant.ID, id)
	if err != nil || got.VerificationStatus != DomainVerified || got.VerifiedAt == nil {
		t.Fatalf("inconsistent verified state: %+v, %v", got, err)
	}
}
