package main

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/smtpidentity"
)

func setIdentity(t *testing.T, host, ips string) {
	t.Helper()
	t.Setenv("MAILX_SMTP_HOSTNAME", host)
	t.Setenv("MAILX_SENDING_IPS", ips)
}

func TestLocalDevelopmentIdentityNeedsNothing(t *testing.T) {
	setIdentity(t, "", "")
	id, err := loadSMTPIdentity()
	if err != nil || id.Public || id.Name() != smtpidentity.LocalIdentity || identityMode(id) != "local" {
		t.Fatalf("%+v %v", id, err)
	}
}

func TestPublicIdentityIsValidatedAndNormalized(t *testing.T) {
	setIdentity(t, "SMTP.Example.COM.", "8.8.8.8")
	id, err := loadSMTPIdentity()
	if err != nil || !id.Public || id.Name() != "smtp.example.com" || identityMode(id) != "public" {
		t.Fatalf("%+v %v", id, err)
	}
	// A relay may use a public hostname without sending IPs.
	setIdentity(t, "smtp.example.com", "")
	if id, err = loadSMTPIdentity(); err != nil || id.Name() != "smtp.example.com" {
		t.Fatalf("%+v %v", id, err)
	}
}

func TestPublicDirectDeliveryRequiresAHostname(t *testing.T) {
	setIdentity(t, "", "8.8.8.8")
	if _, err := loadSMTPIdentity(); err == nil || !strings.Contains(err.Error(), "MAILX_SMTP_HOSTNAME") {
		t.Fatalf("declaring public sending IPs without a hostname must fail startup: %v", err)
	}
}

func TestInvalidHostnamesFailStartupNamingTheVariableNotTheValue(t *testing.T) {
	for _, bad := range []string{"mailx.local", "localhost", "smtp", "*.example.com", "https://smtp.example.com", "smtp.example.com:25", "user@example.com",
		"203.0.113.10", " smtp.example.com", "co.uk", "smtp.example.com\r\nX: y"} {
		setIdentity(t, bad, "")
		_, err := loadSMTPIdentity()
		if err == nil || !strings.Contains(err.Error(), "MAILX_SMTP_HOSTNAME") {
			t.Errorf("%q: %v", bad, err)
			continue
		}
		if strings.Contains(err.Error(), strings.TrimSpace(bad)) {
			t.Errorf("the value leaked into the error: %v", err)
		}
	}
}

// Startup never consults DNS: a resolver outage cannot prevent MailX from starting.
func TestStartupValidationIsStaticOnly(t *testing.T) {
	setIdentity(t, "smtp.example.com", "8.8.8.8")
	if _, err := loadSMTPIdentity(); err != nil { // no resolver is even reachable from this function
		t.Fatal(err)
	}
}

type cmdDNS struct {
	ptr map[string][]string
	fwd map[string][]netip.Addr
}

func (d cmdDNS) LookupAddr(_ context.Context, a string) ([]string, error) {
	if n, ok := d.ptr[a]; ok {
		return n, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}
func (d cmdDNS) LookupNetIP(_ context.Context, _, h string) ([]netip.Addr, error) {
	if a, ok := d.fwd[h]; ok {
		return a, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}
func (d cmdDNS) LookupTXT(context.Context, string) ([]string, error) {
	return nil, &net.DNSError{IsNotFound: true}
}

func TestDiagnosticReportsAndExitsNonZeroUnlessReady(t *testing.T) {
	ip := netip.MustParseAddr("8.8.8.8")
	var out bytes.Buffer
	ready := cmdDNS{ptr: map[string][]string{"8.8.8.8": {"smtp.example.com."}}, fwd: map[string][]netip.Addr{"smtp.example.com": {ip}}}
	if err := runSMTPIdentityCheck(context.Background(), &out, &smtpidentity.Checker{Resolver: ready}, "smtp.example.com", []netip.Addr{ip}); err != nil {
		t.Fatalf("ready must exit zero: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Readiness: ready", "8.8.8.8 (ipv4): ready", "HELO SPF for the hostname: not_configured", "v=spf1 ip4:8.8.8.8 -all", "does not guarantee inbox placement"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	missing := cmdDNS{fwd: map[string][]netip.Addr{"smtp.example.com": {ip}}}
	err := runSMTPIdentityCheck(context.Background(), &out, &smtpidentity.Checker{Resolver: missing}, "smtp.example.com", []netip.Addr{ip})
	if err == nil || !strings.Contains(out.String(), "missing_ptr") || !strings.Contains(err.Error(), "not_ready") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestDiagnosticRefusesArguments(t *testing.T) {
	if err := cmdCheckSMTPIdentity([]string{"8.8.8.8"}, &bytes.Buffer{}); err == nil {
		t.Fatal("the diagnostic must not accept caller-supplied hosts or addresses")
	}
	setIdentity(t, "", "")
	if err := cmdCheckSMTPIdentity(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("%v", err)
	}
}
