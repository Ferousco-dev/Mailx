// Package transfer orchestrates one SMTP mail-transfer attempt from MailX
// to a known SMTP endpoint. It sits above internal/smtp: the SMTP protocol
// implementation stays low-level, and this package translates a single SMTP
// attempt into a structured transfer Result.
//
// In RFC 5321 terms MailX here plays the role of a client MTA moving one
// message to a known peer. Higher-level concerns — MX discovery, retry
// scheduling, per-domain routing, bounces — are explicitly NOT in scope
// for v0.8 and belong to later milestones.
package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
)

// Request describes one transfer attempt to a caller-specified endpoint.
// No DNS/MX resolution is performed — Destination must already be a
// host:port that the outbound SMTP client can dial directly.
type Request struct {
	Destination string
	Envelope    mail.Envelope
	Raw         string
}

// Result records what happened during one transfer attempt. It is always
// returned by Service.Transfer, even alongside a non-nil error, so callers
// have full attempt metadata for future retry/bounce decisions.
//
// Accepted is true only when the remote SMTP server returned a 2xx reply
// to the DATA terminator. A subsequent QUIT failure sets QuitError but
// does not reset Accepted, mirroring RFC 5321 §4.1.1.10 semantics.
type Result struct {
	AttemptID      string
	Destination    string
	StartedAt      time.Time
	FinishedAt     time.Time
	Accepted       bool
	FailureStage   smtp.Stage
	FailureMessage string
	Temporary      bool
	FinalCode      int
	EnhancedStatus string
	RemoteMessage  string
	Recipient      string
	QuitError      string
}

// Duration reports how long this attempt took.
func (r Result) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }

// TransferError wraps the underlying failure with attempt metadata. It is
// returned alongside a Result carrying the same fields, so callers can
// inspect either.
type TransferError struct {
	Destination string
	Stage       smtp.Stage
	Temporary   bool
	Recipient   string
	Code        int
	Enhanced    string
	Err         error
}

func (e *TransferError) Error() string {
	var parts []string
	parts = append(parts, "transfer:"+string(e.Stage))
	if e.Destination != "" {
		parts = append(parts, "to="+e.Destination)
	}
	if e.Recipient != "" {
		parts = append(parts, "recipient="+e.Recipient)
	}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code=%d", e.Code))
	}
	if e.Enhanced != "" {
		parts = append(parts, "status="+e.Enhanced)
	}
	if e.Temporary {
		parts = append(parts, "temporary")
	}
	if e.Err != nil {
		parts = append(parts, "err="+e.Err.Error())
	}
	return strings.Join(parts, " ")
}

func (e *TransferError) Unwrap() error { return e.Err }

// IsTemporary reports whether err is a temporary transfer failure.
func IsTemporary(err error) bool {
	var t *TransferError
	if errors.As(err, &t) {
		return t.Temporary
	}
	return smtp.IsTemporary(err)
}

// Service performs one synchronous SMTP transfer attempt per call. It holds
// only immutable dependencies, so Transfer is safe for concurrent use.
type Service struct {
	client *smtp.Client
	now    func() time.Time
}

// NewService constructs a Service backed by client. The now function is
// primarily for tests; production callers use NewServiceWithClock or
// NewService and get UTC time.Now.
func NewService(client *smtp.Client) (*Service, error) {
	if client == nil {
		return nil, errors.New("transfer: smtp client must not be nil")
	}
	return &Service{client: client, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Transfer performs one SMTP mail-transfer attempt. It always returns a
// Result; err is non-nil iff the message was not accepted or a transport
// failure occurred before acceptance. A post-acceptance QUIT failure is
// recorded on Result.QuitError but does NOT produce a non-nil error.
func (s *Service) Transfer(ctx context.Context, req Request) (Result, error) {
	start := s.now()
	result := Result{
		AttemptID:   newAttemptID(),
		Destination: req.Destination,
		StartedAt:   start,
	}
	if req.Destination == "" {
		result.FinishedAt = s.now()
		result.FailureStage = smtp.StageInvalidInput
		result.FailureMessage = "destination is empty"
		return result, &TransferError{Stage: smtp.StageInvalidInput, Err: errors.New("destination is empty")}
	}
	if len(req.Envelope.Recipients) == 0 {
		result.FinishedAt = s.now()
		result.FailureStage = smtp.StageInvalidInput
		result.FailureMessage = "envelope has no recipients"
		return result, &TransferError{Stage: smtp.StageInvalidInput, Destination: req.Destination, Err: errors.New("envelope has no recipients")}
	}

	sendResult, err := s.client.Send(ctx, smtp.DeliveryRequest{
		Address:  req.Destination,
		Envelope: req.Envelope,
		Raw:      req.Raw,
	})
	result.FinishedAt = s.now()
	result.Accepted = sendResult.Accepted
	result.FinalCode = sendResult.FinalCode
	result.RemoteMessage = sendResult.FinalMessage
	if sendResult.QuitError != nil {
		result.QuitError = sendResult.QuitError.Error()
	}

	if err != nil {
		var de *smtp.DeliveryError
		if errors.As(err, &de) {
			result.FailureStage = de.Stage
			result.Temporary = de.Temporary
			result.FinalCode = de.Code
			result.EnhancedStatus = de.Enhanced
			result.RemoteMessage = de.Remote
			result.Recipient = de.Recipient
			result.FailureMessage = de.Error()
			return result, &TransferError{
				Destination: req.Destination,
				Stage:       de.Stage,
				Temporary:   de.Temporary,
				Recipient:   de.Recipient,
				Code:        de.Code,
				Enhanced:    de.Enhanced,
				Err:         err,
			}
		}
		result.FailureStage = smtp.Stage("unknown")
		result.FailureMessage = err.Error()
		return result, &TransferError{Destination: req.Destination, Stage: smtp.Stage("unknown"), Err: err}
	}
	return result, nil
}

func newAttemptID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("attempt-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
