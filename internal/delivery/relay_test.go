package delivery

import (
	"context"
	"errors"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

type countingResolver struct{ lookups int }

func (c *countingResolver) LookupMX(context.Context, string) ([]dns.MX, error) {
	c.lookups++
	return []dns.MX{{Host: "mx1.example.com", Preference: 10}, {Host: "mx2.example.com", Preference: 20}}, nil
}

func relayEngine(t *testing.T, r Resolver, x Transferer, relay *Relay) *Engine {
	t.Helper()
	e, err := NewEngine(r, x, Config{SMTPPort: 25, Shuffle: noShuffle, Relay: relay})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var deliverReq = Request{Domain: "example.com", Envelope: envelope("<a@example.com>"), Raw: "x"}

// Relay mode: no MX lookup, one destination, the only credentials in the system.
func TestRelayModeRoutesOnlyToConfiguredRelayWithCredentials(t *testing.T) {
	creds := &smtp.Credentials{Username: "u", Password: "p"}
	x := &stubTransfer{script: []stubOutcome{{res: transfer.Result{Accepted: true, FinalCode: 250}}}}
	res := &countingResolver{}
	e := relayEngine(t, res, x, &Relay{Host: "relay.example.net", Port: 587, Auth: creds})
	creds.Password = "mutated-after-construction"
	out, err := e.Deliver(context.Background(), deliverReq)
	if err != nil || !out.Accepted || out.Transport != "relay" {
		t.Fatalf("%+v %v", out, err)
	}
	if res.lookups != 0 {
		t.Fatal("relay mode must not look up recipient MX hosts")
	}
	if len(x.calls) != 1 || x.calls[0].Destination != "relay.example.net:587" {
		t.Fatalf("calls = %+v", x.calls)
	}
	if got := x.calls[0].Auth; got == nil || got.Username != "u" || got.Password != "p" {
		t.Fatalf("engine must hold its own copy of the credentials, got %v", got)
	}
}

// N. Direct delivery never carries credentials, to any MX.
func TestDirectDeliveryNeverCarriesCredentials(t *testing.T) {
	x := &stubTransfer{script: []stubOutcome{
		{err: &transfer.TransferError{Stage: smtp.StageDial, Err: errors.New("refused")}},
		{res: transfer.Result{Accepted: true, FinalCode: 250}},
	}}
	e := relayEngine(t, &countingResolver{}, x, nil)
	out, err := e.Deliver(context.Background(), deliverReq)
	if err != nil || !out.Accepted || out.Transport != "direct" || len(x.calls) != 2 {
		t.Fatalf("%+v %v calls=%d", out, err, len(x.calls))
	}
	for i, c := range x.calls {
		if c.Auth != nil {
			t.Fatalf("direct MX attempt %d carried credentials", i)
		}
	}
}

// O. A failing relay is retried later; it never becomes a direct attempt.
func TestRelayFailureNeverFallsBackToDirectDelivery(t *testing.T) {
	failures := map[string]*transfer.TransferError{
		"auth rejected": {Stage: smtp.StageAuth, Code: 535},
		"auth temp":     {Stage: smtp.StageAuth, Code: 454, Temporary: true},
		"tls failed":    {Stage: smtp.StageTLS, Temporary: true, Err: errors.New("tls")},
		"no starttls":   {Stage: smtp.StageStartTLS, Temporary: true},
		"dial refused":  {Stage: smtp.StageDial, Err: errors.New("refused")},
		"greeting":      {Stage: smtp.StageGreeting, Code: 421, Temporary: true},
	}
	for name, terr := range failures {
		x := &stubTransfer{script: []stubOutcome{{err: terr}, {res: transfer.Result{Accepted: true}}, {res: transfer.Result{Accepted: true}}}}
		res := &countingResolver{}
		e := relayEngine(t, res, x, &Relay{Host: "relay.example.net", Port: 587, Auth: &smtp.Credentials{Username: "u", Password: "p"}})
		out, err := e.Deliver(context.Background(), deliverReq)
		if err == nil || out.Accepted || out.Kind != KindTransferTemporary || !IsTemporary(err) {
			t.Fatalf("%s: kind=%s err=%v", name, out.Kind, err)
		}
		if len(x.calls) != 1 || res.lookups != 0 {
			t.Fatalf("%s: relay failure caused %d transfer calls and %d MX lookups (silent downgrade)", name, len(x.calls), res.lookups)
		}
	}
}

func TestAuthFailuresAreTemporaryNeverAnotherMX(t *testing.T) {
	for _, code := range []int{0, 454, 534, 535, 538} {
		if got := decideFallback(&transfer.TransferError{Stage: smtp.StageAuth, Code: code}); got != stopTemporary {
			t.Fatalf("auth code %d: got %d, want stopTemporary", code, got)
		}
	}
}

func TestRelayConfigValidation(t *testing.T) {
	x := &stubTransfer{}
	ok := smtp.Credentials{Username: "u", Password: "p"}
	for name, r := range map[string]Relay{
		"empty host":   {Host: "", Port: 587},
		"host with @":  {Host: "u@relay", Port: 587},
		"host with /":  {Host: "relay/x", Port: 587},
		"port zero":    {Host: "relay", Port: 0},
		"port too big": {Host: "relay", Port: 70000},
		"bad creds":    {Host: "relay", Port: 587, Auth: &smtp.Credentials{Username: "u"}},
	} {
		if _, err := NewEngine(&countingResolver{}, x, Config{Relay: &r}); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if _, err := NewEngine(&countingResolver{}, x, Config{Relay: &Relay{Host: "relay", Port: 587, Auth: &ok}}); err != nil {
		t.Fatal(err)
	}
}
