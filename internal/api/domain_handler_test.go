package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
)

type domainTXTResolver struct {
	mu      sync.RWMutex
	records map[string][]string
	err     error
}

func (r *domainTXTResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.records[name]...), r.err
}

func (r *domainTXTResolver) set(name string, values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[name] = values
}

func setupDomainAPI(t *testing.T, scopes []string, resolver *domainTXTResolver) (http.Handler, *database.DB, database.Tenant, string) {
	t.Helper()
	db := newTestDB(t)
	tenant := newTestTenant(t, db)
	authSvc := auth.NewService(db, nil)
	key, _, err := authSvc.Create(context.Background(), tenant.ID, "domains", scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newEmailHandler(db, mustStore(t))
	service := maildomain.NewService(db, resolver)
	mux := newMux(h, authSvc, func() error { return nil }, routeServices{domains: service})
	return authInjector{next: mux, token: key.Raw}, db, tenant, key.Raw
}

func createDomain(t *testing.T, mux http.Handler, name string) domainResource {
	t.Helper()
	rec := doJSON(t, mux, "POST", "/v1/domains", map[string]any{"name": name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create domain got %d: %s", rec.Code, rec.Body.String())
	}
	var resource domainResource
	if err := json.Unmarshal(rec.Body.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	return resource
}

func TestDomainCreateAcceptsParameterizedJSONContentType(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}}
	mux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsWrite)}, resolver)
	rec := doRaw(t, mux, "POST", "/v1/domains", "application/json; charset=utf-8", []byte(`{"name":"example.com"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDomainCreateGetListVerifyDelete(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}}
	mux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, resolver)
	created := createDomain(t, mux, " EXAMPLE.COM. ")
	if created.Name != "example.com" || created.OwnershipState != "pending" || len(created.Records) != 1 {
		t.Fatalf("unexpected create response: %+v", created)
	}
	if rec := doJSON(t, mux, "POST", "/v1/domains", map[string]any{"name": "example.com"}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, mux, "GET", "/v1/domains/"+created.ID, nil); rec.Code != http.StatusOK {
		t.Fatalf("get got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, mux, "GET", "/v1/domains?limit=1", nil); rec.Code != http.StatusOK {
		t.Fatalf("list got %d: %s", rec.Code, rec.Body.String())
	}

	// Missing propagation is a normal 200/pending outcome.
	if rec := doJSON(t, mux, "POST", "/v1/domains/"+created.ID+"/verify", nil); rec.Code != http.StatusOK {
		t.Fatalf("pending verify got %d: %s", rec.Code, rec.Body.String())
	}
	resolver.set(created.Records[0].Name, "unrelated=x", created.Records[0].Value)
	rec := doJSON(t, mux, "POST", "/v1/domains/"+created.ID+"/verify", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify got %d: %s", rec.Code, rec.Body.String())
	}
	var verified domainResource
	if err := json.Unmarshal(rec.Body.Bytes(), &verified); err != nil {
		t.Fatal(err)
	}
	if verified.OwnershipState != "verified" || verified.VerifiedAt == nil || verified.LastCheckedAt == nil {
		t.Fatalf("verification did not persist: %+v", verified)
	}
	attacker := createDomain(t, mux, "attacker.com")
	resolver.set(attacker.Records[0].Name, created.Records[0].Value)
	substitution := doJSON(t, mux, "POST", "/v1/domains/"+attacker.ID+"/verify", nil)
	var notVerified domainResource
	if err := json.Unmarshal(substitution.Body.Bytes(), &notVerified); err != nil || notVerified.OwnershipState != "pending" {
		t.Fatalf("another domain's token verified attacker.com: %+v, %v", notVerified, err)
	}

	// Already verified is monotonic/idempotent even if DNS later disappears.
	resolver.err = errors.New("resolver unavailable")
	if rec := doJSON(t, mux, "POST", "/v1/domains/"+created.ID+"/verify", nil); rec.Code != http.StatusOK {
		t.Fatalf("reverify got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, mux, "DELETE", "/v1/domains/"+created.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, mux, "GET", "/v1/domains/"+created.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted get got %d", rec.Code)
	}
	resolver.err = nil
	if readded := createDomain(t, mux, "example.com"); readded.Records[0].Value == created.Records[0].Value {
		t.Fatal("re-added domain reused deleted challenge")
	}
}

func TestDomainDNSFailureIsRetryable(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}, err: &net.DNSError{IsTemporary: true}}
	mux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, resolver)
	created := createDomain(t, mux, "example.com")
	rec := doJSON(t, mux, "POST", "/v1/domains/"+created.ID+"/verify", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("temporary DNS error got %d: %s", rec.Code, rec.Body.String())
	}
	if body := decodeError(t, rec); body.Error.Code != "dns_lookup_failed" {
		t.Fatalf("unexpected error: %+v", body)
	}
}

func TestDomainScopes(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}}
	readMux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsRead)}, resolver)
	if rec := doJSON(t, readMux, "POST", "/v1/domains", map[string]any{"name": "example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("read key wrote domain: %d", rec.Code)
	}
	writeMux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsWrite)}, resolver)
	created := createDomain(t, writeMux, "example.com")
	if rec := doJSON(t, writeMux, "GET", "/v1/domains/"+created.ID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("write key read domain: %d", rec.Code)
	}
	emailMux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeEmailsRead), string(auth.ScopeEmailsSend)}, resolver)
	if rec := doJSON(t, emailMux, "GET", "/v1/domains", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("legacy email-only key gained domain access: %d", rec.Code)
	}
}

func TestDomainListPagination(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}}
	mux, _, _, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, resolver)
	for _, name := range []string{"one.example.com", "two.example.com", "three.example.com"} {
		createDomain(t, mux, name)
	}
	first := doJSON(t, mux, "GET", "/v1/domains?limit=2", nil)
	var page domainList
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || page.NextCursor == nil {
		t.Fatalf("unexpected first page: %+v", page)
	}
	second := doJSON(t, mux, "GET", "/v1/domains?limit=2&cursor="+*page.NextCursor, nil)
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.NextCursor != nil {
		t.Fatalf("unexpected second page: %+v", page)
	}
}

func TestCrossTenantPendingClaimAndIsolation(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}}
	db := newTestDB(t)
	a, err := db.CreateTenant(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateTenant(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(db, nil)
	scopes := []string{string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}
	keyA, _, _ := authSvc.Create(context.Background(), a.ID, "a", scopes, nil)
	keyB, _, _ := authSvc.Create(context.Background(), b.ID, "b", scopes, nil)
	h := newEmailHandler(db, mustStore(t))
	mux := newMux(h, authSvc, func() error { return nil }, routeServices{domains: maildomain.NewService(db, resolver)})
	aMux := authInjector{next: mux, token: keyA.Raw}
	bMux := authInjector{next: mux, token: keyB.Raw}
	da := createDomain(t, aMux, "example.com")
	dbDomain := createDomain(t, bMux, "EXAMPLE.COM.")
	var aList domainList
	listRec := doJSON(t, aMux, "GET", "/v1/domains", nil)
	if err := json.Unmarshal(listRec.Body.Bytes(), &aList); err != nil || len(aList.Data) != 1 || aList.Data[0].ID != da.ID {
		t.Fatalf("tenant-scoped list leaked or omitted a domain: %+v, %v", aList, err)
	}
	if rec := doJSON(t, aMux, "GET", "/v1/domains/"+dbDomain.ID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get leaked resource: %d", rec.Code)
	}
	resolver.set(dbDomain.Records[0].Name, dbDomain.Records[0].Value)
	if rec := doJSON(t, bMux, "POST", "/v1/domains/"+dbDomain.ID+"/verify", nil); rec.Code != http.StatusOK {
		t.Fatalf("legitimate pending claimant could not verify: %d: %s", rec.Code, rec.Body.String())
	}
	resolver.set(da.Records[0].Name, da.Records[0].Value)
	if rec := doJSON(t, aMux, "POST", "/v1/domains/"+da.ID+"/verify", nil); rec.Code != http.StatusConflict {
		t.Fatalf("second verified owner got %d: %s", rec.Code, rec.Body.String())
	}
	for _, methodPath := range [][2]string{{"POST", "/v1/domains/" + dbDomain.ID + "/verify"}, {"DELETE", "/v1/domains/" + dbDomain.ID}} {
		if rec := doJSON(t, aMux, methodPath[0], methodPath[1], nil); rec.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant %s leaked resource: %d", methodPath[0], rec.Code)
		}
	}
}

func TestDomainRestartPersistence(t *testing.T) {
	resolver := &domainTXTResolver{records: map[string][]string{}}
	mux, db, tenant, _ := setupDomainAPI(t, []string{string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, resolver)
	created := createDomain(t, mux, "example.com")
	// Reconstructing the service simulates losing all process memory.
	restarted := maildomain.NewService(db, resolver)
	pending, err := restarted.Get(context.Background(), tenant.ID, created.ID)
	if err != nil || maildomain.RecordValue(pending.VerificationToken) != created.Records[0].Value {
		t.Fatalf("pending challenge was not restart-safe: %+v, %v", pending, err)
	}
	resolver.set(created.Records[0].Name, created.Records[0].Value)
	verified, err := restarted.Verify(context.Background(), tenant.ID, created.ID)
	if err != nil || verified.VerificationStatus != database.DomainVerified {
		t.Fatalf("verify after restart failed: %+v, %v", verified, err)
	}
	if got, err := maildomain.NewService(db, resolver).Get(context.Background(), tenant.ID, created.ID); err != nil || got.VerificationStatus != database.DomainVerified {
		t.Fatalf("verified state was not restart-safe: %+v, %v", got, err)
	}
}
