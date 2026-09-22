package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
)

var allAudienceScopes = []auth.Scope{auth.ScopeAudiencesRead, auth.ScopeAudiencesWrite, auth.ScopeContactsRead, auth.ScopeContactsWrite, auth.ScopeSuppressionsRead, auth.ScopeSuppressionsWrite}

func decodeAudience(t *testing.T, rec *httptest.ResponseRecorder) audienceResource {
	t.Helper()
	var r audienceResource
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return r
}

func TestAudienceCRUD(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)

	rec := doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": "Newsletter"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	created := decodeAudience(t, rec)
	if created.ID == "" || created.Name != "Newsletter" {
		t.Fatalf("%+v", created)
	}

	if rec = doJSON(t, ac.h, "GET", "/v1/audiences/"+created.ID, nil); rec.Code != 200 || decodeAudience(t, rec).ID != created.ID {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, ac.h, "GET", "/v1/audiences", nil)
	var list audienceList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, ac.h, "PATCH", "/v1/audiences/"+created.ID, map[string]any{"name": "Renamed"})
	if rec.Code != 200 || decodeAudience(t, rec).Name != "Renamed" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	if rec = doJSON(t, ac.h, "DELETE", "/v1/audiences/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	if rec = doJSON(t, ac.h, "GET", "/v1/audiences/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAudienceDuplicateNameConflict(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	body := map[string]any{"name": "dup"}
	if rec := doJSON(t, ac.h, "POST", "/v1/audiences", body); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	if rec := doJSON(t, ac.h, "POST", "/v1/audiences", body); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAudienceInvalidNameRejected(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	if rec := doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": ""}); rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAudienceCrossTenantNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	ac1, _ := a.scopedActor("acme", allAudienceScopes...)
	ac2, _ := a.scopedActor("other", allAudienceScopes...)
	rec := doJSON(t, ac1.h, "POST", "/v1/audiences", map[string]any{"name": "x"})
	created := decodeAudience(t, rec)
	if rec := doJSON(t, ac2.h, "GET", "/v1/audiences/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, ac2.h, "DELETE", "/v1/audiences/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := doJSON(t, ac1.h, "GET", "/v1/audiences/"+created.ID, nil); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAudienceRequiresScope(t *testing.T) {
	a := newDKIMAPI(t)
	readOnly, _ := a.scopedActor("acme", auth.ScopeAudiencesRead)
	if rec := doJSON(t, readOnly.h, "POST", "/v1/audiences", map[string]any{"name": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- membership

func TestAddListRemoveMember(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	aud := decodeAudience(t, doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": "a"}))
	c := decodeContact(t, doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "one@example.com"}))

	rec := doJSON(t, ac.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": c.ID})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// Idempotent re-add.
	rec = doJSON(t, ac.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": c.ID})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("re-add: %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, ac.h, "GET", "/v1/audiences/"+aud.ID+"/contacts", nil)
	var list contactList
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != c.ID {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	if rec = doJSON(t, ac.h, "DELETE", "/v1/audiences/"+aud.ID+"/contacts/"+c.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec = doJSON(t, ac.h, "DELETE", "/v1/audiences/"+aud.ID+"/contacts/"+c.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("remove absent membership: %d", rec.Code)
	}
	// Contact itself is untouched.
	if rec := doJSON(t, ac.h, "GET", "/v1/contacts/"+c.ID, nil); rec.Code != 200 {
		t.Fatalf("contact affected by removal: %d", rec.Code)
	}
}

func TestAddMemberCrossTenantContactNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	owner, _ := a.scopedActor("acme", allAudienceScopes...)
	attacker, _ := a.scopedActor("attacker", allAudienceScopes...)
	aud := decodeAudience(t, doJSON(t, owner.h, "POST", "/v1/audiences", map[string]any{"name": "a"}))
	victim := decodeContact(t, doJSON(t, owner.h, "POST", "/v1/contacts", map[string]any{"email": "victim@example.com"}))
	// attacker's own audience, trying to add owner's contact
	attackerAud := decodeAudience(t, doJSON(t, attacker.h, "POST", "/v1/audiences", map[string]any{"name": "x"}))
	rec := doJSON(t, attacker.h, "POST", "/v1/audiences/"+attackerAud.ID+"/contacts", map[string]any{"contact_id": victim.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// attacker trying to add to owner's audience
	rec = doJSON(t, attacker.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": victim.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestListMembersUnknownAudienceNotFound(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	rec := doJSON(t, ac.h, "GET", "/v1/audiences/does-not-exist/contacts", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------- suppression/lifecycle

func TestSuppressedContactCanBeAddedThroughAPI(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	c := decodeContact(t, doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "one@example.com"}))
	doJSON(t, ac.h, "POST", "/v1/suppressions", map[string]any{"email": "one@example.com"})
	aud := decodeAudience(t, doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": "a"}))
	rec := doJSON(t, ac.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": c.ID})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteAudiencePreservesContactViaAPI(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	c := decodeContact(t, doJSON(t, ac.h, "POST", "/v1/contacts", map[string]any{"email": "one@example.com"}))
	aud := decodeAudience(t, doJSON(t, ac.h, "POST", "/v1/audiences", map[string]any{"name": "a"}))
	doJSON(t, ac.h, "POST", "/v1/audiences/"+aud.ID+"/contacts", map[string]any{"contact_id": c.ID})
	doJSON(t, ac.h, "DELETE", "/v1/audiences/"+aud.ID, nil)
	if rec := doJSON(t, ac.h, "GET", "/v1/contacts/"+c.ID, nil); rec.Code != 200 {
		t.Fatalf("contact deleted with audience: %d", rec.Code)
	}
}

func TestListAudiencesPaginationInvalidCursor(t *testing.T) {
	a := newDKIMAPI(t)
	ac, _ := a.scopedActor("acme", allAudienceScopes...)
	rec := doJSON(t, ac.h, "GET", "/v1/audiences?cursor=not-valid-base64!!", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
