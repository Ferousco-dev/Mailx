package worker

import (
	"context"
	"errors"
	"time"

	"github.com/Ferousco-dev/mailx/internal/retry"
)

// ErrOutcomeAlreadyTerminal tells the worker that another process has already
// made terminal delivery truth durable. The current queue claim may be Acked,
// but SMTP must not be attempted again.
var ErrOutcomeAlreadyTerminal = errors.New("worker: delivery outcome already terminal")

// DurableDeliveryState is loaded before SMTP so a reclaimed queue job cannot
// knowingly retransmit a message whose terminal outcome is already durable.
type DurableDeliveryState struct {
	RetryState  *retry.State
	Terminal    bool
	NextRetryAt *time.Time
	// SendingMemberID is the message's durable v0.39 sending-pool routing
	// decision, nil on the legacy no-pool path. See MemberRoutingHold.
	SendingMemberID *string
}

// OutcomeStore is the intentional durability boundary between SMTP work and
// queue finalization. Implementations must make attempt/status/event coherent
// in one durable operation.
type OutcomeStore interface {
	Load(context.Context, string) (DurableDeliveryState, error)
	Persist(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error
}

// MemberRoutingGate is implemented by an OutcomeStore that also enforces the
// v0.39 sending-pool member/pool kill switch. Stores that don't implement it
// (e.g. tests with no routing concept) simply never hold on a disabled
// member — matching pre-v0.39 behavior.
type MemberRoutingGate interface {
	// MemberRoutingEnabled reports whether memberID (and its pool) is
	// currently enabled for a NEW SMTP attempt. It never influences which
	// member a message is routed to; only whether the already-decided
	// member may be used right now.
	MemberRoutingEnabled(ctx context.Context, memberID string) (bool, error)
}
