package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPlanTable(t *testing.T) {
	cases := []struct {
		id                                 string
		price                              int64
		daily, domains, members, retention int
		broadcasts, webhooks               bool
	}{
		{"free", 0, 500, 5, 1, 7, false, false},
		{"plus", 600, 10_000, 15, 5, 30, true, true},
		{"pro", 2400, 100_000, Unlimited, Unlimited, 90, true, true},
	}
	for _, c := range cases {
		p := PlanFor(c.id)
		if p.ID != c.id || p.PriceUSDCents != c.price || p.DailySends != c.daily || p.Domains != c.domains ||
			p.Members != c.members || p.RetentionDays != c.retention || p.Broadcasts != c.broadcasts || p.Webhooks != c.webhooks {
			t.Fatalf("%s: got %+v", c.id, p)
		}
	}
	for _, id := range []string{"", "enterprise", "FREE"} {
		if PlanFor(id).ID != PlanFree {
			t.Fatalf("PlanFor(%q) should default to free", id)
		}
	}
	if IsPaid("free") || !IsPaid("plus") || !IsPaid("pro") || IsPaid("bogus") {
		t.Fatal("IsPaid wrong")
	}
	if !Within(4, 5) || Within(5, 5) || !Within(1_000_000, Unlimited) {
		t.Fatal("Within wrong")
	}
}

