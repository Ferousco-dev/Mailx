package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/spf"
)

var testSendingIP = netip.MustParseAddr("8.8.8.8") // public address, never contacted

func spfAPI(t *testing.T, cfg spf.Config) *dkimAPI {
	t.Helper()
	return newAPIWithSPF(t, func(db *database.DB, dns *publishedDNS) *spf.Service {
		svc, err := spf.NewService(db, dns, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		return svc
	})
}

func directSPF() spf.Config {
	return spf.Config{Mode: spf.ModeDirect, IPs: []netip.Addr{testSendingIP}}
}

func decodeSPF(t *testing.T, rec interface {
	Bytes() []byte
}) spfResource {
	t.Helper()
	var out spfResource
	if err := json.Unmarshal(rec.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSPFEndpointsGuideAndVerifyThroughAPI(t *testing.T) {
	a := spfAPI(t, directSPF())
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	base := "/v1/domains/" + dom.ID + "/spf"

	rec := doJSON(t, ac.h, "GET", base, nil)
	got := decodeSPF(t, rec.Body)
	if rec.Code != 200 || got.Checked || got.Status != "unchecked" || got.Expected.Action != "create" ||
		got.Expected.Value != "v=spf1 ip4:8.8.8.8 ~all" || got.Expected.Name != "example.com" || len(got.Sending.IPs) != 1 {
		t.Fatalf("GET: %d %+v", rec.Code, got)
	}

	steps := []struct {
		publish func()
		status  string
		action  string
	}{
		{func() {}, "not_configured", "create"},
		{func() { a.dns.setTXT("example.com", "v=spf1 include:_spf.google.com ~all") }, "mismatch", "update_existing"},
		{func() { a.dns.setTXT("example.com", "v=spf1 include:_spf.google.com ~all", "v=spf1 -all") }, "conflict", "merge_records"},
		{func() { a.dns.setTXT("example.com", "v=spf1 ip4:bad") }, "invalid", "fix_record"},
		{func() { a.dns.setTXT("example.com", "v=spf1 ip4:8.8.8.8 include:_spf.google.com ~all") }, "verified", "none"},
	}
	for _, st := range steps {
		st.publish()
		rec := doJSON(t, ac.h, "POST", base+"/verify", nil)
		got := decodeSPF(t, rec.Body)
		if rec.Code != 200 || got.Status != st.status || got.Expected.Action != st.action || !got.Checked {
			t.Fatalf("%s: %d %s", st.status, rec.Code, rec.Body.String())
		}
	}
	// The mismatch suggestion extends the existing record rather than adding one.
	a.dns.setTXT("example.com", "v=spf1 include:_spf.google.com ~all")
	got = decodeSPF(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body)
	if got.Expected.Value != "v=spf1 ip4:8.8.8.8 include:_spf.google.com ~all" {
		t.Fatalf("merged = %q", got.Expected.Value)
	}
	// Temporary DNS failure: a 200 with temporary_error, never a misconfiguration.
	a.dns.mu.Lock()
	a.dns.fail = &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	a.dns.mu.Unlock()
	rec = doJSON(t, ac.h, "POST", base+"/verify", nil)
	if got := decodeSPF(t, rec.Body); rec.Code != 200 || got.Status != "temporary_error" || strings.Contains(rec.Body.String(), "misbehaving") {
		t.Fatalf("temporary: %d %s", rec.Code, rec.Body.String())
	}
}

func (p *publishedDNS) setTXT(name string, values ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.txt == nil {
		p.txt = map[string][]string{}
	}
	p.txt[name] = values
}

func TestSPFEndpointsTenantIsolationAndOwnership(t *testing.T) {
	a := spfAPI(t, directSPF())
	owner, other := a.actor("owner"), a.actor("other")
	dom := verifyTestDomain(t, a.db, owner.tenant.ID, "example.com")
	pending, _ := a.db.CreateDomain(context.Background(), owner.tenant.ID, "pending-domain.com", "tok")
	for _, c := range []struct{ method, p string }{{"GET", "/spf"}, {"POST", "/spf/verify"}} {
		if rec := doJSON(t, other.h, c.method, "/v1/domains/"+dom.ID+c.p, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant %s %s = %d, want 404", c.method, c.p, rec.Code)
		}
	}
	if rec := doJSON(t, owner.h, "POST", "/v1/domains/no-such/spf/verify", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", rec.Code)
	}
	if rec := doJSON(t, owner.h, "POST", "/v1/domains/"+pending.ID+"/spf/verify", nil); rec.Code != http.StatusConflict {
		t.Fatalf("unverified domain: %d", rec.Code)
	}
	// Tenant A's SPF state is not visible through tenant B's own domain of another name.
	a.dns.setTXT("example.com", "v=spf1 ip4:8.8.8.8 -all")
	own := verifyTestDomain(t, a.db, other.tenant.ID, "other.org")
	got := decodeSPF(t, doJSON(t, other.h, "POST", "/v1/domains/"+own.ID+"/spf/verify", nil).Body)
	if got.Status != "not_configured" || got.Domain != "other.org" {
		t.Fatalf("state leaked across tenants: %+v", got)
	}
}

// SPF is deliverability guidance only: it must never grant From authorization,
// and DKIM state must be untouched by it (and vice versa).
func TestSPFDoesNotAuthorizeFromNorTouchDKIM(t *testing.T) {
	a := spfAPI(t, directSPF())
	victim, attacker := a.actor("victim"), a.actor("attacker")
	vdom := verifyTestDomain(t, a.db, victim.tenant.ID, "victim.com")
	verifyTestDomain(t, a.db, attacker.tenant.ID, "attacker.com")
	// Victim's SPF is perfect, and even attacker.com publishes an SPF authorizing MailX.
	a.dns.setTXT("victim.com", "v=spf1 ip4:8.8.8.8 -all")
	a.dns.setTXT("attacker.com", "v=spf1 ip4:8.8.8.8 -all")
	if got := decodeSPF(t, doJSON(t, victim.h, "POST", "/v1/domains/"+vdom.ID+"/spf/verify", nil).Body); got.Status != "verified" {
		t.Fatalf("setup: %+v", got)
	}
	rec := a.send(attacker, "ceo@victim.com", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "from_domain_not_authorized") {
		t.Fatalf("SPF must not authorize From: %d %s", rec.Code, rec.Body.String())
	}
	// Independence: DKIM state before/after SPF verification is identical, and a DKIM key does not make SPF verified.
	before := doJSON(t, victim.h, "GET", "/v1/domains/"+vdom.ID+"/dkim", nil).Body.String()
	a.dns.setTXT("victim.com", "v=spf1 -all")
	if got := decodeSPF(t, doJSON(t, victim.h, "POST", "/v1/domains/"+vdom.ID+"/spf/verify", nil).Body); got.Status != "mismatch" {
		t.Fatalf("%+v", got)
	}
	if after := doJSON(t, victim.h, "GET", "/v1/domains/"+vdom.ID+"/dkim", nil).Body.String(); after != before {
		t.Fatal("SPF verification changed DKIM state")
	}
	if rec := doJSON(t, victim.h, "POST", "/v1/domains/"+vdom.ID+"/dkim", nil); rec.Code != http.StatusCreated {
		t.Fatal(rec.Code)
	}
	if got := decodeSPF(t, doJSON(t, victim.h, "POST", "/v1/domains/"+vdom.ID+"/spf/verify", nil).Body); got.Status != "mismatch" {
		t.Fatalf("a DKIM key must not imply SPF readiness: %+v", got)
	}
	// Sending still works with a mismatching SPF record, and is signed by DKIM only once active.
	if rec := a.send(victim, "a@victim.com", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("sending must not depend on SPF: %d %s", rec.Code, rec.Body.String())
	}
}

// Local development: no declared sending IPs, no public SPF, every lookup would
// fail. Sending is unaffected and SPF never queries DNS on the send path.
func TestSendingNeverConsultsSPFAndLocalDevWorks(t *testing.T) {
	for name, cfg := range map[string]spf.Config{
		"direct undeclared": {Mode: spf.ModeDirect},
		"direct declared":   directSPF(),
		"relay undeclared":  {Mode: spf.ModeRelay},
		"relay declared":    {Mode: spf.ModeRelay, RelayInclude: "_spf.relay.example"},
	} {
		a := spfAPI(t, cfg)
		a.dns.fail = &net.DNSError{Err: "everything is down", IsTimeout: true}
		ac := a.actor("acme")
		dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
		if rec := a.send(ac, "alice@example.com", nil); rec.Code != http.StatusAccepted {
			t.Fatalf("%s: send = %d %s", name, rec.Code, rec.Body.String())
		}
		rec := doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/spf", nil)
		got := decodeSPF(t, rec.Body)
		wantUnknown := !cfg.Ready()
		if rec.Code != 200 || (got.Status == "sending_infrastructure_unknown") != wantUnknown {
			t.Fatalf("%s: %d %+v", name, rec.Code, got)
		}
		if wantUnknown && got.Expected.Value != "" {
			t.Fatalf("%s: a record was invented: %+v", name, got.Expected)
		}
	}
}

func TestSPFDirectAndRelayInstructionsDoNotMix(t *testing.T) {
	ad := spfAPI(t, directSPF())
	ac := ad.actor("d")
	dd := verifyTestDomain(t, ad.db, ac.tenant.ID, "example.com")
	got := decodeSPF(t, doJSON(t, ac.h, "GET", "/v1/domains/"+dd.ID+"/spf", nil).Body)
	if got.Mode != "direct" || strings.Contains(got.Expected.Value, "include:") || got.Sending.RelayInclude != "" {
		t.Fatalf("direct: %+v", got)
	}
	ar := spfAPI(t, spf.Config{Mode: spf.ModeRelay, RelayInclude: "_spf.relay.example"})
	rc := ar.actor("r")
	rd := verifyTestDomain(t, ar.db, rc.tenant.ID, "example.com")
	got = decodeSPF(t, doJSON(t, rc.h, "GET", "/v1/domains/"+rd.ID+"/spf", nil).Body)
	if got.Mode != "relay" || strings.Contains(got.Expected.Value, "ip4:") || got.Expected.Value != "v=spf1 include:_spf.relay.example ~all" || len(got.Sending.IPs) != 0 {
		t.Fatalf("relay: %+v", got)
	}
}

func TestSPFRoutesRequireDomainScopesAndUnavailableWithoutService(t *testing.T) {
	a := newDKIMAPI(t) // no SPF service
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	if rec := doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/spf", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service: %d", rec.Code)
	}
	if rec := doJSON(t, a.mux, "GET", "/v1/domains/"+dom.ID+"/spf", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}
}

func TestSPFConcurrentVerificationAcrossTenants(t *testing.T) {
	a := spfAPI(t, directSPF())
	type dom struct {
		ac   actor
		id   string
		want string
	}
	var doms []dom
	for i, tc := range []struct{ txt, want string }{
		{"v=spf1 ip4:8.8.8.8 -all", "verified"}, {"", "not_configured"}, {"v=spf1 ip4:zzz", "invalid"}, {"v=spf1 -all", "mismatch"},
	} {
		ac := a.actor("t" + string(rune('a'+i)))
		name := "d" + string(rune('a'+i)) + ".example.com"
		d := verifyTestDomain(t, a.db, ac.tenant.ID, name)
		if tc.txt != "" {
			a.dns.setTXT(name, tc.txt)
		}
		doms = append(doms, dom{ac, d.ID, tc.want})
	}
	var wg sync.WaitGroup
	for rep := 0; rep < 25; rep++ {
		for _, d := range doms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := doJSON(t, d.ac.h, "POST", "/v1/domains/"+d.id+"/spf/verify", nil)
				if got := decodeSPF(t, rec.Body); rec.Code != 200 || got.Status != d.want {
					t.Errorf("want %s: %d %s", d.want, rec.Code, rec.Body.String())
				}
			}()
		}
	}
	wg.Wait()
}
