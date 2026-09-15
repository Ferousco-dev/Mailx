package database

import (
	"context"
	"testing"
)

// These are ordinary local-machine measurements against a local PostgreSQL
// instance over a Unix/loopback connection — not a claim about production
// throughput, network latency, or any specific deployment. Useful only for
// noticing a future regression, not for marketing numbers.

func BenchmarkInsertMessage(b *testing.B) {
	db := newTestDB(b)
	ctx := context.Background()
	tenant, err := db.CreateTenant(ctx, "bench-tenant")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, err := newID()
		if err != nil {
			b.Fatal(err)
		}
		_, err = db.InsertMessage(ctx, NewMessage{
			ID: id, TenantID: tenant.ID, MailFrom: "<a@x>",
			Recipients: []RecipientInput{{Address: "<b@y>"}},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListMessages(b *testing.B) {
	db := newTestDB(b)
	ctx := context.Background()
	tenant, err := db.CreateTenant(ctx, "bench-tenant")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		id, err := newID()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.InsertMessage(ctx, NewMessage{
			ID: id, TenantID: tenant.ID, MailFrom: "<a@x>",
			Recipients: []RecipientInput{{Address: "<b@y>"}},
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.ListMessages(ctx, tenant.ID, nil, 50, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetMessage(b *testing.B) {
	db := newTestDB(b)
	ctx := context.Background()
	tenant, err := db.CreateTenant(ctx, "bench-tenant")
	if err != nil {
		b.Fatal(err)
	}
	id, err := newID()
	if err != nil {
		b.Fatal(err)
	}
	msg, err := db.InsertMessage(ctx, NewMessage{
		ID: id, TenantID: tenant.ID, MailFrom: "<a@x>",
		Recipients: []RecipientInput{{Address: "<b@y>"}},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.GetMessage(ctx, tenant.ID, msg.ID); err != nil {
			b.Fatal(err)
		}
	}
}