func TestVerifySignature(t *testing.T) {
	p, err := NewPaystack("sk_test_x", "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"charge.success"}`)
	if err := p.VerifySignature(body, p.Sign(body)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	other, _ := NewPaystack("sk_test_y", "")
	for name, sig := range map[string]string{
		"empty":         "",
		"garbage":       "not-hex",
		"short":         "abcd",
		"other key":     other.Sign(body),
		"tampered body": p.Sign([]byte(`{"event":"charge.failed"}`)),
	} {
		if err := p.VerifySignature(body, sig); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("%s: want ErrInvalidSignature, got %v", name, err)
		}
	}
	if _, err := NewPaystack("  ", ""); err == nil {
		t.Fatal("empty secret must be refused")
	}
}

// TestParseEventToleratesNonObjectMetadata is a regression test for a
// CodeRabbit finding (PR #24): Paystack sends non-object metadata (e.g. 0
// or "") for a transaction MailX's checkout never created (a payment page
// or another integration on the same account). That must decode to an
// empty Metadata, not fail ParseEvent - handleWebhook must still be able
// to acknowledge such an event 200 rather than answering 400 and making
// Paystack retry an event that can never become recognizable.
func TestParseEventToleratesNonObjectMetadata(t *testing.T) {
	for name, body := range map[string]string{
		"integer metadata": `{"event":"charge.success","data":{"metadata":0}}`,
		"string metadata":  `{"event":"charge.success","data":{"metadata":""}}`,
		"null metadata":    `{"event":"charge.success","data":{"metadata":null}}`,
		"array metadata":   `{"event":"charge.success","data":{"metadata":[]}}`,
	} {
		ev, err := ParseEvent([]byte(body))
		if err != nil {
			t.Fatalf("%s: expected no error, got %v", name, err)
		}
		if ev.Data.Metadata != (Metadata{}) {
			t.Fatalf("%s: expected empty Metadata, got %+v", name, ev.Data.Metadata)
		}
	}
	// A genuine object still decodes normally.
	ev, err := ParseEvent([]byte(`{"event":"charge.success","data":{"metadata":{"tenant_id":"t1","plan":"plus"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Data.Metadata != (Metadata{TenantID: "t1", Plan: "plus"}) {
		t.Fatalf("expected real metadata to decode, got %+v", ev.Data.Metadata)
	}
}

func TestInitializeTransactionRequest(t *testing.T) {
	for _, tc := range []struct {
		name, callback string
		wantCallback   bool
	}{
		{name: "with callback", callback: "https://mailx.example/billing/return", wantCallback: true},
		{name: "without callback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/transaction/initialize" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer sk_test_secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected request headers: %v", r.Header)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				want := map[string]string{
					"email": `"owner@example.com"`, "amount": "600", "currency": `"USD"`,
					"metadata": `{"tenant_id":"tenant-1","plan":"plus"}`,
				}
				if tc.wantCallback {
					want["callback_url"] = `"https://mailx.example/billing/return"`
				}
				if len(body) != len(want) {
					t.Errorf("request fields = %v, want %v", body, want)
				}
				for key, value := range want {
					if string(body[key]) != value {
						t.Errorf("%s = %s, want %s", key, body[key], value)
					}
				}
				_, _ = io.WriteString(w, `{"status":true,"data":{"authorization_url":"https://checkout.paystack.com/abc","reference":"ref-123"}}`)
			}))
			defer srv.Close()
			p, err := NewPaystack("sk_test_secret", srv.URL+"/")
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.InitializeTransaction(context.Background(), "owner@example.com", PlanFor(PlanPlus), Metadata{TenantID: "tenant-1", Plan: PlanPlus}, tc.callback)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || got.AuthorizationURL != "https://checkout.paystack.com/abc" || got.Reference != "ref-123" {
				t.Fatalf("calls = %d, result = %+v", calls.Load(), got)
			}
			if strings.Contains(p.String(), "sk_test_secret") {
				t.Fatal("client string exposed secret key")
			}
		})
	}
}

func TestInitializeTransactionRejectsFreePlanBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	p, err := NewPaystack("sk_test_secret", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.InitializeTransaction(context.Background(), "owner@example.com", PlanFor(PlanFree), Metadata{}, "")
	if err == nil || calls.Load() != 0 || result != (InitializeResult{}) {
		t.Fatalf("free checkout: calls = %d, result = %+v, error = %v", calls.Load(), result, err)
	}
}

func TestInitializeTransactionProviderFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantInError string
	}{
		{"HTTP failure", http.StatusBadGateway, `{"status":false,"message":"unavailable"}`, "HTTP 502"},
		{"HTTP failure with success body", http.StatusBadGateway, `{"status":true,"data":{"authorization_url":"https://checkout.example","reference":"ref-123"}}`, "HTTP 502"},
		{"provider refusal", http.StatusOK, `{"status":false,"message":"declined"}`, "declined"},
		{"missing checkout URL", http.StatusOK, `{"status":true,"data":{"reference":"ref-123"}}`, "initialize failed"},
		{"malformed response", http.StatusOK, `{"status":`, "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p, err := NewPaystack("sk_test_secret", srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.InitializeTransaction(context.Background(), "owner@example.com", PlanFor(PlanPlus), Metadata{}, "")
			if err == nil || !strings.Contains(err.Error(), tc.wantInError) || result != (InitializeResult{}) {
				t.Fatalf("result = %+v, error = %v; want %q", result, err, tc.wantInError)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	raw := []byte(`{"event":"charge.success","data":{"reference":"ref-123","status":"success","amount":600,"currency":"USD","metadata":{"tenant_id":"tenant-1","plan":"plus"},"customer":{"customer_code":"CUS_1"}}}`)
	event, err := ParseEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "charge.success" || event.Data.Reference != "ref-123" || event.Data.Status != "success" || event.Data.Amount != 600 || event.Data.Currency != "USD" || event.Data.Metadata != (Metadata{TenantID: "tenant-1", Plan: PlanPlus}) || event.Data.Customer.CustomerCode != "CUS_1" {
		t.Fatalf("parsed event = %+v", event)
	}
	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", `{"event":`},
		{"missing event", `{"data":{}}`},
		{"empty event", `{"event":""}`},
		{"wrong event type", `{"event":123}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEvent([]byte(tc.body)); err == nil {
				t.Fatalf("ParseEvent(%s) accepted malformed event", tc.body)
			}
		})
	}
}

func TestSignatureMatchesHMACSHA512OfRawBody(t *testing.T) {
	p, err := NewPaystack("sk_test_secret", "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(" {\"event\":\"charge.success\"}\n")
	mac := hmac.New(sha512.New, []byte("sk_test_secret"))
	_, _ = mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if got := p.Sign(body); got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
	if err := p.VerifySignature(body, " \n"+strings.ToUpper(want)+"\t"); err != nil {
		t.Fatalf("uppercase signature with surrounding whitespace rejected: %v", err)
	}
	if err := p.VerifySignature([]byte(strings.TrimSpace(string(body))), want); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("signature accepted body with different whitespace: %v", err)
	}
}
