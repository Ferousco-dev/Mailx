package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/retry"
	"github.com/Ferousco-dev/mailx/internal/suppression"
	"github.com/Ferousco-dev/mailx/internal/worker"
)

// databaseOutcomeStore translates worker/retry domain values into the
// database package's infrastructure-level representation. Keeping this adapter
// at composition root avoids making database depend on delivery or retry.
type databaseOutcomeStore struct {
	db      *database.DB
	metrics *observability.Metrics // optional
}

// Compile-time guarantee that the production outcome store enforces suppression:
// the worker only enforces it for stores implementing worker.SuppressionGate.
var _ worker.SuppressionGate = databaseOutcomeStore{}

// databaseOutcomeStore also names a message's tenant, so per-tenant delivery
// permits work in production.
var _ worker.TenantLookup = databaseOutcomeStore{}

// databaseOutcomeStore also enforces the v0.39 sending-pool member/pool
// kill switch, so a disabled member cannot receive a new SMTP attempt.
var _ worker.MemberRoutingGate = databaseOutcomeStore{}

func (s databaseOutcomeStore) Load(ctx context.Context, messageID string) (worker.DurableDeliveryState, error) {
	durable, err := s.db.LoadDeliveryState(ctx, messageID)
	if err != nil {
		return worker.DurableDeliveryState{}, err
	}
	state := &retry.State{}
	for _, stored := range durable.Attempts {
		result := deliveryResultFromStored(stored)
		var deliveryErr error
		if stored.Decision != database.DecisionTerminalSuccess {
			deliveryErr = &delivery.Error{
				Kind:      result.Kind,
				Domain:    result.Domain,
				Temporary: stored.Decision == database.DecisionRetry,
				Err:       errors.New("persisted delivery outcome"),
			}
		}
		if err := state.Record(result, deliveryErr); err != nil {
			return worker.DurableDeliveryState{}, fmt.Errorf("restore retry state: %w", err)
		}
		latest, _ := state.Latest()
		if decisionToDatabase(latest.Decision) != stored.Decision {
			return worker.DurableDeliveryState{}, fmt.Errorf("restore retry state: stored decision %q disagrees with result kind %q", stored.Decision, stored.Kind)
		}
	}
	return worker.DurableDeliveryState{
		RetryState: state, Terminal: durable.Terminal(), NextRetryAt: durable.NextRetryAt,
		SendingMemberID: durable.SendingMemberID,
	}, nil
}

// MemberRoutingEnabled implements worker.MemberRoutingGate.
func (s databaseOutcomeStore) MemberRoutingEnabled(ctx context.Context, memberID string) (bool, error) {
	return s.db.MemberRoutingEnabled(ctx, memberID)
}

