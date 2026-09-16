package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// authInjector wraps a mux so every existing test call site (dozens of
// them, from before v0.19) keeps working unchanged: it attaches a fixed
// Bearer token to every request rather than requiring each doJSON/doRaw
// call to know about authentication. Tests that specifically exercise
// authentication (auth_test.go) bypass this and set headers directly.
type authInjector struct {
	next  http.Handler
	token string
}

func (a authInjector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.token != "" {
		r.Header.Set("Authorization", "Bearer "+a.token)
	}
	a.next.ServeHTTP(w, r)
}

func doJSON(t *testing.T, mux http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func doRaw(t *testing.T, mux http.Handler, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// setupMux builds a full API stack, real authentication included, and
// returns a handler pre-authenticated as one API key with every current
// scope — the vast majority of v0.18-era tests care about handler
// behavior, not authentication itself, so they should not need to know
// an Authorization header exists. auth_test.go exercises authentication
// directly via setupMuxNoAuth.
func setupMux(t *testing.T) (http.Handler, *database.DB, string) {
	t.Helper()
	mux, db, tenant, _, rawKey := setupMuxNoAuth(t)
	return authInjector{next: mux, token: rawKey}, db, tenant.ID
}

// setupMuxNoAuth is the same stack without the authInjector wrapper, for
// tests that need to control Authorization themselves.
func setupMuxNoAuth(t *testing.T) (http.Handler, *database.DB, database.Tenant, *auth.Service, string) {
	t.Helper()
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, store)
	authSvc := auth.NewService(db, nil)
	gen, _, err := authSvc.Create(context.Background(), tenant.ID, "test key",
		[]string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead), string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(h, authSvc, func() error { return nil })
	return mux, db, tenant, authSvc, gen.Raw
}

// ------------------------------------------------------------ POST -----

func TestSendTextEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "Alice <alice@example.com>", "to": []string{"bob@example.com"},
		"subject": "hi", "text": "hello",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got email
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID == "" || got.Status != "queued" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if len(got.To) != 1 || got.To[0] != "bob@example.com" {
		t.Fatalf("unexpected to: %v", got.To)
	}
}

func TestSendHTMLEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "html": "<b>hi</b>",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendTextAndHTMLEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "html": "<b>hi</b>", "text": "hi",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendMultipleRecipientsCcBccReplyTo(t *testing.T) {
	mux, db, tenantID := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com", "c@example.com"},
		"cc": []string{"d@example.com"}, "bcc": []string{"secret@example.com"},
		"reply_to": "support@example.com", "text": "hi",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got email
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.To) != 2 || len(got.Cc) != 1 || len(got.Bcc) != 1 {
		t.Fatalf("unexpected recipients: %+v", got)
	}

	recipients, err := db.ListRecipients(t.Context(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recipients) != 4 {
		t.Fatalf("expected 4 durable recipient rows, got %d", len(recipients))
	}
	_ = tenantID
}

func TestSendMixedDomainsRejectedBeforeAcceptance(t *testing.T) {
	mux, db, tenantID := setupMux(t)
	for _, recipients := range []map[string]any{
		{"to": []string{"bob@example.com", "carol@other.example"}},
		{"to": []string{"bob@example.com"}, "cc": []string{"carol@other.example"}},
		{"to": []string{"bob@example.com"}, "bcc": []string{"carol@other.example"}},
	} {
		recipients["from"] = "alice@example.com"
		recipients["text"] = "hello"
		rec := doJSON(t, mux, "POST", "/v1/emails", recipients)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "mixed_recipient_domains") {
			t.Fatalf("mixed-domain send accepted or misclassified: %d %s", rec.Code, rec.Body.String())
		}
	}
	rows, err := db.ListMessages(t.Context(), tenantID, nil, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rejected sends created durable messages: %+v", rows)
	}
}

