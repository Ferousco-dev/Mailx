package delivery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// -------- fakes --------------------------------------------------------

type fakeResolver struct {
	mx  []dns.MX
	err error
}

func (f fakeResolver) LookupMX(_ context.Context, _ string) ([]dns.MX, error) {
	return f.mx, f.err
}

type stubTransfer struct {
	// script[i] is the outcome for the i-th Transfer call.
	script []stubOutcome
	calls  []transfer.Request
	mu     sync.Mutex
	n      int32
}

type stubOutcome struct {
	res transfer.Result
	err *transfer.TransferError
	// delay simulates slow transfer for context tests
	delay time.Duration
}

func (s *stubTransfer) Transfer(ctx context.Context, req transfer.Request) (transfer.Result, error) {
	i := atomic.AddInt32(&s.n, 1) - 1
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	if int(i) >= len(s.script) {
		return transfer.Result{}, &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("no scripted response")}
	}
	out := s.script[i]
	if out.delay > 0 {
		select {
		case <-time.After(out.delay):
		case <-ctx.Done():
			return transfer.Result{}, &transfer.TransferError{Stage: smtp.StageDial, Err: ctx.Err()}
		}
	}
	if out.err != nil {
		return out.res, out.err
	}
	return out.res, nil
}

// no-op shuffle for deterministic tests
func noShuffle(_ []dns.MX) {}

func newEngine(t *testing.T, r Resolver, x Transferer) *Engine {
	t.Helper()
	e, err := NewEngine(r, x, Config{SMTPPort: 25, Shuffle: noShuffle})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func envelope(recipients ...string) mail.Envelope {
	return mail.Envelope{MailFrom: "<sender@example.com>", Recipients: recipients}
}

// -------- validation ---------------------------------------------------

func TestValidateRequest(t *testing.T) {
	e := newEngine(t, fakeResolver{}, &stubTransfer{})
	cases := []struct {
		name string
		req  Request
	}{
		{"empty domain", Request{Envelope: envelope("<a@example.com>"), Raw: "x"}},
		{"zero recipients", Request{Domain: "example.com", Envelope: mail.Envelope{MailFrom: "<s@x>"}, Raw: "x"}},
		{"mixed domain recipients", Request{Domain: "a.test", Envelope: envelope("<x@a.test>", "<y@b.test>"), Raw: "x"}},
		{"unbracketed recipient", Request{Domain: "a.test", Envelope: envelope("x@a.test"), Raw: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Deliver(context.Background(), tc.req)
			if err == nil || res.Accepted || res.Kind != KindInvalidRequest {
				t.Fatalf("%s: expected invalid_request, got %+v %v", tc.name, res, err)
			}
		})
	}
}

// -------- DNS translation ---------------------------------------------

func TestDeliverNullMXZeroAttempts(t *testing.T) {
	x := &stubTransfer{}
	e := newEngine(t, fakeResolver{err: &dns.LookupError{Domain: "nomail.test", Kind: dns.KindNullMX}}, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "nomail.test", Envelope: envelope("<a@nomail.test>"), Raw: "x"})
	if err == nil || res.Accepted {
		t.Fatal("expected failure")
	}
	if res.Kind != KindDNSNullMX {
		t.Fatalf("kind: %s", res.Kind)
	}
	if len(x.calls) != 0 {
		t.Fatalf("Null MX must not attempt transfer, got %d calls", len(x.calls))
	}
	if res.Temporary() {
		t.Fatal("Null MX must not be temporary")
	}
}

func TestDeliverNXDOMAINZeroAttempts(t *testing.T) {
	x := &stubTransfer{}
	e := newEngine(t, fakeResolver{err: &dns.LookupError{Domain: "gone.test", Kind: dns.KindNotFound}}, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "gone.test", Envelope: envelope("<a@gone.test>"), Raw: "x"})
	if err == nil || res.Kind != KindDNSNotFound || len(x.calls) != 0 || res.Temporary() {
		t.Fatalf("bad NXDOMAIN outcome: %+v %v", res, err)
	}
}

func TestDeliverTempDNSZeroAttempts(t *testing.T) {
	x := &stubTransfer{}
	e := newEngine(t, fakeResolver{err: &dns.LookupError{Domain: "slow.test", Kind: dns.KindTemporary, Err: errors.New("servfail")}}, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "slow.test", Envelope: envelope("<a@slow.test>"), Raw: "x"})
	if err == nil || res.Kind != KindDNSTemporary || len(x.calls) != 0 || !res.Temporary() {
		t.Fatalf("bad temp DNS outcome: %+v %v", res, err)
	}
}

// -------- MX ordering + destination construction -----------------------

func TestDeliverPrefersLowerPreferenceMX(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err != nil || !res.Accepted {
		t.Fatalf("delivery failed: %v %+v", err, res)
	}
	if len(x.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(x.calls))
	}
	if x.calls[0].Destination != "mx1.example.com:25" {
		t.Fatalf("wrong destination: %s", x.calls[0].Destination)
	}
}

