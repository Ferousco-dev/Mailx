package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

type dashAPI struct {
	mux http.Handler
	db  *database.DB
	svc *humanauth.Service
}

func newDashAPI(t *testing.T) dashAPI {
	t.Helper()
	db := newTestDB(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := humanauth.NewService(db, []byte("test-secret-at-least-32-bytes-long!!"))
	if err != nil {
		t.Fatal(err)
	}
	mux := newMux(newEmailHandler(db, store), auth.NewService(db, nil), func() error { return nil }, routeServices{humanAuth: svc})
	return dashAPI{mux: mux, db: db, svc: svc}
}

func (d dashAPI) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, req)
	return rec
}

// dashFixture: an org with an owner, a plain member, and an outsider.
type dashFixture struct {
	owner, member, outsider humanauth.Session
	orgID                   string
}

func newDashFixture(t *testing.T, d dashAPI) dashFixture {
	t.Helper()
	ctx := context.Background()
	owner, err := d.svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	org, err := d.svc.CreateOrganization(ctx, owner.Human.ID, "Acme", "")
	if err != nil {
		t.Fatal(err)
	}
	member, err := d.svc.SignUp(ctx, "Bob", "bob@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := d.db.CreateOrgInvitation(ctx, org.ID, owner.Human.ID, "bob@example.com", "h-bob", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.db.AcceptOrgInvitationForExistingHuman(ctx, inv.ID, org.ID, member.Human.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	outsider, err := d.svc.SignUp(ctx, "Eve", "eve@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	return dashFixture{owner: owner, member: member, outsider: outsider, orgID: org.ID}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body, err)
	}
	return m
}

