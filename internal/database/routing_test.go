package database

import (
	"context"
	"errors"
	"testing"
)

func poolFixture(t *testing.T, db *DB, name string) SendingPool {
	t.Helper()
	p, err := db.CreateSendingPool(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func directMemberFixture(t *testing.T, db *DB, poolID, hostname string) SendingPoolMember {
	t.Helper()
	m, err := db.CreateSendingPoolMember(context.Background(), poolID, SendingPoolMemberDirect, strPtr(hostname), strPtr("203.0.113.10"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func messageSendingMemberID(t *testing.T, db *DB, messageID string) *string {
	t.Helper()
	var id *string
	if err := db.pool.QueryRow(context.Background(), `SELECT sending_member_id FROM messages WHERE id = $1`, messageID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreateSendingPoolAndMemberRoundTrip(t *testing.T) {
	db := newTestDB(t)
	p := poolFixture(t, db, "pool-a")
	if !p.Enabled {
		t.Fatal("new pool must default to enabled")
	}
	m := directMemberFixture(t, db, p.ID, "mx1.example.com")
	if !m.Enabled || m.Kind != SendingPoolMemberDirect || *m.Hostname != "mx1.example.com" {
		t.Fatalf("unexpected member: %+v", m)
	}
	list, err := db.ListSendingPoolMembers(context.Background(), p.ID)
	if err != nil || len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("list members: %+v %v", list, err)
	}
}

func TestCreateSendingPoolMemberRelayRejectsIdentity(t *testing.T) {
	db := newTestDB(t)
	p := poolFixture(t, db, "pool-relay")
	if _, err := db.CreateSendingPoolMember(context.Background(), p.ID, SendingPoolMemberRelay, strPtr("should-not-be-set.example.com"), nil); err == nil {
		t.Fatal("expected the DB check constraint to reject a relay member with a hostname")
	}
	m, err := db.CreateSendingPoolMember(context.Background(), p.ID, SendingPoolMemberRelay, nil, nil)
	if err != nil {
		t.Fatalf("relay member with no identity must be accepted: %v", err)
	}
	if m.Hostname != nil || m.SourceIP != nil {
		t.Fatalf("relay member must carry no identity: %+v", m)
	}
}

func TestInsertMessageAssignsRoutingMemberWhenDomainPoolConfigured(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	p := poolFixture(t, db, "pool-assigned")
	m := directMemberFixture(t, db, p.ID, "mx1.example.com")
	if err := db.AssignDomainPool(context.Background(), d.ID, &p.ID); err != nil {
		t.Fatal(err)
	}

	msg := sampleNewMessage(t, tn.ID)
	msg.SenderDomain = "example.com"
	got, err := db.InsertMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	sendingMemberID := messageSendingMemberID(t, db, got.ID)
	if sendingMemberID == nil || *sendingMemberID != m.ID {
		t.Fatalf("expected message routed to the sole enabled member %s, got %v", m.ID, sendingMemberID)
	}
}

func TestInsertMessageLeavesRoutingNilWithoutDomainPool(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	_ = d // domain verified, but never assigned a sending pool

	msg := sampleNewMessage(t, tn.ID)
	msg.SenderDomain = "example.com"
	got, err := db.InsertMessage(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if id := messageSendingMemberID(t, db, got.ID); id != nil {
		t.Fatalf("expected nil sending_member_id on the legacy no-pool path, got %v", *id)
	}
}

func TestInsertMessageLeavesRoutingNilWhenPoolDisabled(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	p := poolFixture(t, db, "pool-disabled")
	directMemberFixture(t, db, p.ID, "mx1.example.com")
	if err := db.AssignDomainPool(context.Background(), d.ID, &p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSendingPoolEnabled(context.Background(), p.ID, false); err != nil {
		t.Fatal(err)
	}

	msg := sampleNewMessage(t, tn.ID)
	msg.SenderDomain = "example.com"
	got, err := db.InsertMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("a disabled pool must degrade to legacy routing, not fail acceptance: %v", err)
	}
	if id := messageSendingMemberID(t, db, got.ID); id != nil {
		t.Fatalf("expected nil sending_member_id for a disabled pool, got %v", *id)
	}
}

func TestInsertMessageLeavesRoutingNilWhenNoEnabledMembers(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	p := poolFixture(t, db, "pool-empty")
	m := directMemberFixture(t, db, p.ID, "mx1.example.com")
	if err := db.SetSendingPoolMemberEnabled(context.Background(), m.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.AssignDomainPool(context.Background(), d.ID, &p.ID); err != nil {
		t.Fatal(err)
	}

	msg := sampleNewMessage(t, tn.ID)
	msg.SenderDomain = "example.com"
	got, err := db.InsertMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("a pool with no enabled members must degrade to legacy routing, not fail acceptance: %v", err)
	}
	if id := messageSendingMemberID(t, db, got.ID); id != nil {
		t.Fatalf("expected nil sending_member_id when no member is enabled, got %v", *id)
	}
}

func TestMemberRoutingEnabledReflectsBothPoolAndMemberFlags(t *testing.T) {
	db := newTestDB(t)
	p := poolFixture(t, db, "pool-flags")
	m := directMemberFixture(t, db, p.ID, "mx1.example.com")
	ctx := context.Background()

	if enabled, err := db.MemberRoutingEnabled(ctx, m.ID); err != nil || !enabled {
		t.Fatalf("expected enabled=true with both pool and member enabled: %v %v", enabled, err)
	}

	if err := db.SetSendingPoolMemberEnabled(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := db.MemberRoutingEnabled(ctx, m.ID); err != nil || enabled {
		t.Fatalf("expected enabled=false once the member itself is disabled: %v %v", enabled, err)
	}
	if err := db.SetSendingPoolMemberEnabled(ctx, m.ID, true); err != nil {
		t.Fatal(err)
	}

	if err := db.SetSendingPoolEnabled(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := db.MemberRoutingEnabled(ctx, m.ID); err != nil || enabled {
		t.Fatalf("expected enabled=false once the POOL is disabled even though the member itself is enabled: %v %v", enabled, err)
	}
}

func TestSetSendingPoolEnabledNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSendingPoolEnabled(context.Background(), "does-not-exist", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSetSendingPoolMemberEnabledNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSendingPoolMemberEnabled(context.Background(), "does-not-exist", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAssignDomainPoolSetAndClear(t *testing.T) {
	db := newTestDB(t)
	tn := newTestTenant(t, db)
	d := dkimFixture(t, db, tn.ID, "example.com")
	p := poolFixture(t, db, "pool-assign")

	if err := db.AssignDomainPool(context.Background(), d.ID, &p.ID); err != nil {
		t.Fatal(err)
	}
	var got *string
	if err := db.pool.QueryRow(context.Background(), `SELECT sending_pool_id FROM domains WHERE id = $1`, d.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != p.ID {
		t.Fatalf("expected domain assigned to pool %s, got %v", p.ID, got)
	}

	if err := db.AssignDomainPool(context.Background(), d.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(context.Background(), `SELECT sending_pool_id FROM domains WHERE id = $1`, d.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected sending_pool_id cleared, got %v", *got)
	}
}