func TestSendSameDomainCaseInsensitive(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "alice@example.com", "to": []string{"bob@EXAMPLE.COM", "carol@example.com"}, "text": "hello",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("same-domain send rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSendUnicodeSubjectAndBody(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"},
		"subject": "héllo wörld", "text": "café ☕",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendMalformedJSON(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doRaw(t, mux, "POST", "/v1/emails", "application/json", []byte(`{"from": `))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendUnknownFieldRejected(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doRaw(t, mux, "POST", "/v1/emails", "application/json", []byte(`{"from":"a@example.com","to":["b@example.com"],"text":"x","attachments":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected unknown field to be rejected as malformed, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendMissingRequiredFields(t *testing.T) {
	mux, _, _ := setupMux(t)
	for _, body := range []map[string]any{
		{"to": []string{"b@example.com"}, "text": "x"},             // missing from
		{"from": "a@example.com", "text": "x"},                     // missing to
		{"from": "a@example.com", "to": []string{"b@example.com"}}, // missing body
	} {
		rec := doJSON(t, mux, "POST", "/v1/emails", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %v: got %d: %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestSendInvalidAddress(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "not-an-address", "to": []string{"b@example.com"}, "text": "x",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendCRLFInjectionRejected(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "x",
		"subject": "hi\r\nBcc: attacker@evil.com",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendEmptyRecipients(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{}, "text": "x",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendTooManyRecipients(t *testing.T) {
	mux, _, _ := setupMux(t)
	to := make([]string, 51)
	for i := range to {
		to[i] = "user@example.com"
	}
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": to, "text": "x",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendOversizedBody(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"},
		"text": strings.Repeat("a", maxBodyLen+1),
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendInvalidContentType(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doRaw(t, mux, "POST", "/v1/emails", "text/plain", []byte(`{}`))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSendAcceptsParameterizedJSONContentType(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doRaw(t, mux, "POST", "/v1/emails", "application/json; charset=utf-8", []byte(`{
		"from":"a@example.com",
		"to":["b@example.com"],
		"text":"hello"
	}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------- GET -----

func TestGetExistingEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	sendRec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "hello body", "html": "<p>hello</p>",
	})
	var sent email
	_ = json.Unmarshal(sendRec.Body.Bytes(), &sent)

	rec := doJSON(t, mux, "GET", "/v1/emails/"+sent.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got email
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Text == nil || *got.Text != "hello body" {
		t.Fatalf("expected text body populated on GET, got %+v", got)
	}
	if got.HTML == nil || *got.HTML != "<p>hello</p>" {
		t.Fatalf("expected html body populated on GET, got %+v", got)
	}
}

func TestGetNonexistentEmail(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/v1/emails/does-not-exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetCrossTenantAccessIsNotFound(t *testing.T) {
	db := newTestDB(t)
	tenantA := newTestTenant(t, db)
	tenantB, err := db.CreateTenant(t.Context(), "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, store)
	authSvc := auth.NewService(db, nil)
	genA, _, err := authSvc.Create(t.Context(), tenantA.ID, "a", []string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	genB, _, err := authSvc.Create(t.Context(), tenantB.ID, "b", []string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(h, authSvc, func() error { return nil })
	muxA := authInjector{next: mux, token: genA.Raw}
	muxB := authInjector{next: mux, token: genB.Raw}

	sendRec := doJSON(t, muxA, "POST", "/v1/emails", map[string]any{
		"from": "a@example.com", "to": []string{"b@example.com"}, "text": "x",
	})
	var sent email
	_ = json.Unmarshal(sendRec.Body.Bytes(), &sent)

	rec := doJSON(t, muxB, "GET", "/v1/emails/"+sent.ID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("tenant B must not see tenant A's email; got %d: %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------------ LIST -----

func TestListEmpty(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/v1/emails", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got emailList
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Data) != 0 || got.NextCursor != nil {
		t.Fatalf("expected empty list, got %+v", got)
	}
}

func TestListMultipleAndPaginationAndStatusFilter(t *testing.T) {
	mux, _, _ := setupMux(t)
	for i := 0; i < 5; i++ {
		rec := doJSON(t, mux, "POST", "/v1/emails", map[string]any{
			"from": "a@example.com", "to": []string{"b@example.com"}, "text": "x",
		})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("seed send failed: %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := doJSON(t, mux, "GET", "/v1/emails?limit=2", nil)
	var page1 emailList
	_ = json.Unmarshal(rec.Body.Bytes(), &page1)
	if len(page1.Data) != 2 || page1.NextCursor == nil {
		t.Fatalf("expected a full first page with a cursor, got %+v", page1)
	}

	rec2 := doJSON(t, mux, "GET", "/v1/emails?limit=2&cursor="+*page1.NextCursor, nil)
	var page2 emailList
	_ = json.Unmarshal(rec2.Body.Bytes(), &page2)
	if len(page2.Data) != 2 {
		t.Fatalf("expected second page of 2, got %+v", page2)
	}
	if page2.Data[0].ID == page1.Data[0].ID {
		t.Fatal("cursor did not advance")
	}

	statusRec := doJSON(t, mux, "GET", "/v1/emails?status=queued", nil)
	var byStatus emailList
	_ = json.Unmarshal(statusRec.Body.Bytes(), &byStatus)
	if len(byStatus.Data) != 5 {
		t.Fatalf("expected all 5 queued, got %d", len(byStatus.Data))
	}
}

func TestListInvalidCursor(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/v1/emails?cursor=not-valid-base64!!", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListLimitBoundaries(t *testing.T) {
	mux, _, _ := setupMux(t)
	for _, limit := range []string{"0", "101", "abc"} {
		rec := doJSON(t, mux, "GET", "/v1/emails?limit="+limit, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("limit=%s: got %d: %s", limit, rec.Code, rec.Body.String())
		}
	}
}

func TestListInvalidStatusFilter(t *testing.T) {
	mux, _, _ := setupMux(t)
	rec := doJSON(t, mux, "GET", "/v1/emails?status=not-a-status", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}