func TestDashboardMe(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	if rec := d.do(t, "GET", "/v1/me", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	rec := d.do(t, "GET", "/v1/me", f.member.AccessToken, nil)
	m := decodeJSON(t, rec)
	orgs, _ := m["organizations"].([]any)
	if rec.Code != 200 || m["email"] != "bob@example.com" || len(orgs) != 1 {
		t.Fatalf("GET /v1/me: %d %s", rec.Code, rec.Body)
	}
	rec = d.do(t, "PATCH", "/v1/me", f.member.AccessToken, map[string]string{"name": " Robert ", "avatar_url": "https://img.example/b.png"})
	m = decodeJSON(t, rec)
	if rec.Code != 200 || m["name"] != "Robert" || m["avatar_url"] != "https://img.example/b.png" || m["email"] != "bob@example.com" {
		t.Fatalf("PATCH /v1/me: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []map[string]string{{"name": "  "}, {"avatar_url": "javascript:alert(1)"}, {"logo_url": "https://x.example"}} {
		if rec := d.do(t, "PATCH", "/v1/me", f.member.AccessToken, bad); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PATCH %v: %d %s", bad, rec.Code, rec.Body)
		}
	}
}

func TestDashboardOrgDetailAndPatch(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	path := "/v1/orgs/" + f.orgID
	rec := d.do(t, "GET", path, f.member.AccessToken, nil)
	if m := decodeJSON(t, rec); rec.Code != 200 || m["name"] != "Acme" || m["plan"] != "free" || m["plan_status"] != "active" {
		t.Fatalf("member GET: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{path, "/v1/orgs/does-not-exist"} {
		if rec := d.do(t, "GET", p, f.outsider.AccessToken, nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "organization_not_found") {
			t.Fatalf("outsider GET %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	body := map[string]string{"name": "Acme Inc", "logo_url": "https://img.example/l.png"}
	if rec := d.do(t, "PATCH", path, f.member.AccessToken, body); rec.Code != 403 || !strings.Contains(rec.Body.String(), "not_org_owner") {
		t.Fatalf("member PATCH: %d %s", rec.Code, rec.Body)
	}
	if rec := d.do(t, "PATCH", path, f.outsider.AccessToken, body); rec.Code != 404 {
		t.Fatalf("outsider PATCH: %d", rec.Code)
	}
	rec = d.do(t, "PATCH", path, f.owner.AccessToken, body)
	if m := decodeJSON(t, rec); rec.Code != 200 || m["name"] != "Acme Inc" || m["logo_url"] != "https://img.example/l.png" {
		t.Fatalf("owner PATCH: %d %s", rec.Code, rec.Body)
	}
}

func TestDashboardMembers(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	base := "/v1/orgs/" + f.orgID + "/members"
	rec := d.do(t, "GET", base, f.member.AccessToken, nil)
	data, _ := decodeJSON(t, rec)["data"].([]any)
	if rec.Code != 200 || len(data) != 2 {
		t.Fatalf("member list: %d %s", rec.Code, rec.Body)
	}
	if rec := d.do(t, "GET", base, f.outsider.AccessToken, nil); rec.Code != 404 {
		t.Fatalf("outsider list: %d", rec.Code)
	}
	cases := []struct {
		name, token, target string
		code                int
		errCode             string
	}{
		{"outsider", f.outsider.AccessToken, f.member.Human.ID, 404, "organization_not_found"},
		{"non-owner", f.member.AccessToken, f.owner.Human.ID, 403, "not_org_owner"},
		{"self", f.owner.AccessToken, f.owner.Human.ID, 409, "cannot_remove_self"},
		{"not a member", f.owner.AccessToken, f.outsider.Human.ID, 404, "member_not_found"},
		{"owner removes member", f.owner.AccessToken, f.member.Human.ID, 204, ""},
	}
	for _, tc := range cases {
		rec := d.do(t, "DELETE", base+"/"+tc.target, tc.token, nil)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.errCode) {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	if ok, _ := d.db.IsTenantMember(context.Background(), f.orgID, f.member.Human.ID); ok {
		t.Fatal("member not removed")
	}
}

func TestDashboardInvites(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	ctx := context.Background()
	inv, err := d.db.CreateOrgInvitation(ctx, f.orgID, f.owner.Human.ID, "Carol@Example.com", "h-carol", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/orgs/" + f.orgID + "/invites"
	if rec := d.do(t, "GET", base, f.member.AccessToken, nil); rec.Code != 403 {
		t.Fatalf("member list invites: %d", rec.Code)
	}
	if rec := d.do(t, "GET", base, f.outsider.AccessToken, nil); rec.Code != 404 {
		t.Fatalf("outsider list invites: %d", rec.Code)
	}
	rec := d.do(t, "GET", base, f.owner.AccessToken, nil)
	data, _ := decodeJSON(t, rec)["data"].([]any)
	if rec.Code != 200 || len(data) != 1 || data[0].(map[string]any)["email"] != "Carol@Example.com" {
		t.Fatalf("owner list invites: %d %s", rec.Code, rec.Body)
	}
	if rec := d.do(t, "DELETE", base+"/"+inv.ID, f.member.AccessToken, nil); rec.Code != 403 {
		t.Fatalf("member revoke: %d", rec.Code)
	}
	for i := 0; i < 2; i++ {
		if rec := d.do(t, "DELETE", base+"/"+inv.ID, f.owner.AccessToken, nil); rec.Code != 204 {
			t.Fatalf("owner revoke #%d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := d.do(t, "DELETE", base+"/nope", f.owner.AccessToken, nil); rec.Code != 404 {
		t.Fatalf("unknown invite: %d", rec.Code)
	}
	rec = d.do(t, "GET", base, f.owner.AccessToken, nil)
	if data, _ := decodeJSON(t, rec)["data"].([]any); len(data) != 0 {
		t.Fatalf("still listed after revoke: %s", rec.Body)
	}
}

func TestDashboardAnalytics(t *testing.T) {
	d := newDashAPI(t)
	f := newDashFixture(t, d)
	to := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	from := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	q := "?from=" + from + "&to=" + to
	ov := "/v1/orgs/" + f.orgID + "/analytics/overview" + q
	ts := "/v1/orgs/" + f.orgID + "/analytics/timeseries" + q + "&interval=hour"
	if rec := d.do(t, "GET", ov, f.member.AccessToken, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"counts"`) {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	if rec := d.do(t, "GET", ts, f.member.AccessToken, nil); rec.Code != 200 {
		t.Fatalf("timeseries: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{ov, ts} {
		if rec := d.do(t, "GET", p, f.outsider.AccessToken, nil); rec.Code != 404 {
			t.Fatalf("outsider %s: %d", p, rec.Code)
		}
	}
	// Shared validation path: the API-key handler's range check applies.
	if rec := d.do(t, "GET", "/v1/orgs/"+f.orgID+"/analytics/overview", f.member.AccessToken, nil); rec.Code != 422 {
		t.Fatalf("missing range: %d", rec.Code)
	}
}