func TestDeliverConfiguredPort(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{{res: transfer.Result{Accepted: true, FinalCode: 250}}}}
	r := fakeResolver{mx: []dns.MX{{Host: "mx.example.com", Preference: 10}}}
	e, err := NewEngine(r, x, Config{SMTPPort: 2525, Shuffle: noShuffle})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if x.calls[0].Destination != "mx.example.com:2525" {
		t.Fatalf("port not applied: %s", x.calls[0].Destination)
	}
}

// -------- Same-operation MX fallback policy ---------------------------

func TestFallbackPolicy(t *testing.T) {
	cases := []struct {
		name string
		err  *transfer.TransferError
		want fallbackDecision
	}{
		{"dial network failure → try next", &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("connection refused")}, tryNext},
		{"greeting failure → try next", &transfer.TransferError{Stage: smtp.StageGreeting, Code: 421, Temporary: true}, tryNext},
		{"EHLO 5xx → try next", &transfer.TransferError{Stage: smtp.StageEHLO, Code: 502}, tryNext},
		{"MAIL 4xx → stop temporary", &transfer.TransferError{Stage: smtp.StageMailFrom, Code: 451, Temporary: true}, stopTemporary},
		{"MAIL 5xx → stop permanent", &transfer.TransferError{Stage: smtp.StageMailFrom, Code: 550}, stopPermanent},
		{"RCPT 4xx → stop temporary", &transfer.TransferError{Stage: smtp.StageRcptTo, Code: 451, Temporary: true}, stopTemporary},
		{"RCPT 5xx → stop permanent", &transfer.TransferError{Stage: smtp.StageRcptTo, Code: 550}, stopPermanent},
		{"final DATA 4xx → stop temporary", &transfer.TransferError{Stage: smtp.StageDataResponse, Code: 451, Temporary: true}, stopTemporary},
		{"final DATA 5xx → stop permanent", &transfer.TransferError{Stage: smtp.StageDataResponse, Code: 550}, stopPermanent},
		{"invalid input → stop permanent", &transfer.TransferError{Stage: smtp.StageInvalidInput, Err: errors.New("bad")}, stopPermanent},
	}
	for _, tc := range cases {
		if got := decideFallback(tc.err); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestFallbackOnDialFailureTriesNext(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("connection refused")}},
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err != nil || !res.Accepted {
		t.Fatalf("expected acceptance after fallback: %v %+v", err, res)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(res.Attempts))
	}
	if res.Attempts[0].TransferErr == nil || res.Attempts[1].TransferErr != nil {
		t.Fatalf("attempt errors wrong: %+v", res.Attempts)
	}
}

func TestNoFallbackOnPermanentSMTPFailure(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageRcptTo, Code: 550, Recipient: "<a@example.com>"}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err == nil || res.Accepted {
		t.Fatal("expected failure")
	}
	if len(x.calls) != 1 {
		t.Fatalf("permanent 5xx must not fall back, got %d calls", len(x.calls))
	}
	if res.Kind != KindTransferPermanent || res.Temporary() {
		t.Fatalf("wrong kind: %+v", res)
	}
	if res.FinalCode != 550 || res.Recipient != "<a@example.com>" {
		t.Fatalf("metadata lost: %+v", res)
	}
}

// TestFailureResultPreservesRemoteMessage is a regression test for a v0.10
// bug found while building v0.12's DSN generation: the failure path built
// Result from the terminating TransferError but never copied its Remote
// (raw SMTP diagnostic) text into Result.RemoteMessage — only the
// acceptance path did. Any consumer that needed the actual remote server
// text on failure (e.g. a bounce/DSN Diagnostic-Code) silently got "".
func TestFailureResultPreservesRemoteMessage(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{
			res: transfer.Result{RemoteMessage: "user unknown"},
			err: &transfer.TransferError{Stage: smtp.StageRcptTo, Code: 550, Recipient: "<a@example.com>"},
		},
	}}
	r := fakeResolver{mx: []dns.MX{{Host: "mx1.example.com", Preference: 10}}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if res.RemoteMessage != "user unknown" {
		t.Fatalf("RemoteMessage not propagated on failure path: got %q", res.RemoteMessage)
	}
}

func TestNoFallbackOnTemporarySMTPCommandFailure(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageMailFrom, Code: 451, Temporary: true}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err == nil || res.Accepted || res.Kind != KindTransferTemporary || !res.Temporary() {
		t.Fatalf("expected temp stop, got %+v %v", res, err)
	}
	if len(x.calls) != 1 {
		t.Fatalf("4xx post-EHLO must not fall back, got %d calls", len(x.calls))
	}
}

