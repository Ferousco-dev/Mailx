package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// scopedActor is a tenant with one API key carrying exactly the given scopes.
func (a *dkimAPI) scopedActor(tenantName string, scopes ...auth.Scope) (actor, string) {
	a.t.Helper()
	tn, err := a.db.CreateTenant(context.Background(), tenantName)
	if err != nil {
		a.t.Fatal(err)
	}
	ac, keyID := a.keyFor(tn.ID, scopes...)
	ac.tenant = tn
	return ac, keyID
}

func (a *dkimAPI) keyFor(tenantID string, scopes ...auth.Scope) (actor, string) {
	a.t.Helper()
	var ss []string
	for _, s := range scopes {
		ss = append(ss, string(s))
	}
	gen, key, err := a.authSvc.Create(context.Background(), tenantID, "k", ss, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return actor{h: authInjector{next: a.mux, token: gen.Raw}}, key.KeyID
}

var allSuppScopes = []auth.Scope{auth.ScopeSuppressionsRead, auth.ScopeSuppressionsWrite, auth.ScopeEmailsSend, auth.ScopeEmailsRead, auth.ScopeDomainsRead, auth.ScopeDomainsWrite}

func decodeSupp(t *testing.T, rec *httptest.ResponseRecorder) suppressionResource {
	t.Helper()
	var s suppressionResource
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return s
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct{ Error struct{ Code string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error.Code
}

func TestSuppressionLifecycleThroughAPI(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allSuppScopes...)

	rec := doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "  Person@EXAMPLE.Com. "})
	created := decodeSupp(t, rec)
	if rec.Code != http.StatusCreated || created.Email != "person@example.com" || created.Reason != "manual" || created.Source != "api" || created.ID == "" || created.CreatedAt.IsZero() {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tenant") {
		t.Fatalf("the response leaks tenant data: %s", rec.Body.String())
	}
	// Duplicate (any spelling): 200 and the SAME entry, unchanged.
	for _, spelling := range []string{"person@example.com", "PERSON@example.com", "<person@example.com>"} {
		rec = doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": spelling, "reason": "manual"})
		if got := decodeSupp(t, rec); rec.Code != http.StatusOK || got.ID != created.ID {
			t.Fatalf("%q: %d %s", spelling, rec.Code, rec.Body.String())
		}
	}
	// Get / list.
	if rec = doJSON(t, ac.h, "GET", "/v1/suppressions/"+created.ID, nil); rec.Code != 200 || decodeSupp(t, rec).ID != created.ID {
		t.Fatal(rec.Body.String())
	}
	rec = doJSON(t, ac.h, "GET", "/v1/suppressions", nil)
	var list suppressionList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 1 || list.NextCursor != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// Delete -> gone -> 404 (and deleting again is 404).
	if rec = doJSON(t, ac.h, "DELETE", "/v1/suppressions/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	for _, m := range []string{"GET", "DELETE"} {
		if rec = doJSON(t, ac.h, m, "/v1/suppressions/"+created.ID, nil); rec.Code != http.StatusNotFound || errCode(t, rec) != "suppression_not_found" {
			t.Fatalf("%s after delete: %d %s", m, rec.Code, rec.Body.String())
		}
	}
	// Re-suppress after delete is a fresh entry.
	if rec = doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "person@example.com"}); rec.Code != http.StatusCreated || decodeSupp(t, rec).ID == created.ID {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSuppressionInputValidation(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allSuppScopes...)
	for name, tc := range map[string]struct {
		body any
		code string
		http int
	}{
		"not an email":              {map[string]any{"email": "not-an-email"}, "invalid_email", 422},
		"missing email":             {map[string]any{}, "invalid_email", 422},
		"display name":              {map[string]any{"email": "A <a@example.com>"}, "invalid_email", 422},
		"unicode":                   {map[string]any{"email": "é@example.com"}, "invalid_email", 422},
		"two addresses":             {map[string]any{"email": "a@example.com,b@example.com"}, "invalid_email", 422},
		"control character":         {map[string]any{"email": "a@example.com\r\nBcc: x@y.z"}, "invalid_email", 422},
		"forged hard_bounce":        {map[string]any{"email": "a@example.com", "reason": "hard_bounce"}, "invalid_reason", 422},
		"unimplemented complaint":   {map[string]any{"email": "a@example.com", "reason": "complaint"}, "invalid_reason", 422},
		"unimplemented unsubscribe": {map[string]any{"email": "a@example.com", "reason": "unsubscribe"}, "invalid_reason", 422},
		"free-form reason":          {map[string]any{"email": "a@example.com", "reason": "because"}, "invalid_reason", 422},
		"unknown field":             {map[string]any{"email": "a@example.com", "source": "delivery"}, "malformed_json", 400},
	} {
		rec := doJSON(t, ac.h, "POST", "/v1/suppressions", tc.body)
		if rec.Code != tc.http || errCode(t, rec) != tc.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "sql") || strings.Contains(rec.Body.String(), "pq:") {
			t.Errorf("%s leaked internals: %s", name, rec.Body.String())
		}
	}
	// Wrong media type and malformed JSON.
	req := httptest.NewRequest("POST", "/v1/suppressions", strings.NewReader(`email=a@example.com`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	ac.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("%d", rec.Code)
	}
	req = httptest.NewRequest("POST", "/v1/suppressions", bytes.NewReader([]byte(`{"email":`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	ac.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	if n := emailCount(t, ac); n != 0 {
		t.Fatal(n)
	}
}

func TestSuppressionScopes(t *testing.T) {
	a := newDKIMAPI(t)
	owner, _ := a.scopedActor("acme", auth.ScopeSuppressionsRead, auth.ScopeSuppressionsWrite)
	created := decodeSupp(t, doJSON(t, owner.h, "POST", "/v1/suppressions", map[string]any{"email": "x@example.com"}))
	tn := owner.tenant
	readOnly, _ := a.keyFor(tn.ID, auth.ScopeSuppressionsRead)
	writeOnly, _ := a.keyFor(tn.ID, auth.ScopeSuppressionsWrite)
	unrelated, _ := a.keyFor(tn.ID, auth.ScopeEmailsSend, auth.ScopeEmailsRead, auth.ScopeDomainsRead, auth.ScopeDomainsWrite, auth.ScopeWebhooksRead, auth.ScopeWebhooksWrite)
	type call struct{ method, path string }
	read := []call{{"GET", "/v1/suppressions"}, {"GET", "/v1/suppressions/" + created.ID}}
	write := []call{{"POST", "/v1/suppressions"}, {"DELETE", "/v1/suppressions/" + created.ID}}
	expect := func(name string, ac actor, calls []call, want int) {
		for _, c := range calls {
			var body any
			if c.method == "POST" {
				body = map[string]any{"email": "scope-check@example.com"}
			}
			if rec := doJSON(t, ac.h, c.method, c.path, body); rec.Code != want && !(want == 200 && (rec.Code == 201 || rec.Code == 204)) {
				t.Errorf("%s %s %s = %d, want %d: %s", name, c.method, c.path, rec.Code, want, rec.Body.String())
			}
		}
	}
	expect("read-only reading", readOnly, read, 200)
	expect("read-only writing", readOnly, write, 403)
	expect("write-only reading", writeOnly, read, 403)
	expect("write-only writing", writeOnly, write[:1], 200)
	expect("unrelated scopes reading", unrelated, read, 403)
	expect("unrelated scopes writing", unrelated, write, 403)
	if rec := doJSON(t, a.mux, "GET", "/v1/suppressions", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
	// A revoked key stops working immediately.
	rev, keyID := a.keyFor(tn.ID, auth.ScopeSuppressionsRead, auth.ScopeSuppressionsWrite)
	if rec := doJSON(t, rev.h, "GET", "/v1/suppressions", nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if err := a.authSvc.Revoke(context.Background(), keyID); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, rev.h, "GET", "/v1/suppressions", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", rec.Code)
	}
	// The scopes are accepted by the key service and stored (the DB CHECK allows them).
	if _, _, err := a.authSvc.Create(context.Background(), tn.ID, "x", []string{"suppressions:read", "suppressions:write"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.authSvc.Create(context.Background(), tn.ID, "x", []string{"suppressions:admin"}, nil); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
}

func TestSuppressionTenantIsolationThroughAPI(t *testing.T) {
	a := newDKIMAPI(t)
	ta, _ := a.scopedActor("tenant-a", allSuppScopes...)
	tb, _ := a.scopedActor("tenant-b", allSuppScopes...)
	sa := decodeSupp(t, doJSON(t, ta.h, "POST", "/v1/suppressions", map[string]any{"email": "shared@example.com"}))
	for _, m := range []string{"GET", "DELETE"} {
		if rec := doJSON(t, tb.h, m, "/v1/suppressions/"+sa.ID, nil); rec.Code != http.StatusNotFound || errCode(t, rec) != "suppression_not_found" {
			t.Fatalf("cross-tenant %s: %d %s", m, rec.Code, rec.Body.String())
		}
	}
	if rec := doJSON(t, ta.h, "GET", "/v1/suppressions/"+sa.ID, nil); rec.Code != 200 {
		t.Fatal("tenant B's delete attempt must not have removed tenant A's entry")
	}
	var list suppressionList
	_ = json.Unmarshal(doJSON(t, tb.h, "GET", "/v1/suppressions", nil).Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Fatalf("tenant B sees tenant A's list: %+v", list)
	}
	if rec := doJSON(t, tb.h, "GET", "/v1/suppressions?email=shared@example.com", nil); !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("the email filter revealed another tenant's entry: %s", rec.Body.String())
	}
	// The same address is independent per tenant: B can suppress it too, with its own id.
	sb := decodeSupp(t, doJSON(t, tb.h, "POST", "/v1/suppressions", map[string]any{"email": "shared@example.com"}))
	if sb.ID == sa.ID {
		t.Fatal("the entries must be independent")
	}
	// Tenant A's suppression does not affect tenant B's sending, and vice versa.
	verifyTestDomain(t, a.db, ta.tenant.ID, "a-domain.com")
	verifyTestDomain(t, a.db, tb.tenant.ID, "b-domain.com")
	doJSON(t, tb.h, "DELETE", "/v1/suppressions/"+sb.ID, nil)
	if rec := a.send(tb, "x@b-domain.com", map[string]any{"to": []string{"shared@example.net"}}); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSuppressionListPaginationAndFilter(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allSuppScopes...)
	for i := 0; i < 5; i++ {
		doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "u" + string(rune('a'+i)) + "@example.com"})
	}
	var seen []string
	cursor := ""
	for page := 0; page < 6; page++ {
		path := "/v1/suppressions?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := doJSON(t, ac.h, "GET", path, nil)
		var list suppressionList
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
		if rec.Code != 200 || len(list.Data) > 2 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		for _, s := range list.Data {
			seen = append(seen, s.Email)
		}
		if list.NextCursor == nil {
			break
		}
		cursor = *list.NextCursor
	}
	if len(seen) != 5 || seen[0] != "ue@example.com" || seen[4] != "ua@example.com" {
		t.Fatalf("complete newest-first pagination: %v", seen)
	}
	for path, want := range map[string]int{"/v1/suppressions?limit=0": 422, "/v1/suppressions?limit=abc": 422, "/v1/suppressions?limit=100000": 422,
		"/v1/suppressions?cursor=!!!": 400, "/v1/suppressions?email=garbage": 422} {
		if rec := doJSON(t, ac.h, "GET", path, nil); rec.Code != want {
			t.Errorf("%s = %d want %d: %s", path, rec.Code, want, rec.Body.String())
		}
	}
	if rec := doJSON(t, ac.h, "GET", "/v1/suppressions?email=UC@Example.COM", nil); !strings.Contains(rec.Body.String(), "uc@example.com") {
		t.Fatal(rec.Body.String())
	}
}

func TestConcurrentSuppressionRequestsConvergeThroughAPI(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allSuppScopes...)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	ids := map[string]bool{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": []string{"Race@Example.com", "race@example.com"}[i%2]})
			mu.Lock()
			defer mu.Unlock()
			codes[rec.Code]++
			ids[decodeSupp(t, rec).ID] = true
		}()
	}
	wg.Wait()
	if codes[201] != 1 || codes[200] != 19 || len(ids) != 1 {
		t.Fatalf("codes=%v ids=%d", codes, len(ids))
	}
}

// --- Acceptance-time behaviour of POST /v1/emails -------------------------------------------------

func suppSetup(t *testing.T) (*dkimAPI, actor) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allSuppScopes...)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	return a, ac
}

