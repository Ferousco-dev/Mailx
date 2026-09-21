package delivery

import (
	"context"
	"errors"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

func TestTLSStagesTryNextMXBecauseNoMessageWasSent(t *testing.T) {
	for _, stage := range []smtp.Stage{smtp.StageStartTLS, smtp.StageTLS, smtp.StageEHLOTLS} {
		err := &transfer.TransferError{Stage: stage, Temporary: true}
		if got := decideFallback(err); got != tryNext {
			t.Fatalf("%s: got %d, want tryNext", stage, got)
		}
	}
}

// One MX with broken TLS must not stop delivery through the next MX.
func TestBrokenTLSOnFirstMXFallsBackToSecondMX(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageTLS, Temporary: true, Err: errors.New("tls verify_failed")}},
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
	}}
	r := fakeResolver{mx: []dns.MX{{Host: "mx1.example.com", Preference: 10}, {Host: "mx2.example.com", Preference: 20}}}
	res, err := newEngine(t, r, x).Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err != nil || !res.Accepted || len(res.Attempts) != 2 {
		t.Fatalf("err=%v accepted=%v attempts=%d", err, res.Accepted, len(res.Attempts))
	}
}

// When every MX fails TLS the outcome is temporary, so the retry engine
// reschedules instead of failing the email permanently (see the retry
// package's TLS test for the Decide side).
func TestAllMXTLSFailuresAreTemporaryAndRetryable(t *testing.T) {
	tlsErr := func(stage smtp.Stage) stubOutcome {
		return stubOutcome{err: &transfer.TransferError{Stage: stage, Temporary: true, Err: errors.New("tls")}}
	}
	for _, stage := range []smtp.Stage{smtp.StageStartTLS, smtp.StageTLS, smtp.StageEHLOTLS} {
		x := &stubTransfer{script: []stubOutcome{tlsErr(stage), tlsErr(stage)}}
		r := fakeResolver{mx: []dns.MX{{Host: "mx1.example.com", Preference: 10}, {Host: "mx2.example.com", Preference: 20}}}
		res, err := newEngine(t, r, x).Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
		if err == nil || res.Accepted || res.Kind != KindTransferTemporary || !IsTemporary(err) {
			t.Fatalf("%s: kind=%s err=%v", stage, res.Kind, err)
		}
	}
}
