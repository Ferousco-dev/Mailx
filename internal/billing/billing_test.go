package billing

import (
	"errors"
	"testing"
)

func TestPlanTable(t *testing.T) {
	cases := []struct {
		id                                 string
		price                              int64
		daily, domains, members, retention int
		broadcasts, webhooks               bool
	}{
		{"free", 0, 500, 5, 1, 7, false, false},
		{"plus", 600, 10_000, 15, 5, 30, true, true},
		{"pro", 2400, 100_000, Unlimited, Unlimited, 90, true, true},
	}
	for _, c := range cases {
		p := PlanFor(c.id)
		if p.ID != c.id || p.PriceUSDCents != c.price || p.DailySends != c.daily || p.Domains != c.domains ||
			p.Members != c.members || p.RetentionDays != c.retention || p.Broadcasts != c.broadcasts || p.Webhooks != c.webhooks {
			t.Fatalf("%s: got %+v", c.id, p)
		}
	}
	for _, id := range []string{"", "enterprise", "FREE"} {
		if PlanFor(id).ID != PlanFree {
			t.Fatalf("PlanFor(%q) should default to free", id)
		}
	}
	if IsPaid("free") || !IsPaid("plus") || !IsPaid("pro") || IsPaid("bogus") {
		t.Fatal("IsPaid wrong")
	}
	if !Within(4, 5) || Within(5, 5) || !Within(1_000_000, Unlimited) {
		t.Fatal("Within wrong")
	}
}

func TestVerifySignature(t *testing.T) {
	p, err := NewPaystack("sk_test_x", "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"charge.success"}`)
	if err := p.VerifySignature(body, p.Sign(body)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	other, _ := NewPaystack("sk_test_y", "")
	for name, sig := range map[string]string{
		"empty":         "",
		"garbage":       "not-hex",
		"short":         "abcd",
		"other key":     other.Sign(body),
		"tampered body": p.Sign([]byte(`{"event":"charge.failed"}`)),
	} {
		if err := p.VerifySignature(body, sig); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("%s: want ErrInvalidSignature, got %v", name, err)
		}
	}
	if _, err := NewPaystack("  ", ""); err == nil {
		t.Fatal("empty secret must be refused")
	}
}

// TestParseEventToleratesNonObjectMetadata is a regression test for a
// CodeRabbit finding (PR #24): Paystack sends non-object metadata (e.g. 0
// or "") for a transaction MailX's checkout never created (a payment page
// or another integration on the same account). That must decode to an
// empty Metadata, not fail ParseEvent - handleWebhook must still be able
// to acknowledge such an event 200 rather than answering 400 and making
// Paystack retry an event that can never become recognizable.
func TestParseEventToleratesNonObjectMetadata(t *testing.T) {
	for name, body := range map[string]string{
		"integer metadata": `{"event":"charge.success","data":{"metadata":0}}`,
		"string metadata":  `{"event":"charge.success","data":{"metadata":""}}`,
		"null metadata":    `{"event":"charge.success","data":{"metadata":null}}`,
		"array metadata":   `{"event":"charge.success","data":{"metadata":[]}}`,
	} {
		ev, err := ParseEvent([]byte(body))
		if err != nil {
			t.Fatalf("%s: expected no error, got %v", name, err)
		}
		if ev.Data.Metadata != (Metadata{}) {
			t.Fatalf("%s: expected empty Metadata, got %+v", name, ev.Data.Metadata)
		}
	}
	// A genuine object still decodes normally.
	ev, err := ParseEvent([]byte(`{"event":"charge.success","data":{"metadata":{"tenant_id":"t1","plan":"plus"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Data.Metadata != (Metadata{TenantID: "t1", Plan: "plus"}) {
		t.Fatalf("expected real metadata to decode, got %+v", ev.Data.Metadata)
	}
}