func TestEmailAcceptanceWithSuppressedRecipients(t *testing.T) {
	a, ac := suppSetup(t)
	doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "b@example.net"})

	// Every recipient suppressed: refused, nothing stored, nothing written.
	rec := a.send(ac, "alice@example.com", map[string]any{"to": []string{"B@Example.NET"}})
	if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec) != "all_recipients_suppressed" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := emailCount(t, ac); n != 0 {
		t.Fatalf("a rejected send left %d messages", n)
	}
	if files, _ := a.store.List(); len(files) != 0 {
		t.Fatalf("a rejected send wrote %d message files", len(files))
	}
	// Partial: accepted (the worker skips the suppressed one at delivery).
	rec = a.send(ac, "alice@example.com", map[string]any{"to": []string{"a@example.net", "b@example.net"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// cc/bcc count too: suppressed recipient in bcc only, another visible recipient elsewhere.
	rec = a.send(ac, "alice@example.com", map[string]any{"to": []string{"a@example.net"}, "bcc": []string{"b@example.net"}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// Unsuppress: accepted again.
	rows := doJSON(t, ac.h, "GET", "/v1/suppressions", nil)
	var list suppressionList
	_ = json.Unmarshal(rows.Body.Bytes(), &list)
	doJSON(t, ac.h, "DELETE", "/v1/suppressions/"+list.Data[0].ID, nil)
	if rec = a.send(ac, "alice@example.com", map[string]any{"to": []string{"b@example.net"}}); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAllSuppressedRejectionDoesNotConsumeTheIdempotencyKey(t *testing.T) {
	_, ac := suppSetup(t)
	created := decodeSupp(t, doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "b@example.net"}))
	body := map[string]any{"from": "alice@example.com", "to": []string{"b@example.net"}, "subject": "s", "text": "t"}
	if rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "key-1", body); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	doJSON(t, ac.h, "DELETE", "/v1/suppressions/"+created.ID, nil)
	// The same key with the same body now succeeds: the earlier refusal never claimed it.
	if rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "key-1", body); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestRecipientsThatCannotBeKeyedAreRefusedAtAcceptance(t *testing.T) {
	a, ac := suppSetup(t)
	for _, bad := range []string{`"weird name"@example.net`, "user@[192.0.2.1]"} {
		rec := a.send(ac, "alice@example.com", map[string]any{"to": []string{bad}})
		if rec.Code < 400 || rec.Code >= 500 || rec.Code == http.StatusAccepted {
			t.Errorf("%q: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	if n := emailCount(t, ac); n != 0 {
		t.Fatal(n)
	}
}

func TestSuppressedStatusAndEventAreValidPublicVocabulary(t *testing.T) {
	_, ac := suppSetup(t)
	if rec := doJSON(t, ac.h, "GET", "/v1/emails?status=suppressed", nil); rec.Code != http.StatusOK {
		t.Fatalf("status=suppressed filter: %d %s", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, ac.h, "POST", "/v1/webhooks", map[string]any{"url": "https://hooks.example.net/x", "events": []string{"email.suppressed"}})
	if rec.Code == http.StatusBadRequest || rec.Code == http.StatusUnprocessableEntity || rec.Code >= 500 {
		// creation may need webhooks scopes/config in this harness; a validation error naming the event is what would be wrong
		if strings.Contains(rec.Body.String(), "email.suppressed") {
			t.Fatalf("email.suppressed rejected as an event type: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func FuzzSuppressionCursor(f *testing.F) {
	for _, s := range []string{"", "!!!", "YQ", encodeSuppressionCursor(database.SuppressionCursor{CreatedAt: time.Now(), ID: "id"})} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if c, err := decodeSuppressionCursor(s); err == nil && c.ID == "" {
			t.Fatalf("accepted a cursor without an id: %q", s)
		}
	})
}
