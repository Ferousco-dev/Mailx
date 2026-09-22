package worker

import (
	"context"
	"time"

	"github.com/Ferousco-dev/mailx/internal/queue"
)

// memberDisabledHold is how long a job waits when its durably-assigned
// sending-pool member (or that member's pool) is disabled, and how long it
// waits when routing-enabled state cannot be read at all. It is NOT a
// delivery attempt: no SMTP occurs, nothing is recorded, retry counters do
// not move, and messages.sending_member_id is never changed — the job
// simply re-enters processOne later and, if the member has been
// re-enabled by then, resumes through that SAME member. A fixed bound
// (rather than 0 or an unbounded wait) is what keeps a disabled member
// from causing a hot retry loop.
const memberDisabledHold = 30 * time.Second

// holdIfMemberDisabled runs BEFORE any SMTP work (mirrors
// enforceSuppression's placement) for a message with a durable v0.39
// routing decision. It returns true when the job was released without
// attempting delivery — because the member (or its pool) is disabled, or
// because that state could not be determined — and false when the member
// is confirmed enabled and processing should continue normally.
//
// This is a kill switch, not failover: on disabled it always releases the
// SAME job to be reclaimed later: never reassigns sending_member_id,
// never selects a different member, never attempts SMTP through anything
// else. That is what keeps a disabled member from being evadable by
// silently rerouting around it.
func (p *Pool) holdIfMemberDisabled(ctx context.Context, c queue.Claim, memberID string) bool {
	// Checked first, and without consuming a retry attempt: a member the
	// database reports as enabled can still be missing from THIS process's
	// routing registry (built once at startup — see routing.Router). Letting
	// that reach the coordinator would return a real delivery error, which
	// counts as an attempt and eventually exhausts the message permanently
	// instead of holding it for a restart to fix.
	if registry, ok := p.outcomes.(MemberRegistryGate); ok && !registry.MemberKnownLocally(memberID) {
		p.log.Warn("member_unknown_to_local_registry", "job_id", c.Job.ID, "member_id", memberID)
		p.release(c, p.now().Add(memberDisabledHold))
		return true
	}

	gate, ok := p.outcomes.(MemberRoutingGate)
	if !ok {
		return false
	}
	enabled, err := gate.MemberRoutingEnabled(ctx, memberID)
	if err != nil {
		p.log.Warn("member_routing_check_failed", "job_id", c.Job.ID, "member_id", memberID, "error", err.Error())
		p.release(c, p.now().Add(memberDisabledHold))
		return true
	}
	if !enabled {
		p.log.Info("member_routing_held", "job_id", c.Job.ID, "member_id", memberID)
		p.release(c, p.now().Add(memberDisabledHold))
		return true
	}
	return false
}