func (s databaseOutcomeStore) Persist(ctx context.Context, messageID string, attempt retry.DeliveryAttempt, outcome retry.Outcome) error {
	result := attempt.Result
	in := database.NewDeliveryAttempt{
		MessageID:      messageID,
		AttemptNumber:  attempt.Number,
		Decision:       decisionToDatabase(attempt.Decision),
		Kind:           string(result.Kind),
		Accepted:       result.Accepted,
		FinalCode:      result.FinalCode,
		EnhancedStatus: result.EnhancedStatus,
		RemoteMessage:  result.RemoteMessage,
		FailureStage:   result.FailureStage,
		Recipient:      result.Recipient,
		QuitError:      result.QuitError,
		StartedAt:      result.StartedAt,
		FinishedAt:     result.FinishedAt,
		// A recipient hard bounce also suppresses that recipient, atomically with
		// this outcome. Structured fields only: a permanent RCPT TO rejection with
		// an enhanced status about the destination MAILBOX (5.1.1, 5.1.6). Temporary
		// failures, sender/policy/authentication failures and every other stage do
		// not qualify (see suppression.QualifiesHardBounce).
		SuppressRecipient: attempt.Decision == retry.TerminalFailure && result.Kind == delivery.KindTransferPermanent &&
			suppression.QualifiesHardBounce(result.FailureStage, true, result.Accepted, result.FinalCode, result.EnhancedStatus, result.Recipient),
		// v0.39 historical transport snapshot. TransportKind reuses
		// Result.Transport ("direct"/"relay", already set by every Engine,
		// legacy or per-member) so it is populated for every attempt.
		// SendingMemberID/EffectiveHostname/EffectiveSourceIP are set only
		// by routing.Router when it actually dispatched to a pool member,
		// and stay empty on the legacy no-pool path.
		SendingMemberID:   result.EffectiveMemberID,
		TransportKind:     result.Transport,
		EffectiveHostname: result.EffectiveHostname,
		EffectiveSourceIP: result.EffectiveSourceIP,
	}
	for _, mx := range result.Attempts {
		in.MXAttempts = append(in.MXAttempts, database.MXAttempt{
			Host: mx.MX.Host, Preference: int(mx.MX.Preference), Destination: mx.Destination,
			Accepted: mx.Transfer.Accepted, FailureStage: string(mx.Transfer.FailureStage),
			Temporary: mx.Transfer.Temporary, Code: mx.Transfer.FinalCode,
			EnhancedStatus: mx.Transfer.EnhancedStatus, RemoteMessage: mx.Transfer.RemoteMessage,
			Recipient: mx.Transfer.Recipient, AttemptID: mx.Transfer.AttemptID,
			StartedAt: mx.Transfer.StartedAt, FinishedAt: mx.Transfer.FinishedAt,
		})
	}
	metadata := map[string]any{
		"delivery_id": result.DeliveryID,
		"kind":        result.Kind,
		"accepted":    result.Accepted,
		"finished_at": result.FinishedAt,
	}
	if result.FinalCode != 0 {
		metadata["smtp_code"] = result.FinalCode
	}
	if result.EnhancedStatus != "" {
		metadata["enhanced_status"] = result.EnhancedStatus
	}
	if result.FailureStage != "" {
		metadata["failure_stage"] = result.FailureStage
	}
	if outcome.Schedule != nil {
		metadata["next_retry_at"] = outcome.Schedule.NextRetryAt
	}
	err := s.db.PersistDeliveryOutcome(ctx, in, outcome.Status == retry.StatusExhausted, metadata)
	if errors.Is(err, database.ErrMessageTerminal) {
		return worker.ErrOutcomeAlreadyTerminal
	}
	if err == nil && in.SuppressRecipient {
		s.metrics.SuppressionWrite(string(suppression.ReasonHardBounce), string(suppression.SourceDelivery))
	}
	return err
}

// SuppressedRecipients implements worker.SuppressionGate.
func (s databaseOutcomeStore) SuppressedRecipients(ctx context.Context, messageID string, keys []string) (map[string]bool, error) {
	return s.db.SuppressedForMessage(ctx, messageID, keys)
}

// MessageTenant implements worker.TenantLookup.
func (s databaseOutcomeStore) MessageTenant(ctx context.Context, messageID string) (string, error) {
	return s.db.MessageTenant(ctx, messageID)
}

// RecordSuppressed implements worker.SuppressionGate.
func (s databaseOutcomeStore) RecordSuppressed(ctx context.Context, messageID string, suppressed map[string]bool, all bool) error {
	err := s.db.RecordSuppressedRecipients(ctx, messageID, suppressed, all)
	if errors.Is(err, database.ErrMessageTerminal) {
		return worker.ErrOutcomeAlreadyTerminal
	}
	return err
}

func decisionToDatabase(decision retry.Decision) database.Decision {
	switch decision {
	case retry.Retry:
		return database.DecisionRetry
	case retry.TerminalSuccess:
		return database.DecisionTerminalSuccess
	default:
		return database.DecisionTerminalFailure
	}
}

func deliveryResultFromStored(stored database.DeliveryAttempt) delivery.Result {
	result := delivery.Result{
		Kind:       delivery.Kind(stored.Kind),
		Accepted:   stored.Accepted,
		StartedAt:  stored.StartedAt,
		FinishedAt: stored.FinishedAt,
	}
	if stored.FinalCode != nil {
		result.FinalCode = *stored.FinalCode
	}
	if stored.EnhancedStatus != nil {
		result.EnhancedStatus = *stored.EnhancedStatus
	}
	if stored.RemoteMessage != nil {
		result.RemoteMessage = *stored.RemoteMessage
		result.FailureMessage = *stored.RemoteMessage
	}
	if stored.FailureStage != nil {
		result.FailureStage = *stored.FailureStage
	}
	if stored.Recipient != nil {
		result.Recipient = *stored.Recipient
	}
	if stored.QuitError != nil {
		result.QuitError = *stored.QuitError
	}
	return result
}
