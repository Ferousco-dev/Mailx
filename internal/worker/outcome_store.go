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

// MemberRegistryGate is implemented by an OutcomeStore that also knows
// whether memberID is present in THIS process's routing registry (built
// once at startup from pool/member config — see internal/routing.Router).
// A member the database reports as enabled can still be locally unknown
// (created, or given a hostname, after this process started); dispatching
// it through routing.Router in that state returns a delivery error that
// would consume a real retry attempt and eventually fail the message
// permanently. holdIfMemberDisabled checks this FIRST, before any SMTP
// work, so that case holds/retries for free (no attempt recorded) instead,
// exactly like a disabled member — see MemberRoutingGate's doc.
type MemberRegistryGate interface {
	MemberKnownLocally(memberID string) bool
}
