package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func addMember(t *testing.T, db *DB, tenantID, email, role string) Human {
	t.Helper()
	h, err := db.CreateHuman(context.Background(), "M", email, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(context.Background(),
		`INSERT INTO tenant_members (tenant_id, human_id, role) VALUES ($1, $2, $3)`, tenantID, h.ID, role); err != nil {
		t.Fatal(err)
	}
	return h
}

func countOwners(t *testing.T, db *DB, tenantID string) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_members WHERE tenant_id = $1 AND role = 'owner'`, tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRemoveTenantMemberRules(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	owner, tn := newOwnedOrg(t, db, "owner@example.com")
	m1 := addMember(t, db, tn.ID, "m1@example.com", "member")
	m2 := addMember(t, db, tn.ID, "m2@example.com", "member")
	outsider := addMember(t, db, newTestTenant(t, db).ID, "x@example.com", "owner")

	cases := []struct {
		name          string
		actor, target string
		want          error
	}{
		{"self", owner.ID, owner.ID, ErrCannotRemoveSelf},
		{"non-owner actor", m1.ID, m2.ID, ErrNotTenantOwner},
		{"target not a member", owner.ID, outsider.ID, ErrNotFound},
		{"owner removes member", owner.ID, m1.ID, nil},
		{"already removed", owner.ID, m1.ID, ErrNotFound},
	}
	for _, tc := range cases {
		if err := db.RemoveTenantMember(ctx, tn.ID, tc.actor, tc.target); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err=%v want %v", tc.name, err, tc.want)
		}
	}
	if ok, _ := db.IsTenantMember(ctx, tn.ID, m1.ID); ok {
		t.Fatal("m1 still a member")
	}
	if ok, _ := db.IsTenantMember(ctx, tn.ID, m2.ID); !ok {
		t.Fatal("m2 wrongly removed")
	}
	if n := countOwners(t, db, tn.ID); n != 1 {
		t.Fatalf("owners=%d want 1", n)
	}
}

// Two owners removing each other at the same instant must never leave the
// org ownerless: the tenant row lock serializes them and the loser finds
// its own ownership gone. Repeated so the race is actually exercised.
func TestRemoveTenantMemberConcurrentMutualRemovalKeepsAnOwner(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		a, tn := newOwnedOrg(t, db, fmt.Sprintf("a%d@example.com", round))
		b := addMember(t, db, tn.ID, fmt.Sprintf("b%d@example.com", round), "owner")
		pairs := [][2]string{{a.ID, b.ID}, {b.ID, a.ID}}
		var wg sync.WaitGroup
		results := make(chan error, len(pairs))
		start := make(chan struct{})
		for _, p := range pairs {
			wg.Add(1)
			go func(actor, target string) {
				defer wg.Done()
				<-start
				results <- db.RemoveTenantMember(ctx, tn.ID, actor, target)
			}(p[0], p[1])
		}
		close(start)
		wg.Wait()
		close(results)
		ok, denied := 0, 0
		for err := range results {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrNotTenantOwner):
				denied++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if owners := countOwners(t, db, tn.ID); ok != 1 || denied != 1 || owners != 1 {
			t.Fatalf("round %d: ok=%d denied=%d owners=%d; want 1/1/1", round, ok, denied, owners)
		}
	}
}

func TestOrgInvitationListAndRevoke(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	owner, tn := newOwnedOrg(t, db, "owner@example.com")
	_, other := newOwnedOrg(t, db, "other@example.com")
	now := time.Now().UTC()
	pending, err := db.CreateOrgInvitation(ctx, tn.ID, owner.ID, "p@example.com", "h-p", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateOrgInvitation(ctx, tn.ID, owner.ID, "old@example.com", "h-old", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	accepted, err := db.CreateOrgInvitation(ctx, tn.ID, owner.ID, "acc@example.com", "h-acc", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptOrgInvitationWithSignup(ctx, accepted.ID, tn.ID, "A", "acc@example.com", "x", now); err != nil {
		t.Fatal(err)
	}

	list, err := db.ListPendingOrgInvitations(ctx, tn.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != pending.ID {
		t.Fatalf("pending list = %+v, want only %s", list, pending.ID)
	}
	if err := db.RevokeOrgInvitation(ctx, other.ID, pending.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant revoke: %v", err)
	}
	if err := db.RevokeOrgInvitation(ctx, tn.ID, "nope", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown revoke: %v", err)
	}
	for i := 0; i < 2; i++ { // second call is the idempotent no-op
		if err := db.RevokeOrgInvitation(ctx, tn.ID, pending.ID, now); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	if err := db.RevokeOrgInvitation(ctx, tn.ID, accepted.ID, now); err != nil {
		t.Fatalf("revoke accepted: %v", err)
	}
	if list, _ := db.ListPendingOrgInvitations(ctx, tn.ID, now); len(list) != 0 {
		t.Fatalf("still pending after revoke: %+v", list)
	}
	acc, err := db.GetOrgInvitationByHash(ctx, "h-acc")
	if err != nil || acc.AcceptedAt == nil || !acc.ExpiresAt.After(now) {
		t.Fatalf("accepted invite was modified: %+v %v", acc, err)
	}
}

func TestUpdateProfileAndOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	h, tn := newOwnedOrg(t, db, "o@example.com")
	name, url, empty := "New Name", "https://x.example/a.png", ""
	if err := db.UpdateHumanProfile(ctx, h.ID, &name, &url); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetHuman(ctx, h.ID)
	if got.Name != name || got.AvatarURL == nil || *got.AvatarURL != url || got.Email != "o@example.com" {
		t.Fatalf("profile = %+v", got)
	}
	if err := db.UpdateHumanProfile(ctx, h.ID, nil, &empty); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetHuman(ctx, h.ID); got.Name != name || got.AvatarURL != nil {
		t.Fatalf("clear avatar: %+v", got)
	}
	if err := db.UpdateOrganization(ctx, tn.ID, nil, &url); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetTenant(ctx, tn.ID); got.Name != tn.Name || got.LogoURL == nil || *got.LogoURL != url {
		t.Fatalf("org = %+v", got)
	}
	if err := db.UpdateOrganization(ctx, "missing", &name, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing org: %v", err)
	}
}