func TestAllMXNetworkFailuresTemporary(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("timeout")}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err == nil || res.Accepted {
		t.Fatal("expected failure")
	}
	if !res.Temporary() {
		t.Fatalf("all-unreachable must be temporary: %+v", res)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(res.Attempts))
	}
}

// -------- Acceptance semantics ----------------------------------------

func TestAcceptanceStopsFallback(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
		// This one must NEVER run:
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err != nil || !res.Accepted {
		t.Fatal("expected acceptance")
	}
	if len(x.calls) != 1 {
		t.Fatalf("second MX must not be contacted after acceptance, got %d calls", len(x.calls))
	}
}

func TestAcceptedPreservesQuitError(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{res: transfer.Result{Accepted: true, FinalCode: 250, QuitError: "connection reset during QUIT"}},
	}}
	r := fakeResolver{mx: []dns.MX{{Host: "mx.example.com", Preference: 10}}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err != nil {
		t.Fatalf("QUIT failure must not fail delivery: %v", err)
	}
	if !res.Accepted || res.QuitError == "" {
		t.Fatalf("expected accepted with QuitError, got %+v", res)
	}
}

func TestFinalDATAFailureDoesNotAccept(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDataResponse, Code: 552, Enhanced: "5.3.4"}},
	}}
	r := fakeResolver{mx: []dns.MX{{Host: "mx.example.com", Preference: 10}}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if res.Accepted {
		t.Fatalf("final DATA 552 must NOT be accepted: %+v", res)
	}
	if err == nil || res.Kind != KindTransferPermanent {
		t.Fatalf("expected permanent: %+v %v", res, err)
	}
}

// -------- Equal-preference ordering ------------------------------------

func TestEqualPreferenceShuffle(t *testing.T) {
	var shufCalls int
	shuffle := func(mx []dns.MX) { shufCalls++ }
	r := fakeResolver{mx: []dns.MX{
		{Host: "a.example.com", Preference: 10},
		{Host: "b.example.com", Preference: 10},
		{Host: "c.example.com", Preference: 10},
		{Host: "d.example.com", Preference: 20},
	}}
	x := &stubTransfer{script: []stubOutcome{{res: transfer.Result{Accepted: true, FinalCode: 250}}}}
	e, _ := NewEngine(r, x, Config{SMTPPort: 25, Shuffle: shuffle})
	_, _ = e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if shufCalls != 1 {
		t.Fatalf("expected 1 equal-pref shuffle (the pref=10 group), got %d", shufCalls)
	}
}

// -------- Context ------------------------------------------------------

func TestDeliverContextCanceledStopsFallback(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1.example.com", Preference: 10},
		{Host: "mx2.example.com", Preference: 20},
	}}
	e := newEngine(t, r, x)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before the second attempt runs, using a stub that runs first
	// then loops back to check ctx.
	cancel()
	res, err := e.Deliver(ctx, Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err == nil || res.Accepted {
		t.Fatal("expected failure")
	}
	if res.Kind != KindContext {
		t.Fatalf("expected KindContext, got %s", res.Kind)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation chain lost: %v", err)
	}
}

func TestDeliverDeadlineExceededPreserved(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{delay: 200 * time.Millisecond, err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
	}}
	r := fakeResolver{mx: []dns.MX{{Host: "mx.example.com", Preference: 10}}}
	e := newEngine(t, r, x)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	res, err := e.Deliver(ctx, Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err == nil || res.Accepted {
		t.Fatal("expected failure")
	}
	if !res.Temporary() {
		t.Fatalf("deadline should be temporary: %+v", res)
	}
}

// -------- Concurrency + identity --------------------------------------

