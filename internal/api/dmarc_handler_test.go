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
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
	"github.com/Ferousco-dev/mailx/internal/spf"
)

// Test copies of the production adapters in cmd/mailx/dmarcconfig.go.
type testDKIMState struct{ svc *dkim.Service }

func (a testDKIMState) ActiveSigningDomain(ctx context.Context, tenantID, domainID string) (string, bool, error) {
	dom, keys, err := a.svc.Keys(ctx, tenantID, domainID)
	if err != nil {
		return "", false, err
	}
	for _, k := range keys {
		if k.Status == database.DKIMActive {
			return dom.Name, true, nil
		}
	}
	return dom.Name, false, nil
}

type testSPFState struct{ svc *spf.Service }

func (a testSPFState) SPFView(ctx context.Context, tenantID, domainID string, check bool) (dmarc.SPFView, error) {
	v := dmarc.SPFView{Mode: string(a.svc.Config().Mode)}
	if !check {
		return v, nil
	}
	res, err := a.svc.Verify(ctx, tenantID, domainID)
	if err != nil {
		return v, err
	}
	v.Status = string(res.Status)
	return v, nil
}

func dmarcAPI(t *testing.T, spfCfg spf.Config) *dkimAPI {
	t.Helper()
	return newAPIFull(t,
		func(db *database.DB, dns *publishedDNS) *spf.Service {
			s, err := spf.NewService(db, dns, spfCfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		func(db *database.DB, dns *publishedDNS, d *dkim.Service, s *spf.Service) *dmarc.Service {
			svc, err := dmarc.NewService(db, dns, testDKIMState{d}, testSPFState{s}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return svc
		})
}

func decodeDMARC(t *testing.T, body interface{ Bytes() []byte }) dmarcResource {
	t.Helper()
	var out dmarcResource
	if err := json.Unmarshal(body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDMARCLifecycleThroughAPI(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect, IPs: []netip.Addr{testSendingIP}})
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	base := "/v1/domains/" + dom.ID + "/dmarc"

	// Guidance: no DNS, honest about what is not known.
	rec := doJSON(t, ac.h, "GET", base, nil)
	g := decodeDMARC(t, rec.Body)
	if rec.Code != 200 || g.Checked || g.Readiness != "unchecked" || g.ReceiverResult != "not_observed" || g.Expected.Action != "create" ||
		g.Expected.Name != "_dmarc.example.com" || g.Expected.Value != "v=DMARC1; p=none" || g.DKIM.Status != "not_configured" || g.SPF.Status != "unknown" ||
		g.OrgDomain != "" || strings.Contains(g.Expected.Value, "rua") { // the Organizational Domain needs the DNS Tree Walk, so GET does not claim one
		t.Fatalf("GET: %d %+v", rec.Code, g)
	}

	// No record yet.
	v := decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body)
	if v.DNS.Status != "not_configured" || v.Readiness != "dns_action_required" || !v.Checked {
		t.Fatalf("%+v", v)
	}

	// Publish monitoring policy: DNS ok but no aligned authentication configured yet.
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=none;")
	v = decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body)
	if v.DNS.Status != "monitoring" || v.Readiness != "authentication_incomplete" || v.DKIM.Status != "not_configured" || v.SPF.Status != "not_configured" ||
		v.OrgDomain != "example.com" {
		t.Fatalf("%+v", v)
	}

	// Activate DKIM through the real lifecycle: aligned DKIM path becomes ready.
	rec = doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dkim", nil)
	st := decodeStatus(t, rec)
	a.dns.setTXT(st.Pending.DNS.Name, st.Pending.DNS.Value)
	if rec = doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dkim/verify", nil); rec.Code != 200 {
		t.Fatalf("dkim verify: %d %s", rec.Code, rec.Body.String())
	}
	v = decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body)
	if v.Readiness != "ready" || v.DKIM.Status != "ready" || !v.DKIM.Aligned || v.DKIM.Identity != "example.com" || v.ReceiverResult != "not_observed" {
		t.Fatalf("%+v", v)
	}
	if !contains(v.Warnings, "policy_not_enforcing") {
		t.Fatalf("monitoring policy must warn it is not enforcing: %v", v.Warnings)
	}

	// SPF verified too: both paths ready.
	a.dns.setTXT("example.com", "v=spf1 ip4:8.8.8.8 ~all")
	v = decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body)
	if v.SPF.Status != "ready" || v.SPF.Identity != "example.com" || v.DKIM.Status != "ready" {
		t.Fatalf("%+v", v)
	}

	// An existing stricter policy is reported and never replaced by MailX's default.
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=reject; adkim=s; aspf=s; pct=100")
	v = decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body)
	if v.DNS.Status != "enforcing" || v.DNS.EffectivePolicy != "reject" || v.Expected.Action != "none" || v.Expected.Value != "" ||
		v.DKIM.Mode != "strict" || v.DKIM.Status != "ready" || !contains(v.Warnings, "deprecated_tag") {
		t.Fatalf("%+v", v)
	}
	// Conflict / invalid are surfaced, never resolved arbitrarily.
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=none", "v=DMARC1; p=reject")
	if v = decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body); v.DNS.Status != "conflict" || v.Expected.Action != "merge_records" || v.Expected.Value != "" {
		t.Fatalf("%+v", v)
	}
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=bogus")
	if v = decodeDMARC(t, doJSON(t, ac.h, "POST", base+"/verify", nil).Body); v.DNS.Status != "invalid" || v.DNS.Reason != "bad_policy" || v.Expected.Action != "fix_record" {
		t.Fatalf("%+v", v)
	}
}

