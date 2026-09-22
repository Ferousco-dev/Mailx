package routing

import (
	"context"
	"fmt"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

// deliverer is the narrow shape Router dispatches to — matches
// retry.Deliverer/delivery.Engine structurally without importing
// internal/retry, keeping this package a sibling of delivery/retry rather
// than a dependent of either.
type deliverer interface {
	Deliver(context.Context, delivery.Request) (delivery.Result, error)
}

// MemberRoute is one pool member's process-local dispatch target: which
// Deliverer handles it, plus the non-secret identity Router snapshots onto
// delivery.Result for the delivery_attempts historical record. Kind is
// "direct" or "relay", matching database.SendingPoolMemberKind.
type MemberRoute struct {
	Deliver  deliverer
	Kind     string
	Hostname string
	SourceIP string
}

// Router is a process-local (built once at startup from current pool/member
// config — see cmd/mailx/serve.go) Deliverer that dispatches by
// Request.MemberID. It is itself a plain Deliverer, so it drops into the
// existing single-Coordinator/single-Deliverer worker wiring unchanged.
//
// Router never fails a delivery over routing/config trouble: an empty
// MemberID, or a MemberID no longer present in the registry (deleted —
// though v0.39 doesn't support deletion — or built from stale config),
// silently falls back to Base. This is deliberate: routing is best-effort
// infrastructure selection, never a reason to block mail. The v0.39
// disabled-member KILL SWITCH is enforced earlier, in
// worker.holdIfMemberDisabled, before Router is ever reached for that
// message — Router itself does not consult enabled state.
type Router struct {
	Base    deliverer
	Members map[string]MemberRoute
}

// NewRouter builds a Router. base is the legacy pre-v0.39 Deliverer (used
// for MemberID == "" and as the fallback for an unrecognized MemberID);
// members maps member id to its dispatch target.
func NewRouter(base deliverer, members map[string]MemberRoute) (*Router, error) {
	if base == nil {
		return nil, fmt.Errorf("routing: base deliverer must not be nil")
	}
	if members == nil {
		members = map[string]MemberRoute{}
	}
	return &Router{Base: base, Members: members}, nil
}

func (r *Router) Deliver(ctx context.Context, req delivery.Request) (delivery.Result, error) {
	if req.MemberID == "" {
		return r.Base.Deliver(ctx, req)
	}
	route, ok := r.Members[req.MemberID]
	if !ok {
		return r.Base.Deliver(ctx, req)
	}
	res, err := route.Deliver.Deliver(ctx, req)
	res.EffectiveHostname = route.Hostname
	res.EffectiveSourceIP = route.SourceIP
	res.EffectiveMemberID = req.MemberID
	return res, err
}
