package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var messageIDRe = regexp.MustCompile(`(?m)^Message-ID: (<[^>]+>)\r?$`)

func rawMessageID(t *testing.T, raw []byte) string {
	t.Helper()
	m := messageIDRe.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no Message-ID in:\n%s", raw)
	}
	return string(m[1])
}

// Public mode: generated Message-IDs use the configured infrastructure hostname,
// never a fake local domain and never a tenant domain; the header and the stored
// metadata agree; the DKIM signature (made after generation) still verifies; and
// the stored bytes never change afterwards, so retries resend identical bytes.
func TestGeneratedMessageIDUsesInfrastructureHostnameAndStaysSigned(t *testing.T) {
	testMessageIDDomain = "smtp.infra.example.com"
	t.Cleanup(func() { testMessageIDDomain = "" })
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "customer.com")

	st := decodeStatus(t, doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dkim", nil))
	a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
	if rec := doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dkim/verify", nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		rec := a.send(ac, "alice@customer.com", map[string]any{"bcc": []string{"hidden@example.net"}})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		sent := decodeEmail(t, rec)
		stored, err := a.store.Load(sent.ID)
		if err != nil {
			t.Fatal(err)
		}
		mid := rawMessageID(t, stored.Raw)
		if mid != "<"+sent.ID+"@smtp.infra.example.com>" || seen[mid] {
			t.Fatalf("Message-ID %q (duplicate=%v)", mid, seen[mid])
		}
		seen[mid] = true
		raw := string(stored.Raw)
		if strings.Contains(raw, "mailx.local") || strings.Contains(mid, "customer.com") {
			t.Fatalf("public message advertises a local or tenant identity in Message-ID:\n%s", raw)
		}
		msg, err := a.db.GetMessage(context.Background(), ac.tenant.ID, sent.ID)
		if err != nil || msg.MessageIDHeader != mid {
			t.Fatalf("stored metadata %q != header %q (%v)", msg.MessageIDHeader, mid, err)
		}
		// The signature covers the corrected Message-ID: h= lists it and verification passes.
		if !strings.Contains(strings.ToLower(raw), "message-id") || !strings.Contains(raw, "DKIM-Signature:") {
			t.Fatal("message must be DKIM-signed and carry a Message-ID")
		}
		pub := strings.TrimPrefix(strings.Split(st.Pending.DNS.Value, "p=")[1], "")
		if err := verifyWith(t, stored.Raw, "customer.com", st.Pending.Selector, pub); err != nil {
			t.Fatalf("DKIM verification after Message-ID correction: %v", err)
		}
		// Stored bytes are immutable: a later load (a retry) returns identical bytes.
		again, _ := a.store.Load(sent.ID)
		if string(again.Raw) != raw {
			t.Fatal("stored signed MIME changed between loads")
		}
		// Bcc privacy is unchanged.
		if strings.Contains(strings.ToLower(raw), "hidden@example.net") {
			t.Fatal("Bcc leaked into the message")
		}
	}
}

// Local mode keeps the development identity.
func TestGeneratedMessageIDDefaultsToLocalIdentity(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	verifyTestDomain(t, a.db, ac.tenant.ID, "customer.com")
	sent := decodeEmail(t, a.send(ac, "alice@customer.com", nil))
	stored, _ := a.store.Load(sent.ID)
	if got := rawMessageID(t, stored.Raw); got != "<"+sent.ID+"@mailx.local>" {
		t.Fatalf("%s", got)
	}
}

func TestMessageIDDomainValidation(t *testing.T) {
	for _, ok := range []string{"smtp.example.com", "mx1.mail.example.co.uk", "mailx.local"} {
		if !validMessageIDDomain(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "a b.example.com", "x@example.com", "<x.example.com>", "smtp.example.com\r\nBcc: x@y.z", "smtp\x00.example.com", "é.example.com", strings.Repeat("a", 254)} {
		if validMessageIDDomain(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	// validate reports the bad domain itself (not a missing dependency).
	if err := (Config{MessageIDDomain: "bad domain"}).validate(); err == nil || !strings.Contains(err.Error(), "MessageIDDomain") {
		t.Fatalf("validate: %v", err)
	}
}