func TestDeliverConcurrent(t *testing.T) {
	r := fakeResolver{mx: []dns.MX{{Host: "mx.example.com", Preference: 10}}}
	// Each call gets its own stub so no shared counter races on script.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := &stubTransfer{script: []stubOutcome{{res: transfer.Result{Accepted: true, FinalCode: 250}}}}
			e := newEngine(t, r, x)
			_, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
			if err != nil {
				t.Errorf("concurrent: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestDeliveryIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newDeliveryID()
		if seen[id] {
			t.Fatalf("collision at %d", i)
		}
		seen[id] = true
	}
}

// -------- Small helpers ------------------------------------------------

func TestExtractDomain(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   string
	}{
		{"<a@example.com>", true, "example.com"},
		{"<user@sub.example.com>", true, "sub.example.com"},
		{"<>", false, ""},
		{"a@example.com", false, ""},
		{"<a@>", false, ""},
	}
	for _, tc := range cases {
		got, ok := extractDomain(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("extractDomain(%q) = (%q, %v) want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestOrderCandidatesShufflesOnlyEqualGroups(t *testing.T) {
	in := []dns.MX{
		{Host: "a", Preference: 10}, {Host: "b", Preference: 10},
		{Host: "c", Preference: 20},
		{Host: "d", Preference: 30}, {Host: "e", Preference: 30}, {Host: "f", Preference: 30},
	}
	var groups [][]int
	shuffle := func(g []dns.MX) { groups = append(groups, []int{len(g)}) }
	orderCandidates(in, shuffle)
	if len(groups) != 2 || groups[0][0] != 2 || groups[1][0] != 3 {
		t.Fatalf("unexpected shuffle groups: %+v", groups)
	}
}

// -------- Invariants ---------------------------------------------------

func TestInvariantAttemptsLEcandidates(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1", Preference: 10}, {Host: "mx2", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, _ := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if len(res.Attempts) > len(res.MXCandidates) {
		t.Fatalf("attempts exceed candidates")
	}
	if !res.FinishedAt.After(res.StartedAt) && !res.FinishedAt.Equal(res.StartedAt) {
		t.Fatalf("finish before start")
	}
	if res.Attempts[0].MX.Host != res.MXCandidates[0].Host {
		t.Fatalf("attempted MX not from resolved candidates")
	}
}

func TestInvariantAcceptedImpliesNoLaterAttempts(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
		{res: transfer.Result{Accepted: true, FinalCode: 250}}, // never runs
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1", Preference: 10}, {Host: "mx2", Preference: 20}, {Host: "mx3", Preference: 30},
	}}
	e := newEngine(t, r, x)
	res, err := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if err != nil || !res.Accepted {
		t.Fatal("expected acceptance")
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("expected 2 attempts (fail, accept), got %d", len(res.Attempts))
	}
	if !res.Attempts[1].Transfer.Accepted {
		t.Fatal("last attempt should be the accepted one")
	}
	// Last attempt must be the accepted one; nothing after.
	for i := 0; i < len(res.Attempts)-1; i++ {
		if res.Attempts[i].Transfer.Accepted {
			t.Fatalf("attempt %d accepted before last", i)
		}
	}
}

// Ensure Deliver never reports Accepted=true without ≥1 accepted attempt.
func TestInvariantAcceptedImpliesAcceptedAttempt(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
	}}
	r := fakeResolver{mx: []dns.MX{
		{Host: "mx1", Preference: 10}, {Host: "mx2", Preference: 20},
	}}
	e := newEngine(t, r, x)
	res, _ := e.Deliver(context.Background(), Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"})
	if res.Accepted {
		t.Fatal("accepted with no successful attempts")
	}
}

// Config validation
func TestConfigValidation(t *testing.T) {
	r := fakeResolver{}
	x := &stubTransfer{}
	if _, err := NewEngine(nil, x, DefaultConfig()); err == nil {
		t.Fatal("nil resolver must be rejected")
	}
	if _, err := NewEngine(r, nil, DefaultConfig()); err == nil {
		t.Fatal("nil transferer must be rejected")
	}
	if _, err := NewEngine(r, x, Config{SMTPPort: 70000}); err == nil {
		t.Fatal("bad port must be rejected")
	}
	if _, err := NewEngine(r, x, Config{SMTPPort: -1}); err == nil {
		t.Fatal("negative port must be rejected")
	}
}

func TestResultTemporaryMatrix(t *testing.T) {
	cases := map[Kind]bool{
		KindAccepted:          false,
		KindInvalidRequest:    false,
		KindDNSNotFound:       false,
		KindDNSNullMX:         false,
		KindDNSTemporary:      true,
		KindDNSFailure:        false,
		KindTransferTemporary: true,
		KindTransferPermanent: false,
		KindContext:           true,
	}
	for k, want := range cases {
		if got := (Result{Kind: k}).Temporary(); got != want {
			t.Errorf("Temporary(%s)=%v want %v", k, got, want)
		}
	}
}

// Sanity check: ensure Deliver never invoked transfer for Null MX by
// counting via a distinct path (spy resolver + spy transfer using a channel).
func TestSpyNoTransferForNullMX(t *testing.T) {
	fired := make(chan struct{}, 1)
	x := &stubTransferChan{fired: fired}
	r := fakeResolver{err: &dns.LookupError{Kind: dns.KindNullMX}}
	e := newEngine(t, r, x)
	_, _ = e.Deliver(context.Background(), Request{Domain: "nomail.test", Envelope: envelope("<a@nomail.test>"), Raw: "x"})
	select {
	case <-fired:
		t.Fatal("transfer was invoked for Null MX")
	default:
	}
}

type stubTransferChan struct{ fired chan struct{} }

func (s *stubTransferChan) Transfer(_ context.Context, _ transfer.Request) (transfer.Result, error) {
	select {
	case s.fired <- struct{}{}:
	default:
	}
	return transfer.Result{}, &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("nope")}
}

// small compile-time-ish note so import "fmt" doesn't feel wasted
var _ = fmt.Sprintf
