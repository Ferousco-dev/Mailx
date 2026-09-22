package smtp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
)

const publicIdentity = "smtp.infra.example.com"

func identityClient(t *testing.T, pki *smtptest.PKI, policy TLSPolicy) *Client {
	t.Helper()
	cfg := testClientConfig()
	cfg.Identity = publicIdentity
	cfg.TLS = TLSConfig{Policy: policy, RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// helloCommands returns the EHLO/HELO commands exactly as the peer received them.
func helloCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if i := strings.Index(c, ":"); i >= 0 {
			c = c[i+1:]
		}
		if strings.HasPrefix(c, "EHLO") || strings.HasPrefix(c, "HELO") {
			out = append(out, c)
		}
	}
	return out
}

// Direct delivery over STARTTLS: the SAME configured identity before and after the
// upgrade, and never the local development identity.
func TestEHLOIdentityIsIdenticalBeforeAndAfterSTARTTLS(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert})
	res, err := identityClient(t, pki, TLSOpportunistic).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted || !res.TLS.Established {
		t.Fatalf("%+v %v", res, err)
	}
	got := helloCommands(mx.Commands())
	if len(got) != 2 || got[0] != "EHLO "+publicIdentity || got[1] != got[0] {
		t.Fatalf("hello commands = %q", got)
	}
	for _, c := range mx.Commands() {
		if strings.Contains(c, "mailx.local") {
			t.Fatalf("public mode sent the local identity: %q", c)
		}
	}
	// The pre-STARTTLS EHLO, STARTTLS, post-TLS EHLO order is preserved.
	if cmds := mx.Commands(); !strings.HasPrefix(cmds[0], "plain:EHLO") || !strings.HasPrefix(cmds[1], "plain:STARTTLS") || !strings.HasPrefix(cmds[2], "tls:EHLO") {
		t.Fatalf("order: %v", cmds)
	}
}

// A peer that rejects EHLO gets HELO with the same identity.
func TestHELOFallbackUsesTheSameConfiguredIdentity(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := smtptest.Start(t, smtptest.Options{HeloOnly: true})
	res, err := identityClient(t, pki, TLSOpportunistic).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted {
		t.Fatalf("%+v %v", res, err)
	}
	got := helloCommands(mx.Commands())
	if len(got) != 2 || got[0] != "EHLO "+publicIdentity || got[1] != "HELO "+publicIdentity {
		t.Fatalf("hello commands = %q", got)
	}
}

// The identity can never carry protocol-breaking bytes into the EHLO line.
func TestIdentityWithControlCharactersIsRejectedByTheClient(t *testing.T) {
	for _, bad := range []string{"smtp.example.com\r\nMAIL FROM:<x@y.z>", "smtp example.com", "smtp\x00.example.com", "smtp.example.com\t"} {
		cfg := testClientConfig()
		cfg.Identity = bad
		if _, err := NewClient(cfg); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Relay submission: the configured identity is used for the relay session too,
// identical before and after STARTTLS, with AUTH strictly after TLS and the
// credentials only ever seen post-TLS by that relay.
func TestRelaySessionKeepsIdentityAndTLSBeforeAUTH(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{RequireAuth: true})
	cfg := testClientConfig()
	cfg.Identity = publicIdentity
	cfg.TLS = TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	if err != nil || !res.Accepted || res.Auth.Outcome != AuthSuccess {
		t.Fatalf("%+v %v", res, err)
	}
	got := helloCommands(mx.Commands())
	if len(got) != 2 || got[0] != "EHLO "+publicIdentity || got[1] != got[0] {
		t.Fatalf("hello commands = %q", got)
	}
	if at := mx.AuthAttempts(); len(at) != 1 || at[0].Phase != "tls" {
		t.Fatalf("credentials must only be presented after TLS: %+v", at)
	}
	cmds := mx.Commands()
	auth, mail := -1, -1
	for i, c := range cmds {
		switch {
		case strings.Contains(c, "AUTH"):
			auth = i
		case strings.Contains(c, "MAIL"):
			mail = i
		}
	}
	if auth < 0 || mail < auth || !strings.HasPrefix(cmds[auth], "tls:") {
		t.Fatalf("AUTH must be after TLS and before MAIL: %v", cmds)
	}
}
