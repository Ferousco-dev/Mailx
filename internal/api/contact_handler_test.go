package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

var allContactScopes = []auth.Scope{auth.ScopeContactsRead, auth.ScopeContactsWrite, auth.ScopeSuppressionsRead, auth.ScopeSuppressionsWrite}

func decodeContact(t *testing.T, rec *httptest.ResponseRecorder) contactResource {
	t.Helper()
	var r contactResource
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return r
}

func TestContactCRUD(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)

	rec := doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{
		"email": "alice@example.com", "name": "Alice", "attributes": map[string]string{"plan": "pro"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	created := decodeContact(t, rec)
	if created.ID == "" || created.Email != "alice@example.com" || created.Attributes["plan"] != "pro" {
		t.Fatalf("%+v", created)
	}

	if rec = doJSON(t, ac.h, "GET", "/v1/contacts/"+created.ID, nil); rec.Code != 200 || decodeContact(t, rec).ID != created.ID {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, ac.h, "GET", "/v1/contacts", nil)
	var list contactList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, ac.h, "PATCH", "/v1/contacts/"+created.ID, map[string]any{"name": "Alice B"})
	updated := decodeContact(t, rec)
	if rec.Code != 200 || updated.Name != "Alice B" || updated.Email != created.Email {
		t.Fatalf("%d %+v", rec.Code, updated)
	}

	if rec = doJSON(t, ac.h, "DELETE", "/v1/contacts/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	if rec = doJSON(t, ac.h, "GET", "/v1/contacts/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("GET after delete: %d", rec.Code)
	}
	if rec = doJSON(t, ac.h, "PATCH", "/v1/contacts/"+created.ID, map[string]any{"name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH after delete: %d", rec.Code)
	}
	if rec = doJSON(t, ac.h, "DELETE", "/v1/contacts/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE after delete: %d", rec.Code)
	}
}

func TestContactDuplicateConflict(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)
	body := map[string]any{"email": "dup@example.com"}
	if rec := doJSON(t, ac.h, "POST", "/v1/contacts", body); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	if rec := doJSON(t, ac.h, "POST", "/v1/contacts", body); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestContactInvalidEmailRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)
	if rec := doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "not-an-email"}); rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestContactOversizedAttributesRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)
	attrs := map[string]string{}
	for i := 0; i < 25; i++ {
		attrs[string(rune('a'+i%26))+string(rune(i))] = "v"
	}
	rec := doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "a@example.com", "attributes": attrs})
	if rec.Code != 422 || errCode(t, rec) != "invalid_attributes" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestContactCrossTenantNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	ac1, _ := a.scopedActor("acme", allContactScopes...)
	ac2, _ := a.scopedActor("other", allContactScopes...)
	rec := doJSON(t, ac1.h, "POST", "/v1/contacts", map[string]any{"email": "alice@example.com"})
	created := decodeContact(t, rec)
	if rec := doJSON(t, ac2.h, "GET", "/v1/contacts/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, ac2.h, "DELETE", "/v1/contacts/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := doJSON(t, ac1.h, "GET", "/v1/contacts/"+created.ID, nil); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestContactSameEmailAcrossTenants(t *testing.T) {
	a := newDKIMAPI(t)
	ac1, _ := a.scopedActor("acme", allContactScopes...)
	ac2, _ := a.scopedActor("other", allContactScopes...)
	if rec := doJSON(t, ac1.h, "POST", "/v1/contacts", map[string]any{"email": "alice@example.com"}); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	if rec := doJSON(t, ac2.h, "POST", "/v1/contacts", map[string]any{"email": "alice@example.com"}); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
}

func TestContactRequiresScope(t *testing.T) {
	a := newDKIMAPI(t)
	readOnly, _ := a.scopedActor("acme", auth.ScopeContactsRead)
	if rec := doJSON(t, readOnly.h, "POST", "/v1/contacts", map[string]any{"email": "a@example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestListContactsPaginationInvalidCursor(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)
	rec := doJSON(t, ac.h, "GET", "/v1/contacts?cursor=not-valid-base64!!", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ------------------------------------------------------ suppression independence

func TestContactCreationNotBlockedBySuppression(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)
	doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "alice@example.com"})
	rec := doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "alice@example.com"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("suppressed address must still be creatable as a contact: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteContactDoesNotDeleteSuppression(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allContactScopes...)
	rec := doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "alice@example.com"})
	created := decodeContact(t, rec)
	supRec := doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "alice@example.com"})
	sup := decodeSupp(t, supRec)

	doJSON(t, ac.h, "DELETE", "/v1/contacts/"+created.ID, nil)

	if rec := doJSON(t, ac.h, "GET", "/v1/suppressions/"+sup.ID, nil); rec.Code != 200 {
		t.Fatalf("suppression removed alongside contact delete: %d", rec.Code)
	}
}

func TestSendingEmailDoesNotRequireContact(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", auth.ScopeEmailsSend, auth.ScopeEmailsRead)
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := doJSON(t, ac.h, "POST", "/v1/emails", map[string]any{"from": "a@example.com", "to": []string{"nobody-is-a-contact@example.com"}, "text": "hi"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sending must not require a pre-existing contact: %d %s", rec.Code, rec.Body.String())
	}
}
