package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
)

func TestDecide(t *testing.T) {
	tests := []struct {
		name   string
		result delivery.Result
		err    error
		want   Decision
	}{
		{
			name:   "accepted delivery",
			result: delivery.Result{Kind: delivery.KindAccepted, Accepted: true},
			want:   TerminalSuccess,
		},
		{
			name: "accepted delivery with QUIT error",
			result: delivery.Result{
				Kind:      delivery.KindAccepted,
				Accepted:  true,
				QuitError: "connection reset during QUIT",
			},
			want: TerminalSuccess,
		},
		{
			name:   "temporary DNS failure",
			result: delivery.Result{Kind: delivery.KindDNSTemporary},
			want:   Retry,
		},
		{
			name:   "temporary SMTP delivery failure",
			result: delivery.Result{Kind: delivery.KindTransferTemporary},
			want:   Retry,
		},
		{
			name:   "permanent SMTP delivery failure",
			result: delivery.Result{Kind: delivery.KindTransferPermanent},
			want:   TerminalFailure,
		},
		{
			name:   "Null MX",
			result: delivery.Result{Kind: delivery.KindDNSNullMX},
			want:   TerminalFailure,
		},
		{
			name:   "DNS not found",
			result: delivery.Result{Kind: delivery.KindDNSNotFound},
			want:   TerminalFailure,
		},
		{
			name:   "invalid request",
			result: delivery.Result{Kind: delivery.KindInvalidRequest},
			want:   TerminalFailure,
		},
		{
			name:   "caller cancellation",
			result: delivery.Result{Kind: delivery.KindContext},
			err:    fmt.Errorf("delivery interrupted: %w", context.Canceled),
			want:   TerminalFailure,
		},
		{
			name:   "caller deadline",
			result: delivery.Result{Kind: delivery.KindContext},
			err:    fmt.Errorf("delivery interrupted: %w", context.DeadlineExceeded),
			want:   TerminalFailure,
		},
		{
			name:   "cancellation overrides temporary kind",
			result: delivery.Result{Kind: delivery.KindTransferTemporary},
			err:    context.Canceled,
			want:   TerminalFailure,
		},
		{
			name:   "unclassified DNS failure",
			result: delivery.Result{Kind: delivery.KindDNSFailure},
			want:   TerminalFailure,
		},
		{
			name: "unknown failure",
			err:  errors.New("unclassified delivery failure"),
			want: TerminalFailure,
		},
		{
			name: "zero value result",
			want: TerminalFailure,
		},
		{
			name:   "inconsistent accepted kind without acceptance flag",
			result: delivery.Result{Kind: delivery.KindAccepted},
			want:   TerminalFailure,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Decide(test.result, test.err); got != test.want {
				t.Fatalf("Decide() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAcceptedIsAlwaysTerminalSuccess(t *testing.T) {
	kinds := []delivery.Kind{
		"",
		delivery.KindAccepted,
		delivery.KindInvalidRequest,
		delivery.KindDNSNotFound,
		delivery.KindDNSNullMX,
		delivery.KindDNSTemporary,
		delivery.KindDNSFailure,
		delivery.KindTransferTemporary,
		delivery.KindTransferPermanent,
		delivery.KindContext,
		delivery.Kind("future_unknown_kind"),
	}
	errs := []error{
		nil,
		errors.New("cleanup failed"),
		context.Canceled,
		context.DeadlineExceeded,
	}

	for _, kind := range kinds {
		for _, err := range errs {
			result := delivery.Result{
				Kind:      kind,
				Accepted:  true,
				QuitError: "post-acceptance connection failure",
			}
			if got := Decide(result, err); got != TerminalSuccess {
				t.Fatalf("Decide(Accepted=true, Kind=%q, err=%v) = %v, want %v", kind, err, got, TerminalSuccess)
			}
		}
	}
}
