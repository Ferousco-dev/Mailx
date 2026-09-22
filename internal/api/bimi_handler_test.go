package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/bimi"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

type stubBIMIStore struct{ db *database.DB }

func (s stubBIMIStore) GetDomain(ctx context.Context, tenant, id string) (database.Domain, error) {
	return s.db.GetDomain(ctx, tenant, id)
}

type stubBIMIDNS struct {
	answer func(name string) ([]string, error)
}

func (d stubBIMIDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	return d.answer(name)
}

type stubBIMIDMARC struct{ view bimi.DMARCPrereq }

func (d stubBIMIDMARC) DMARCView(context.Context, string, string) (bimi.DMARCPrereq, error) {
	return d.view, nil
}

type bimiTestAPI struct {
	t       *testing.T
	db      *database.DB
	authSvc *auth.Service
	mux     *http.ServeMux
}

func newBIMITestAPI(t *testing.T, dnsAnswer func(name string) ([]string, error), dmarcView bimi.DMARCPrereq) *bimiTestAPI {
	t.Helper()
	db := newTestDB(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := bimi.NewService(stubBIMIStore{db: db}, stubBIMIDNS{answer: dnsAnswer}, stubBIMIDMARC{view: dmarcView}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(db, nil)
	eh := newEmailHandler(db, store)
	mux := newMux(eh, authSvc, func() error { return nil }, routeServices{bimi: svc})
	return &bimiTestAPI{t: t, db: db, authSvc: authSvc, mux: mux}
}

func (a *bimiTestAPI) actor(name string) actor {
	a.t.Helper()
	tn, err := a.db.CreateTenant(context.Background(), name)
	if err != nil {
		a.t.Fatal(err)
	}
	gen, _, err := a.authSvc.Create(context.Background(), tn.ID, "k", []string{string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return actor{tn, authInjector{next: a.mux, token: gen.Raw}}
}

func TestBIMIHandleGetNoDNS(t *testing.T) {
	api := newBIMITestAPI(t, func(string) ([]string, error) {
		t.Fatal("GET must not query DNS")
		return nil, nil
	}, bimi.DMARCPrereq{})
	ac := api.actor("t1")
	dom, err := api.db.CreateDomain(context.Background(), ac.tenant.ID, "example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/bimi", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Readiness  string `json:"readiness"`
		Disclaimer string `json:"disclaimer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Readiness != bimi.ReadinessUnchecked {
		t.Fatalf("readiness = %q", got.Readiness)
	}
	if got.Disclaimer == "" {
		t.Fatal("expected a disclaimer")
	}
}

func TestBIMIHandleVerifyRequiresVerifiedDomain(t *testing.T) {
	api := newBIMITestAPI(t, func(string) ([]string, error) { return nil, nil }, bimi.DMARCPrereq{})
	ac := api.actor("t1")
	dom, err := api.db.CreateDomain(context.Background(), ac.tenant.ID, "example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/bimi/verify", nil)
	if rec.Code != 409 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBIMIHandleVerifyTenantIsolation(t *testing.T) {
	api := newBIMITestAPI(t, func(string) ([]string, error) { return nil, nil }, bimi.DMARCPrereq{})
	owner := api.actor("owner")
	other := api.actor("other")
	dom, err := api.db.CreateDomain(context.Background(), owner.tenant.ID, "example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, other.h, "GET", "/v1/domains/"+dom.ID+"/bimi", nil)
	if rec.Code != 404 {
		t.Fatalf("cross-tenant GET must 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBIMIHandleVerifyReadyEndToEnd(t *testing.T) {
	api := newBIMITestAPI(t, func(name string) ([]string, error) {
		if name == "default._bimi.example.com" {
			return []string{"v=BIMI1; l=https://example.com/logo.svg"}, nil
		}
		return nil, nil
	}, bimi.DMARCPrereq{Checked: true, EffectivePolicy: "reject", OrgDomain: "example.com"})
	ac := api.actor("t1")
	dom, err := api.db.CreateDomain(context.Background(), ac.tenant.ID, "example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.db.RecordDomainCheck(context.Background(), ac.tenant.ID, dom.ID, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/bimi/verify", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got bimiResource
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Readiness != bimi.ReadinessReady {
		t.Fatalf("got %+v", got)
	}
	if got.Logo.Checked {
		t.Fatal("asset validation must be opt-in; default verify must not fetch the logo")
	}
}