func TestDMARCSubdomainInheritsOrganizationalPolicy(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect, IPs: []netip.Addr{testSendingIP}})
	ac := a.actor("acme")
	sub := verifyTestDomain(t, a.db, ac.tenant.ID, "mail.example.com")
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=reject; sp=quarantine")
	v := decodeDMARC(t, doJSON(t, ac.h, "POST", "/v1/domains/"+sub.ID+"/dmarc/verify", nil).Body)
	if v.DNS.Source != "organizational_domain" || v.DNS.RecordName != "_dmarc.example.com" || v.DNS.EffectivePolicy != "quarantine" ||
		v.DNS.Status != "enforcing" || v.OrgDomain != "example.com" {
		t.Fatalf("%+v", v)
	}
}

func TestDMARCTenantIsolationAndOwnership(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect})
	owner, other := a.actor("owner"), a.actor("other")
	dom := verifyTestDomain(t, a.db, owner.tenant.ID, "example.com")
	pending, _ := a.db.CreateDomain(context.Background(), owner.tenant.ID, "pending-domain.com", "tok")
	for _, c := range []struct{ method, p string }{{"GET", "/dmarc"}, {"POST", "/dmarc/verify"}} {
		if rec := doJSON(t, other.h, c.method, "/v1/domains/"+dom.ID+c.p, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant %s %s = %d", c.method, c.p, rec.Code)
		}
	}
	if rec := doJSON(t, owner.h, "POST", "/v1/domains/nope/dmarc/verify", nil); rec.Code != http.StatusNotFound {
		t.Fatal(rec.Code)
	}
	if rec := doJSON(t, owner.h, "POST", "/v1/domains/"+pending.ID+"/dmarc/verify", nil); rec.Code != http.StatusConflict {
		t.Fatalf("unverified: %d", rec.Code)
	}
	// Tenant B's own domain never sees tenant A's published DMARC.
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=reject")
	own := verifyTestDomain(t, a.db, other.tenant.ID, "other.org")
	if v := decodeDMARC(t, doJSON(t, other.h, "POST", "/v1/domains/"+own.ID+"/dmarc/verify", nil).Body); v.DNS.Status != "not_configured" {
		t.Fatalf("state leaked across tenants: %+v", v)
	}
}

// DMARC (like SPF) must never grant From authorization, even if a victim's
// _dmarc record is public or the attacker publishes records of their own.
func TestDMARCCannotBypassVerifiedFrom(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect, IPs: []netip.Addr{testSendingIP}})
	victim, attacker := a.actor("victim"), a.actor("attacker")
	verifyTestDomain(t, a.db, victim.tenant.ID, "victim.com")
	verifyTestDomain(t, a.db, attacker.tenant.ID, "attacker.com")
	a.dns.setTXT("_dmarc.victim.com", "v=DMARC1; p=none")
	a.dns.setTXT("_dmarc.attacker.com", "v=DMARC1; p=none; rua=mailto:x@victim.com")
	rec := a.send(attacker, "ceo@victim.com", nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "from_domain_not_authorized") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := emailCount(t, attacker); n != 0 {
		t.Fatalf("rejected send left state: %d", n)
	}
}

// DMARC consumes DKIM/SPF facts; it must not change them, and neither implies the other.
func TestDMARCDoesNotTouchDKIMOrSPFAndSendingIgnoresIt(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect, IPs: []netip.Addr{testSendingIP}})
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=reject")
	dkimBefore := doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/dkim", nil).Body.String()
	spfBefore := doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/spf", nil).Body.String()
	for i := 0; i < 3; i++ {
		if rec := doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dmarc/verify", nil); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
	}
	if doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/dkim", nil).Body.String() != dkimBefore {
		t.Fatal("DMARC verification changed DKIM state")
	}
	if doJSON(t, ac.h, "GET", "/v1/domains/"+dom.ID+"/spf", nil).Body.String() != spfBefore {
		t.Fatal("DMARC verification changed SPF state")
	}
	// p=reject published and no DKIM/SPF set up: sending is still accepted (DMARC never blocks), and unsigned as before.
	if rec := a.send(ac, "alice@example.com", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
}

