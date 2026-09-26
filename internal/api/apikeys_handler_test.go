package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// callDomains hits a real requireScope(domains:read) route with an API key.
func (d dashAPI) callDomains(t *testing.T, rawKey string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/domains", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, req)
	return rec.Code
}

func createOrgKey(t *testing.T, d dashAPI, f dashFixture, scopes []string) map[string]any {
	t.Helper()
	rec := d.do(t, "POST", "/v1/orgs/"+f.orgID+"/api-keys", f.owner.AccessToken, map[string]any{"name": "ci", "scopes": scopes})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return decodeJSON(t, rec)
}

func TestOrgAPIKeysAuthorization(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	key := createOrgKey(t, d, f, []string{"domains:read"})
	kid := key["id"].(string)
	base := "/v1/orgs/" + f.orgID + "/api-keys"
	body := map[string]any{"name": "x", "scopes": []string{"domains:read"}}
	cases := []struct {
		name, method, path, token string
		body                      any
		want                      int
	}{
		{"outsider list", "GET", base, f.outsider.AccessToken, nil, http.StatusNotFound},
		{"outsider create", "POST", base, f.outsider.AccessToken, body, http.StatusNotFound},
		{"outsider rotate", "POST", base + "/" + kid + "/rotate", f.outsider.AccessToken, nil, http.StatusNotFound},
		{"outsider revoke", "DELETE", base + "/" + kid, f.outsider.AccessToken, nil, http.StatusNotFound},
		{"nonexistent org", "GET", "/v1/orgs/00000000-0000-0000-0000-000000000000/api-keys", f.owner.AccessToken, nil, http.StatusNotFound},
		{"member create", "POST", base, f.member.AccessToken, body, http.StatusForbidden},
		{"member rotate", "POST", base + "/" + kid + "/rotate", f.member.AccessToken, nil, http.StatusForbidden},
		{"member revoke", "DELETE", base + "/" + kid, f.member.AccessToken, nil, http.StatusForbidden},
		{"member list", "GET", base, f.member.AccessToken, nil, http.StatusOK},
		{"no token", "GET", base, "", nil, http.StatusUnauthorized},
		{"unknown key", "DELETE", base + "/mx_nope", f.owner.AccessToken, nil, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := d.do(t, c.method, c.path, c.token, c.body); rec.Code != c.want {
				t.Fatalf("got %d want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
	// The member/outsider attempts must not have revoked or rotated anything.
	if code := d.callDomains(t, key["key"].(string)); code != http.StatusOK {
		t.Fatalf("key disturbed by unauthorized calls: %d", code)
	}
}

func TestOrgAPIKeyCreateWorksAndListNeverLeaks(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	key := createOrgKey(t, d, f, []string{"domains:read", "emails:read"})
	raw, _ := key["key"].(string)
	if raw == "" || key["status"] != "active" || key["id"] == "" {
		t.Fatalf("bad create response: %v", key)
	}
	if _, ok := key["secret_hash"]; ok {
		t.Fatal("create leaked hash")
	}
	// A real, working API key on a real requireScope route.
	if code := d.callDomains(t, raw); code != http.StatusOK {
		t.Fatalf("HTTP-created key rejected by /v1/domains: %d", code)
	}
	// Scopes are honored: a key without domains:read is refused.
	narrow := createOrgKey(t, d, f, []string{"emails:read"})
	if code := d.callDomains(t, narrow["key"].(string)); code != http.StatusForbidden {
		t.Fatalf("scope not enforced: %d", code)
	}

	rec := d.do(t, "GET", "/v1/orgs/"+f.orgID+"/api-keys", f.member.AccessToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	body := rec.Body.String()
	secret := raw[strings.LastIndex(raw, "_")+1:]
	if strings.Contains(body, raw) || strings.Contains(body, secret) || strings.Contains(body, "hash") || strings.Contains(body, `"key"`) {
		t.Fatalf("list leaked key material: %s", body)
	}
	if n := len(decodeJSON(t, rec)["data"].([]any)); n != 2 {
		t.Fatalf("want 2 keys, got %d", n)
	}
}

func TestOrgAPIKeyRotateAndRevoke(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	base := "/v1/orgs/" + f.orgID + "/api-keys/"
	old := createOrgKey(t, d, f, []string{"domains:read"})

	rec := d.do(t, "POST", base+old["id"].(string)+"/rotate", f.owner.AccessToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	nk := decodeJSON(t, rec)
	if nk["id"] == old["id"] || nk["key"] == "" {
		t.Fatalf("bad rotate response: %v", nk)
	}
	if code := d.callDomains(t, old["key"].(string)); code != http.StatusUnauthorized {
		t.Fatalf("old key still works after rotate: %d", code)
	}
	if code := d.callDomains(t, nk["key"].(string)); code != http.StatusOK {
		t.Fatalf("rotated key rejected: %d", code)
	}
	// The rotated-out key cannot be rotated again.
	if rec := d.do(t, "POST", base+old["id"].(string)+"/rotate", f.owner.AccessToken, nil); rec.Code != http.StatusConflict {
		t.Fatalf("rotate dead key: %d", rec.Code)
	}

	if rec := d.do(t, "DELETE", base+nk["id"].(string), f.owner.AccessToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if code := d.callDomains(t, nk["key"].(string)); code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", code)
	}
	// CLI-matching semantics: a second revoke is not-found, no state change.
	if rec := d.do(t, "DELETE", base+nk["id"].(string), f.owner.AccessToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second revoke: %d", rec.Code)
	}
}

func TestOrgAPIKeyCreateValidation(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	path := "/v1/orgs/" + f.orgID + "/api-keys"
	for name, b := range map[string]map[string]any{
		"unknown scope":   {"name": "x", "scopes": []string{"domains:read", "admin:all"}},
		"duplicate scope": {"name": "x", "scopes": []string{"domains:read", "domains:read"}},
		"no scopes":       {"name": "x", "scopes": []string{}},
		"blank name":      {"name": "  ", "scopes": []string{"domains:read"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := d.do(t, "POST", path, f.owner.AccessToken, b)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
		})
	}
	rec := d.do(t, "POST", path, f.owner.AccessToken, map[string]any{"name": "x", "scopes": []string{"admin:all"}})
	if !strings.Contains(rec.Body.String(), "admin:all") || !strings.Contains(rec.Body.String(), "invalid_scopes") {
		t.Fatalf("error not clear: %s", rec.Body)
	}
	// Nothing was created by the rejected requests.
	list := d.do(t, "GET", path, f.owner.AccessToken, nil)
	if n := len(decodeJSON(t, list)["data"].([]any)); n != 0 {
		t.Fatalf("rejected requests created %d keys", n)
	}
}
