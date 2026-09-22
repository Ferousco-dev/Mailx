package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/queue"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

// routingGateStore layers configurable MemberRoutingGate/MemberRegistryGate
// answers over fakeOutcomeStore. Both default to true (enabled/known) so a
// test only needs to set the one condition it's exercising.
type routingGateStore struct {
	*fakeOutcomeStore
	knownLocally   map[string]bool
	routingEnabled map[string]bool
	routingErr     error
}

func newRoutingGateStore() *routingGateStore {
	return &routingGateStore{fakeOutcomeStore: newFakeOutcomeStore(), knownLocally: map[string]bool{}, routingEnabled: map[string]bool{}}
}

func (s *routingGateStore) MemberKnownLocally(memberID string) bool {
	if v, ok := s.knownLocally[memberID]; ok {
		return v
	}
	return true
}

func (s *routingGateStore) MemberRoutingEnabled(_ context.Context, memberID string) (bool, error) {
	if s.routingErr != nil {
		return false, s.routingErr
	}
	if v, ok := s.routingEnabled[memberID]; ok {
		return v, nil
	}
	return true, nil
}

// runBriefly starts p.Run for a fixed wall-clock window then stops it —
// used for the hold-path tests below, where nothing (loader, coordinator)
// is ever called, so there is no notify channel to block on instead.
func runBriefly(p *Pool, window time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	p.Run(ctx)
}

func TestHoldsWithoutAttemptWhenMemberUnknownLocally(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator() // intentionally no script: Attempt must never be called

	outcomes := newRoutingGateStore()
	outcomes.setSendingMember("m1", "member-not-registered")
	outcomes.knownLocally["member-not-registered"] = false

	p, err := NewPool(q, l, c, outcomes, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	runBriefly(p, 300*time.Millisecond)

	if got := l.calls; got != 0 {
		t.Fatalf("an unknown-locally member must hold before the loader is ever called, got %d loader calls", got)
	}
	if got := c.totalAttempts(); got != 0 {
		t.Fatalf("an unknown-locally member must hold WITHOUT consuming a retry attempt (that would eventually exhaust and permanently fail the message), got %d attempts", got)
	}
	if q.Len() != 1 {
		t.Fatalf("the job must remain queued (held, not acked), Len=%d", q.Len())
	}
}

func TestHoldsWithoutAttemptWhenMemberDisabled(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()

	outcomes := newRoutingGateStore()
	outcomes.setSendingMember("m1", "disabled-member")
	outcomes.routingEnabled["disabled-member"] = false

	p, err := NewPool(q, l, c, outcomes, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}

	runBriefly(p, 300*time.Millisecond)

	if got := l.calls; got != 0 {
		t.Fatalf("a disabled member must hold before the loader is ever called, got %d loader calls", got)
	}
	if got := c.totalAttempts(); got != 0 {
		t.Fatalf("a disabled member must hold without an SMTP attempt, got %d attempts", got)
	}
	if q.Len() != 1 {
		t.Fatalf("the job must remain queued (held, not acked), Len=%d", q.Len())
	}
}

func TestProceedsNormallyWhenMemberKnownAndEnabled(t *testing.T) {
	q := mustQ(t, 4)
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()
	c.script("<a@x>", coordOutcome{outcome: retry.Outcome{Status: retry.StatusSucceeded, Result: delivery.Result{Accepted: true, Kind: delivery.KindAccepted}}})

	outcomes := newRoutingGateStore()
	outcomes.setSendingMember("m1", "good-member") // default known + enabled

	p, err := NewPool(q, l, c, outcomes, Config{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runPoolUntilIdle(t, p, q, 2*time.Second)

	if got := c.totalAttempts(); got != 1 {
		t.Fatalf("a known, enabled member must proceed to exactly 1 attempt, got %d", got)
	}
	if q.Len() != 0 {
		t.Fatalf("job must be acked, Len=%d", q.Len())
	}
}

func TestHoldsWhenMemberRoutingCheckFails(t *testing.T) {
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := mustQ(t, 4, queue.WithClock(func() time.Time { return frozen }))
	l := newFakeLoader()
	l.put("m1", envelopeFor("<a@x>", "<b@y>"), "x\r\n")
	c := newScriptedCoordinator()

	outcomes := newRoutingGateStore()
	outcomes.setSendingMember("m1", "some-member")
	outcomes.routingErr = errors.New("database unavailable")

	p, err := NewPool(q, l, c, outcomes, Config{Workers: 1}, WithClock(func() time.Time { return frozen }))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(context.Background(), queue.Job{ID: "j1", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	runBriefly(p, 300*time.Millisecond)

	if got := c.totalAttempts(); got != 0 {
		t.Fatalf("an unreadable routing-enabled state must degrade to a hold, not an attempt, got %d attempts", got)
	}
	if q.Len() != 1 {
		t.Fatalf("the job must remain queued (held), Len=%d", q.Len())
	}
}