// Local development: every DNS lookup fails; sending works and DMARC reports "unknown",
// with no raw resolver text exposed.
func TestDMARCLocalDevAndTemporaryFailures(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect})
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	a.dns.mu.Lock()
	a.dns.fail = &net.DNSError{Err: "resolver-internal-secret-text", IsTimeout: true, IsTemporary: true}
	a.dns.mu.Unlock()
	if rec := a.send(ac, "alice@example.com", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("send: %d", rec.Code)
	}
	rec := doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dmarc/verify", nil)
	v := decodeDMARC(t, rec.Body)
	if rec.Code != 200 || v.DNS.Status != "temporary_error" || v.Readiness != "unknown" || strings.Contains(rec.Body.String(), "resolver-internal-secret-text") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// Relay mode: MailX cannot know the relay's return-path, so SPF alignment is "unknown", never faked.
func TestDMARCRelayModeIsTruthfulAboutSPF(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeRelay, RelayInclude: "_spf.relay.example"})
	ac := a.actor("acme")
	dom := verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	a.dns.setTXT("_dmarc.example.com", "v=DMARC1; p=none")
	a.dns.setTXT("example.com", "v=spf1 include:_spf.relay.example ~all") // SPF "verified" must not be treated as DMARC-aligned
	v := decodeDMARC(t, doJSON(t, ac.h, "POST", "/v1/domains/"+dom.ID+"/dmarc/verify", nil).Body)
	if v.Mode != "relay" || v.SPF.Status != "unknown" || v.SPF.Reason != "relay_return_path_unknown" || v.SPF.Identity != "" || v.SPF.Aligned {
		t.Fatalf("%+v", v)
	}
	if v.Readiness == "ready" {
		t.Fatalf("relay without DKIM must not be reported ready: %+v", v)
	}
}

// Source truth for the identity model: MAIL FROM (and so the SPF identity) is the API From address.
func TestMailFromEqualsFromDomain(t *testing.T) {
	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect})
	ac := a.actor("acme")
	verifyTestDomain(t, a.db, ac.tenant.ID, "example.com")
	rec := a.send(ac, "Alice <alice@Example.com>", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var out struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	msg, err := a.db.GetMessage(context.Background(), ac.tenant.ID, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The envelope path keeps SMTP angle brackets and the sender's letter case;
	// domains compare case-insensitively (RFC 5321 2.4, RFC 7208 4.3).
	path := strings.Trim(msg.MailFrom, "<>")
	at := strings.LastIndexByte(path, '@')
	if ok, _ := dmarc.Aligned("example.com", path[at+1:], dmarc.Strict, nil); !ok {
		t.Fatalf("MAIL FROM %q is not the From domain", msg.MailFrom)
	}
}

func TestDMARCRoutesUnavailableWithoutServiceAndConcurrent(t *testing.T) {
	plain := newDKIMAPI(t)
	pc := plain.actor("acme")
	pd := verifyTestDomain(t, plain.db, pc.tenant.ID, "example.com")
	if rec := doJSON(t, pc.h, "GET", "/v1/domains/"+pd.ID+"/dmarc", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service: %d", rec.Code)
	}
	if rec := doJSON(t, plain.mux, "GET", "/v1/domains/"+pd.ID+"/dmarc", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", rec.Code)
	}

	a := dmarcAPI(t, spf.Config{Mode: spf.ModeDirect, IPs: []netip.Addr{testSendingIP}})
	type dom struct {
		ac         actor
		id, status string
	}
	var doms []dom
	for i, tc := range []struct{ txt, want string }{
		{"v=DMARC1; p=none", "monitoring"}, {"", "not_configured"}, {"v=DMARC1; p=x", "invalid"}, {"v=DMARC1; p=reject", "enforcing"},
	} {
		ac := a.actor("t" + string(rune('a'+i)))
		name := "d" + string(rune('a'+i)) + ".example.com"
		d := verifyTestDomain(t, a.db, ac.tenant.ID, name)
		if tc.txt != "" {
			a.dns.setTXT("_dmarc."+name, tc.txt)
		}
		doms = append(doms, dom{ac, d.ID, tc.want})
	}
	var wg sync.WaitGroup
	for rep := 0; rep < 20; rep++ {
		for _, d := range doms {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := doJSON(t, d.ac.h, "POST", "/v1/domains/"+d.id+"/dmarc/verify", nil)
				if v := decodeDMARC(t, rec.Body); rec.Code != 200 || v.DNS.Status != d.status {
					t.Errorf("want %s: %d %s", d.status, rec.Code, rec.Body.String())
				}
			}()
		}
	}
	wg.Wait()
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
