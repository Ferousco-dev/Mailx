package routing

import (
	"context"
	"errors"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

type fakeDeliverer struct {
	name string
	err  error
}

func (f *fakeDeliverer) Deliver(context.Context, delivery.Request) (delivery.Result, error) {
	if f.err != nil {
		return delivery.Result{}, f.err
	}
	return delivery.Result{Domain: f.name, Accepted: true, Kind: delivery.KindAccepted}, nil
}

func TestRouterEmptyMemberIDUsesBase(t *testing.T) {
	base := &fakeDeliverer{name: "base"}
	r, err := NewRouter(base, map[string]MemberRoute{
		"m1": {Deliver: &fakeDeliverer{name: "m1"}, Kind: "direct", Hostname: "mx1.example.com", SourceIP: "203.0.113.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Deliver(context.Background(), delivery.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Domain != "base" {
		t.Fatalf("expected base deliverer, got %q", res.Domain)
	}
	if res.EffectiveHostname != "" || res.EffectiveSourceIP != "" {
		t.Fatalf("legacy path must not carry an effective identity, got %+v", res)
	}
}

func TestRouterKnownMemberIDDispatchesAndSnapshotsIdentity(t *testing.T) {
	base := &fakeDeliverer{name: "base"}
	r, err := NewRouter(base, map[string]MemberRoute{
		"m1": {Deliver: &fakeDeliverer{name: "m1"}, Kind: "direct", Hostname: "mx1.example.com", SourceIP: "203.0.113.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Deliver(context.Background(), delivery.Request{MemberID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Domain != "m1" {
		t.Fatalf("expected member deliverer, got %q", res.Domain)
	}
	if res.EffectiveHostname != "mx1.example.com" || res.EffectiveSourceIP != "203.0.113.5" {
		t.Fatalf("expected member identity snapshot, got %+v", res)
	}
}

func TestRouterKnown(t *testing.T) {
	base := &fakeDeliverer{name: "base"}
	r, err := NewRouter(base, map[string]MemberRoute{"m1": {Deliver: &fakeDeliverer{name: "m1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Known("m1") {
		t.Fatal("m1 was registered and must be reported known")
	}
	if r.Known("ghost") {
		t.Fatal("an unregistered member must be reported unknown")
	}
}

func TestRouterUnknownMemberIDNeverUsesBase(t *testing.T) {
	base := &fakeDeliverer{name: "base"}
	r, err := NewRouter(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Deliver(context.Background(), delivery.Request{MemberID: "ghost"})
	if !errors.Is(err, ErrUnknownMember) {
		t.Fatalf("expected ErrUnknownMember, got %v", err)
	}
	if res.Domain == "base" {
		t.Fatal("an unrecognized member must never silently dispatch through base — wrong source IP/identity, or a bypassed relay")
	}
	if res.Kind != delivery.KindTransferTemporary {
		t.Fatalf("expected a temporary/retryable result so the message is held, not failed, got %q", res.Kind)
	}
}

func TestRouterPropagatesDeliveryError(t *testing.T) {
	wantErr := errors.New("boom")
	base := &fakeDeliverer{name: "base"}
	r, err := NewRouter(base, map[string]MemberRoute{
		"m1": {Deliver: &fakeDeliverer{name: "m1", err: wantErr}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Deliver(context.Background(), delivery.Request{MemberID: "m1"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

func TestNewRouterRejectsNilBase(t *testing.T) {
	if _, err := NewRouter(nil, nil); err == nil {
		t.Fatal("expected error for nil base deliverer")
	}
}
