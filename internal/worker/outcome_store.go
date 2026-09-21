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
}

// OutcomeStore is the intentional durability boundary between SMTP work and
// queue finalization. Implementations must make attempt/status/event coherent
// in one durable operation.
type OutcomeStore interface {
	Load(context.Context, string) (DurableDeliveryState, error)
	Persist(context.Context, string, retry.DeliveryAttempt, retry.Outcome) error
}
