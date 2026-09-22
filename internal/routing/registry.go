package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

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
// An empty MemberID always uses Base (the legacy no-pool path). A MemberID
// the registry does not recognize (an enabled member created, or a
// hostname fixed, after this process started — the registry is built once
// at startup and only refreshes on restart) is NEVER silently sent through
// Base: doing so would dial with the wrong source IP/EHLO identity, or
// bypass a relay the operator intended, without any record of it. Instead
// Deliver returns a temporary delivery error, so the existing retry/
// backoff machinery holds the message (same "never silently reroute"
// posture as worker.holdIfMemberDisabled) until an operator restarts the
// process to pick up the current pool/member configuration.
type Router struct {
	Base    deliverer
	Members map[string]MemberRoute
}

// ErrUnknownMember is wrapped into the *delivery.Error Deliver returns for
// a MemberID not present in this process's registry.
var ErrUnknownMember = errors.New("routing: sending pool member is not in this process's registry (restart pending?)")

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
		now := time.Now().UTC()
		return delivery.Result{Domain: req.Domain, StartedAt: now, FinishedAt: now, Kind: delivery.KindTransferTemporary},
			&delivery.Error{Kind: delivery.KindTransferTemporary, Domain: req.Domain, Temporary: true, Err: ErrUnknownMember}
	}
	res, err := route.Deliver.Deliver(ctx, req)
	res.EffectiveHostname = route.Hostname
	res.EffectiveSourceIP = route.SourceIP
	res.EffectiveMemberID = req.MemberID
	return res, err
}
