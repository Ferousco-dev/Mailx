package retry

import (
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

// A delivery that failed only because TLS could not be established surfaces as
// KindTransferTemporary, which the existing policy retries; there is no
// separate TLS retry system.
func TestTLSFailureKindIsRetriedByExistingPolicy(t *testing.T) {
	res := delivery.Result{Kind: delivery.KindTransferTemporary}
	err := &delivery.Error{Kind: delivery.KindTransferTemporary, Temporary: true}
	if Decide(res, err) != Retry {
		t.Fatal("temporary transfer failure (TLS) must be retried")
	}
	// Acceptance still wins over any later error, TLS or otherwise.
	res.Accepted = true
	if Decide(res, err) != TerminalSuccess {
		t.Fatal("Accepted=true must never be retried")
	}
}
