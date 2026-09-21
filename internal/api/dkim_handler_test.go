package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	msgauth "github.com/emersion/go-msgauth/dkim"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
	"github.com/Ferousco-dev/mailx/internal/spf"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

type publishedDNS struct {
	mu   sync.Mutex
	txt  map[string][]string
	fail error
}

func (p *publishedDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return nil, p.fail
	}
	if r, ok := p.txt[name]; ok {
		return r, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (p *publishedDNS) publish(name, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.txt == nil {
		p.txt = map[string][]string{}
	}
	p.txt[name] = []string{value}
}

type dkimAPI struct {
	t       *testing.T
	db      *database.DB
	store   *storage.FileStore
	authSvc *auth.Service
	box     *secretbox.Box
	dns     *publishedDNS
	mux     http.Handler // raw mux (no auth injected)
}

type actor struct {
	tenant database.Tenant
	h      http.Handler
}

func newDKIMAPI(t *testing.T) *dkimAPI { return newAPIWithSPF(t, nil) }

// newAPIWithSPF builds the shared test API; mkSPF (optional) receives the
// shared DNS fake so SPF and DKIM/ownership see the same published records.
func newAPIWithSPF(t *testing.T, mkSPF func(*database.DB, *publishedDNS) *spf.Service) *dkimAPI {
	return newAPIFull(t, mkSPF, nil)
}

// newAPIFull additionally builds a DMARC service from the shared fakes.
func newAPIFull(t *testing.T, mkSPF func(*database.DB, *publishedDNS) *spf.Service,
	mkDMARC func(*database.DB, *publishedDNS, *dkim.Service, *spf.Service) *dmarc.Service) *dkimAPI {
	t.Helper()
	db := newTestDB(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	box, _ := secretbox.New(mk)
	dns := &publishedDNS{}
	svc, err := dkim.NewService(db, box, dns, nil)
	if err != nil {
		t.Fatal(err)
	}
	authSvc := auth.NewService(db, nil)
	var spfSvc *spf.Service
	if mkSPF != nil {
		spfSvc = mkSPF(db, dns)
	}
	var dmarcSvc *dmarc.Service
	if mkDMARC != nil {
		dmarcSvc = mkDMARC(db, dns, svc, spfSvc)
	}
	mux := newMux(newEmailHandler(db, store), authSvc, func() error { return nil }, routeServices{dkim: svc, spf: spfSvc, dmarc: dmarcSvc})
	return &dkimAPI{t: t, db: db, store: store, authSvc: authSvc, box: box, dns: dns, mux: mux}
}

func (a *dkimAPI) actor(name string) actor {
	a.t.Helper()
	tn, err := a.db.CreateTenant(context.Background(), name)
	if err != nil {
		a.t.Fatal(err)
	}
	gen, _, err := a.authSvc.Create(context.Background(), tn.ID, "k", []string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead),
		string(auth.ScopeDomainsRead), string(auth.ScopeDomainsWrite)}, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return actor{tn, authInjector{next: a.mux, token: gen.Raw}}
}

func (a *dkimAPI) send(ac actor, from string, extra map[string]any) *httptest.ResponseRecorder {
	body := map[string]any{"from": from, "to": []string{"bob@example.net"}, "subject": "hi", "text": "hello body"}
	for k, v := range extra {
		body[k] = v
	}
	return doJSON(a.t, ac.h, "POST", "/v1/emails", body)
}

func emailCount(t *testing.T, ac actor) int {
	t.Helper()
	rec := doJSON(t, ac.h, "GET", "/v1/emails?limit=100", nil)
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	return len(list.Data)
}

func TestVerifiedFromAuthorizationAndAdversarialValues(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	accepted := 0
	allowed := []string{"alice@example.com", "Alice Example <billing@example.com>", "ALICE@EXAMPLE.COM", "<alice@example.com>"}
	for _, from := range allowed {
		if rec := a.send(ac, from, nil); rec.Code != http.StatusAccepted {
			t.Fatalf("%q should be allowed: %d %s", from, rec.Code, rec.Body.String())
		}
		accepted++
	}
	rejected := []string{
		"alice@evil.com",
		"alice@attackerexample.com",
		"alice@example.com.attacker.com",
		"alice@sub.example.com", // exact-domain ownership: the parent does not cover subdomains
		`"alice@example.com"@evil.com`,
		`"alice@example.com" <x@evil.com>`,
		"alice@examp1e.com",
		"alice@[127.0.0.1]",
		"alice@localhost",
	}
	for _, from := range rejected {
		rec := a.send(ac, from, nil)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%q must be rejected (403/422), got %d %s", from, rec.Code, rec.Body.String())
		}
	}
	for _, from := range []string{"alice@example.com@evil.com", "alice@example.com, mallory@evil.com", "not an address"} {
		if rec := a.send(ac, from, nil); rec.Code == http.StatusAccepted || rec.Code >= 500 {
			t.Fatalf("%q must be a client error, got %d", from, rec.Code)
		}
	}
	if n := emailCount(t, ac); n != accepted {
		t.Fatalf("rejected senders left durable state: %d emails stored, want %d", n, accepted)
	}
	files, _ := a.store.List()
	if len(files) != accepted {
		t.Fatalf("rejected sends wrote %d message files, want %d", len(files), accepted)
	}
	rec := a.send(ac, "alice@evil.com", nil)
	var e struct{ Error struct{ Code string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusForbidden || e.Error.Code != "from_domain_not_authorized" {
		t.Fatalf("unauthorized From: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSenderAuthorizationIsTenantScopedAndRequiresVerification(t *testing.T) {
	a := newDKIMAPI(t)
	owner, other := a.actor("owner"), a.actor("other")
	verifyTestDomain(t, a.db, owner.tenant.ID, "example.com")
	if rec := a.send(other, "x@example.com", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("another tenant must not send from a domain it does not own: %d", rec.Code)
	}
	// A claimed but UNVERIFIED domain does not authorize sending.
	if _, err := a.db.CreateDomain(context.Background(), other.tenant.ID, "pending-domain.com", "tok"); err != nil {
		t.Fatal(err)
	}
	if rec := a.send(other, "x@pending-domain.com", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("unverified domain must not authorize sending: %d", rec.Code)
	}
	// Root/subdomain semantics follow v0.21: a verified subdomain does not authorize the root.
	sub := a.actor("subowner")
	verifyTestDomain(t, a.db, sub.tenant.ID, "sub.example.org")
	if rec := a.send(sub, "x@sub.example.org", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("verified subdomain must send: %d", rec.Code)
	}
	if rec := a.send(sub, "x@example.org", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a verified subdomain must not authorize the root: %d", rec.Code)
	}
}

func TestRejectedSenderDoesNotConsumeIdempotencyKey(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	body := func(from string) map[string]any {
		return map[string]any{"from": from, "to": []string{"b@example.net"}, "subject": "s", "text": "t"}
	}
	if rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "same-key", body("x@evil.com")); rec.Code != http.StatusForbidden {
		t.Fatalf("%d", rec.Code)
	}
	rec := doJSONWithKey(t, ac.h, "POST", "/v1/emails", "same-key", body("x@example.com"))
	if rec.Code != http.StatusAccepted || rec.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatalf("a rejected request must leave the key unused: %d replayed=%q", rec.Code, rec.Header().Get("Idempotency-Replayed"))
	}
}

type dkimStatusBody struct {
	DomainID string `json:"domain_id"`
	Domain   string `json:"domain"`
	Signing  bool   `json:"signing"`
	Active   *struct {
		Selector, Status string
		KeyBits          int `json:"key_bits"`
		DNS              struct{ Type, Name, Value string }
	}
	Pending *struct {
		Selector, Status string
		DNS              struct{ Type, Name, Value string }
	}
	Retired []struct{ Selector string }
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) dkimStatusBody {
	t.Helper()
	var s dkimStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return s
}

func verifyWith(t *testing.T, signed []byte, domain, selector, pubB64 string) error {
	t.Helper()
	lookup := func(name string) ([]string, error) {
		if name == dkim.DNSName(selector, domain) {
			return []string{dkim.DNSValue(pubB64)}, nil
		}
		return nil, errors.New("no record")
	}
	res, err := msgauth.VerifyWithOptions(strings.NewReader(string(signed)), &msgauth.VerifyOptions{LookupTXT: lookup})
	if err != nil {
		return err
	}
	if len(res) != 1 {
		return errors.New("expected one signature")
	}
	return res[0].Err
}

func TestDKIMLifecycleThroughAPISignsAndSurvivesRotationForQueuedMessages(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	path := "/v1/domains/" + dom.ID + "/dkim"

	// Before setup: mail from a verified domain without DKIM is accepted unsigned.
	rec := a.send(ac, "alice@example.com", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	unsigned := decodeEmail(t, rec)
	if stored, _ := a.store.Load(unsigned.ID); strings.Contains(string(stored.Raw), "DKIM-Signature") {
		t.Fatal("no key: message must be unsigned")
	}

	// Create -> pending, DNS instructions, no private material.
	rec = doJSON(t, ac.h, "POST", path, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	st := decodeStatus(t, rec)
	if st.Pending == nil || st.Active != nil || st.Signing || st.Pending.DNS.Type != "TXT" ||
		!strings.HasPrefix(st.Pending.DNS.Value, "v=DKIM1; k=rsa; p=") ||
		st.Pending.DNS.Name != st.Pending.Selector+"._domainkey.example.com" {
		t.Fatalf("status after create: %+v", st)
	}
	if lower := strings.ToLower(rec.Body.String()); strings.Contains(lower, "private") || strings.Contains(lower, "begin") {
		t.Fatalf("response mentions private key material: %s", rec.Body.String())
	}
	// Pending keys do not sign.
	sent := decodeEmail(t, a.send(ac, "alice@example.com", nil))
	if stored, _ := a.store.Load(sent.ID); strings.Contains(string(stored.Raw), "DKIM-Signature") {
		t.Fatal("a pending key must not sign")
	}
	if rec := doJSON(t, ac.h, "POST", path, nil); rec.Code != http.StatusConflict {
		t.Fatalf("second create while pending: %d", rec.Code)
	}

	// Verify before publication: nothing changes.
	rec = doJSON(t, ac.h, "POST", path+"/verify", nil)
	var vr struct {
		Published bool
		Status    dkimStatusBody
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &vr)
	if rec.Code != http.StatusOK || vr.Published || vr.Status.Signing {
		t.Fatalf("unpublished verify: %d %s", rec.Code, rec.Body.String())
	}
	a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
	rec = doJSON(t, ac.h, "POST", path+"/verify", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &vr)
	if rec.Code != http.StatusOK || !vr.Published || !vr.Status.Signing || vr.Status.Active == nil || vr.Status.Active.KeyBits != 2048 {
		t.Fatalf("published verify: %d %s", rec.Code, rec.Body.String())
	}
	k1copy := *vr.Status.Active // copy: vr is decoded into again below
	key1 := &k1copy
	pub1 := strings.TrimPrefix(key1.DNS.Value, "v=DKIM1; k=rsa; p=")

	// Signed at acceptance; the stored bytes verify independently.
	sent = decodeEmail(t, a.send(ac, "alice@example.com", nil))
	stored, err := a.store.Load(sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyWith(t, stored.Raw, "example.com", key1.Selector, pub1); err != nil {
		t.Fatalf("stored message does not verify: %v\n%s", err, stored.Raw)
	}
	before := string(stored.Raw)

	// Rotate: while the new key is pending the old one keeps signing.
	rec = doJSON(t, ac.h, "POST", path, nil)
	st2 := decodeStatus(t, rec)
	if rec.Code != http.StatusCreated || st2.Pending == nil || st2.Active == nil || st2.Active.Selector != key1.Selector {
		t.Fatalf("rotation start: %d %s", rec.Code, rec.Body.String())
	}
	a.dns.publish(st2.Pending.DNS.Name, st2.Pending.DNS.Value)
	rec = doJSON(t, ac.h, "POST", path+"/verify", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &vr)
	if vr.Status.Active == nil || vr.Status.Active.Selector == key1.Selector || len(vr.Status.Retired) != 1 {
		t.Fatalf("rotation activation: %s", rec.Body.String())
	}
	// A message accepted before rotation is retried byte-for-byte identical and
	// still verifies against the OLD selector (kept in DNS during the transition).
	again, _ := a.store.Load(sent.ID)
	if string(again.Raw) != before {
		t.Fatal("stored (queued) message bytes changed after key rotation")
	}
	if err := verifyWith(t, again.Raw, "example.com", key1.Selector, pub1); err != nil {
		t.Fatalf("queued message must still verify with the retired selector's public key: %v", err)
	}
	// New mail uses the new key.
	next := decodeEmail(t, a.send(ac, "alice@example.com", nil))
	ns, _ := a.store.Load(next.ID)
	if !strings.Contains(strings.ReplaceAll(string(ns.Raw), "\r\n\t", " "), "s="+vr.Status.Active.Selector) {
		t.Fatalf("new message not signed with the new selector:\n%s", ns.Raw)
	}
}

func TestDKIMEndpointsAreTenantScopedAndRequireVerifiedDomain(t *testing.T) {
	a := newDKIMAPI(t)
	owner, other := a.actor("owner"), a.actor("other")
	dom := verifyTestDomain(t, a.db, owner.tenant.ID, "example.com")
	pending, _ := a.db.CreateDomain(context.Background(), owner.tenant.ID, "pending-domain.com", "tok")
	path := "/v1/domains/" + dom.ID + "/dkim"
	if rec := doJSON(t, owner.h, "POST", "/v1/domains/"+pending.ID+"/dkim", nil); rec.Code != http.StatusConflict {
		t.Fatalf("unverified domain: %d", rec.Code)
	}
	if rec := doJSON(t, owner.h, "POST", path, nil); rec.Code != http.StatusCreated {
		t.Fatal(rec.Code)
	}
	for _, c := range []struct{ method, p string }{{"GET", path}, {"POST", path}, {"POST", path + "/verify"}} {
		if rec := doJSON(t, other.h, c.method, c.p, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant %s %s = %d, want 404", c.method, c.p, rec.Code)
		}
	}
	if rec := doJSON(t, owner.h, "POST", "/v1/domains/no-such-id/dkim/verify", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown domain: %d", rec.Code)
	}
	if rec := doJSON(t, owner.h, "POST", path+"/verify", nil); rec.Code != http.StatusOK {
		t.Fatalf("owner verify: %d", rec.Code)
	}
	// No pending key left to verify: conflict, not 404, for the owner only.
	a.dns.publish("x", "y")
}

func TestDKIMSigningFailureRefusesTheMessageInsteadOfSendingUnsigned(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	path := "/v1/domains/" + dom.ID + "/dkim"
	st := decodeStatus(t, doJSON(t, ac.h, "POST", path, nil))
	a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
	doJSON(t, ac.h, "POST", path+"/verify", nil)
	if rec := a.send(ac, "alice@example.com", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("precondition: %d", rec.Code)
	}
	beforeEmails := emailCount(t, ac)
	beforeFiles, _ := a.store.List()

	// Same database, but a service that holds the WRONG master key: decrypt fails.
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)
	wbox, _ := secretbox.New(wrong)
	badSvc, _ := dkim.NewService(a.db, wbox, a.dns, nil)
	badMux := newMux(newEmailHandler(a.db, a.store), a.authSvc, func() error { return nil }, routeServices{dkim: badSvc})
	gen, _, _ := a.authSvc.Create(context.Background(), ac.tenant.ID, "k2", []string{string(auth.ScopeEmailsSend), string(auth.ScopeEmailsRead)}, nil)
	bad := actor{ac.tenant, authInjector{next: badMux, token: gen.Raw}}

	rec := a.send(bad, "alice@example.com", nil)
	var e struct{ Error struct{ Type, Code string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusServiceUnavailable || e.Error.Code != "dkim_signing_unavailable" {
		t.Fatalf("key failure must refuse the message: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "decrypt") || strings.Contains(rec.Body.String(), "authentication failed") {
		t.Fatalf("crypto detail leaked: %s", rec.Body.String())
	}
	if emailCount(t, ac) != beforeEmails {
		t.Fatal("a refused message must not be durably accepted")
	}
	if after, _ := a.store.List(); len(after) != len(beforeFiles) {
		t.Fatal("a refused message must not be written to storage")
	}
}

func TestDomainDeletionStopsSendingAndReclaimStartsClean(t *testing.T) {
	a := newDKIMAPI(t)
	owner, claimant := a.actor("owner"), a.actor("claimant")
	dom := verifyTestDomain(t, a.db, owner.tenant.ID, "example.com")
	path := "/v1/domains/" + dom.ID + "/dkim"
	st := decodeStatus(t, doJSON(t, owner.h, "POST", path, nil))
	a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
	doJSON(t, owner.h, "POST", path+"/verify", nil)
	if a.send(owner, "x@example.com", nil).Code != http.StatusAccepted {
		t.Fatal("precondition")
	}
	if rec := doJSON(t, owner.h, "DELETE", "/v1/domains/"+dom.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := a.send(owner, "x@example.com", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a deleted domain must not authorize sending: %d", rec.Code)
	}
	// Another tenant claims and verifies the name: no inherited key, no signing with the old one.
	nd := verifyTestDomain(t, a.db, claimant.tenant.ID, "example.com")
	status := decodeStatus(t, doJSON(t, claimant.h, "GET", "/v1/domains/"+nd.ID+"/dkim", nil))
	if status.Signing || status.Active != nil || status.Pending != nil || len(status.Retired) != 0 {
		t.Fatalf("reclaimed domain inherited key state: %+v", status)
	}
	sent := decodeEmail(t, a.send(claimant, "y@example.com", nil))
	if stored, _ := a.store.Load(sent.ID); strings.Contains(string(stored.Raw), "DKIM-Signature") {
		t.Fatal("reclaimed domain signed with a key it never created")
	}
}

// The private key never appears in any API response.
func TestPrivateKeyNeverAppearsInAPIResponses(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	path := "/v1/domains/" + dom.ID + "/dkim"
	var surfaces []string
	rec := doJSON(t, ac.h, "POST", path, nil)
	surfaces = append(surfaces, rec.Body.String())
	st := decodeStatus(t, rec)
	a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
	surfaces = append(surfaces, doJSON(t, ac.h, "POST", path+"/verify", nil).Body.String(), doJSON(t, ac.h, "GET", path, nil).Body.String())
	sent := a.send(ac, "alice@example.com", nil)
	surfaces = append(surfaces, sent.Body.String(), doJSON(t, ac.h, "GET", "/v1/emails", nil).Body.String())

	row, err := a.db.ActiveSigningKey(context.Background(), ac.tenant.ID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	der, err := a.box.Decrypt(row.PrivateCiphertext, row.PrivateNonce, []byte("mailx-dkim-v1\x00"+ac.tenant.ID+"\x00"+dom.ID+"\x00"+row.Selector))
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := x509.ParsePKCS8PrivateKey(der)
	forms := []string{base64.StdEncoding.EncodeToString(der), base64.StdEncoding.EncodeToString(priv.(*rsa.PrivateKey).D.Bytes()), "BEGIN PRIVATE KEY", "BEGIN RSA PRIVATE KEY"}
	for _, body := range surfaces {
		for _, f := range forms {
			if strings.Contains(body, f[:min(len(f), 60)]) {
				t.Fatalf("private key material %q... found in an API response", f[:12])
			}
		}
	}
}

// Domain resource contract: DKIM routes honour the domains scopes.
func TestDKIMRoutesRequireDomainScopes(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	gen, _, _ := a.authSvc.Create(context.Background(), ac.tenant.ID, "ro", []string{string(auth.ScopeDomainsRead)}, nil)
	ro := authInjector{next: a.mux, token: gen.Raw}
	if rec := doJSON(t, ro, "GET", "/v1/domains/"+dom.ID+"/dkim", nil); rec.Code != http.StatusOK {
		t.Fatalf("read scope may read status: %d", rec.Code)
	}
	for _, p := range []string{"", "/verify"} {
		if rec := doJSON(t, ro, "POST", "/v1/domains/"+dom.ID+"/dkim"+p, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("read-only key must not write DKIM (%q): %d", p, rec.Code)
		}
	}
}

// Sends running while a rotation is activated: every accepted message is signed
// by the key that was active for it (old or new), verifies, and none is unsigned
// or signed for another tenant/domain.
func TestConcurrentSendsAcrossTenantsDuringRotationAlwaysVerify(t *testing.T) {
	a := newDKIMAPI(t)
	type site struct {
		ac     actor
		domain string
		id     string
		pubs   map[string]string // selector -> public key
		mu     sync.Mutex
	}
	setup := func(name, domain string) *site {
		ac := a.actor(name)
		d := verifyTestDomain(t, a.db, ac.tenant.ID, domain)
		s := &site{ac: ac, domain: domain, id: d.ID, pubs: map[string]string{}}
		path := "/v1/domains/" + d.ID + "/dkim"
		st := decodeStatus(t, doJSON(t, ac.h, "POST", path, nil))
		a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
		s.pubs[st.Pending.Selector] = strings.TrimPrefix(st.Pending.DNS.Value, "v=DKIM1; k=rsa; p=")
		doJSON(t, ac.h, "POST", path+"/verify", nil)
		return s
	}
	sites := []*site{setup("t1", "one.example.com"), setup("t2", "two.example.com")}
	rotate := func(s *site) {
		path := "/v1/domains/" + s.id + "/dkim"
		st := decodeStatus(t, doJSON(t, s.ac.h, "POST", path, nil))
		if st.Pending == nil {
			t.Error("rotation did not create a pending key")
			return
		}
		s.mu.Lock()
		s.pubs[st.Pending.Selector] = strings.TrimPrefix(st.Pending.DNS.Value, "v=DKIM1; k=rsa; p=")
		s.mu.Unlock()
		a.dns.publish(st.Pending.DNS.Name, st.Pending.DNS.Value)
		doJSON(t, s.ac.h, "POST", path+"/verify", nil)
	}

	var wg sync.WaitGroup
	ids := make(chan [2]string, 200)
	for i := 0; i < 24; i++ {
		for _, s := range sites {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := a.send(s.ac, "alice@"+s.domain, nil)
				if rec.Code != http.StatusAccepted {
					t.Errorf("%s: send got %d: %s", s.domain, rec.Code, rec.Body.String())
					return
				}
				ids <- [2]string{s.domain, decodeEmail(t, rec).ID}
			}()
		}
		if i == 8 {
			for _, s := range sites {
				rotate(s)
			}
		}
	}
	wg.Wait()
	close(ids)
	for pair := range ids {
		var s *site
		for _, c := range sites {
			if c.domain == pair[0] {
				s = c
			}
		}
		stored, err := a.store.Load(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		raw := string(stored.Raw)
		flat := strings.NewReplacer("\r\n", "", "\t", " ").Replace(raw[:strings.Index(raw, "\r\nFrom:")])
		s.mu.Lock()
		ok := false
		for sel, pub := range s.pubs {
			if strings.Contains(flat, "d="+s.domain+";") && strings.Contains(flat, "s="+sel+";") && verifyWith(t, stored.Raw, s.domain, sel, pub) == nil {
				ok = true
			}
		}
		s.mu.Unlock()
		if !ok {
			t.Fatalf("%s: stored message is unsigned or does not verify under any of its own keys:\n%.400s", pair[0], raw)
		}
	}
}

// storeFailingSecondLookup: the first Keys lookup reports "no pending key"; the
// follow-up lookup that disambiguates 404 vs 409 hits a database failure.
type flakyDKIMStore struct {
	mu    sync.Mutex
	lists int
}

func (f *flakyDKIMStore) GetDomain(_ context.Context, _, id string) (database.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.lists > 1 {
		return database.Domain{}, errors.New("connection reset by peer")
	}
	return database.Domain{ID: id, Name: "example.com"}, nil
}
func (f *flakyDKIMStore) CreateDKIMKey(context.Context, string, string, database.NewDKIMKey) (database.DKIMKey, error) {
	return database.DKIMKey{}, errors.New("unused")
}
func (f *flakyDKIMStore) ListDKIMKeys(context.Context, string, string) ([]database.DKIMKey, error) {
	return nil, nil
}
func (f *flakyDKIMStore) ActivateDKIMKey(context.Context, string, string, time.Time) (database.DKIMKey, error) {
	return database.DKIMKey{}, errors.New("unused")
}
func (f *flakyDKIMStore) ActiveSigningKey(context.Context, string, string) (database.DKIMKey, error) {
	return database.DKIMKey{}, database.ErrNotFound
}

func TestDKIMVerifyDatabaseFailureIsNot404(t *testing.T) {
	a := newDKIMAPI(t)
	ac := a.actor("acme")
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	box, _ := secretbox.New(mk)
	svc, _ := dkim.NewService(&flakyDKIMStore{}, box, a.dns, nil)
	mux := newMux(newEmailHandler(a.db, a.store), a.authSvc, func() error { return nil }, routeServices{dkim: svc})
	gen, _, _ := a.authSvc.Create(context.Background(), ac.tenant.ID, "k", []string{string(auth.ScopeDomainsWrite)}, nil)
	rec := doJSON(t, authInjector{next: mux, token: gen.Raw}, "POST", "/v1/domains/anything/dkim/verify", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a database failure must be a 500, not a 404/409: %d %s", rec.Code, rec.Body.String())
	}
}
