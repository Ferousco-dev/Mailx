package smtp

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/smtp/smtptest"
)

const (
	markerUser = "MAILX_PRIVATE_USER_MARKER_7c1d"
	markerPass = "MAILX_PRIVATE_PASSWORD_MARKER_9f2e"
)

var markerCreds = &Credentials{Username: markerUser, Password: markerPass}

func b64s(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// secretSurface concatenates everything a caller could log or return, and
// reports which secret forms (raw and encoded) it contains.
func leaks(surface, user, pass string) []string {
	forms := map[string]string{
		"username": user, "password": pass,
		"b64(username)": b64s(user), "b64(password)": b64s(pass),
		"b64(PLAIN payload)": b64s("\x00" + user + "\x00" + pass),
	}
	var found []string
	for name, form := range forms {
		if strings.Contains(surface, form) {
			found = append(found, name)
		}
	}
	return found
}

type authRec struct {
	mu     sync.Mutex
	events []string
}

func (o *authRec) AuthResult(mech, outcome string) {
	o.mu.Lock()
	o.events = append(o.events, mech+"/"+outcome)
	o.mu.Unlock()
}

func (o *authRec) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.events) == 0 {
		return ""
	}
	return o.events[len(o.events)-1]
}

type panicAuthObserver struct{}

func (panicAuthObserver) AuthResult(string, string) { panic("observer bug") }

func authClient(t testing.TB, pki *smtptest.PKI, obs AuthObserver) *Client {
	t.Helper()
	cfg := testClientConfig()
	cfg.ReadTimeout = 400 * time.Millisecond
	cfg.TLS = TLSConfig{RootCAs: pki.Pool, HandshakeTimeout: 2 * time.Second}
	cfg.AuthObserver = obs
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func authRequest(addr string, creds *Credentials) DeliveryRequest {
	r := request(addr)
	r.Auth = creds
	return r
}

// relayServer is a fake submission server that accepts markerCreds after TLS.
func relayServer(t testing.TB, pki *smtptest.PKI, o smtptest.Options) *smtptest.Server {
	t.Helper()
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	o.Advertise, o.Cert = true, &cert
	if o.AuthPost == "" && o.AuthReply == "" && !o.AuthDrop && !o.AuthStall {
		o.AuthPost = "PLAIN LOGIN"
	}
	if o.AuthUser == "" {
		o.AuthUser, o.AuthPass = markerUser, markerPass
	}
	return smtptest.Start(t, o)
}

// A/F. Happy path with the exact command order; MAIL only after AUTH.
func TestAuthHappyPathCommandOrderPlain(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{RequireAuth: true})
	obs := &authRec{}
	res, err := authClient(t, pki, obs).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	if err != nil || !res.Accepted {
		t.Fatalf("%+v %v", res, err)
	}
	want := []string{"plain:EHLO", "plain:STARTTLS", "tls:EHLO", "tls:AUTH PLAIN", "tls:MAIL", "tls:RCPT", "tls:DATA", "tls:QUIT"}
	got := mx.Commands()
	if len(got) != len(want) {
		t.Fatalf("commands = %v", got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("command %d = %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
	at := mx.AuthAttempts()
	if len(at) != 1 || at[0].Phase != "tls" || at[0].User != markerUser || at[0].Pass != markerPass {
		t.Fatalf("server saw %+v", at)
	}
	if !res.Auth.Attempted || res.Auth.Mechanism != "plain" || res.Auth.Outcome != AuthSuccess || obs.last() != "plain/success" {
		t.Fatalf("auth info %+v observer %q", res.Auth, obs.last())
	}
}

func TestAuthLoginWhenPlainNotOffered(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{AuthPost: "LOGIN", RequireAuth: true})
	res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	if err != nil || !res.Accepted || res.Auth.Mechanism != "login" {
		t.Fatalf("%+v %v", res, err)
	}
	if at := mx.AuthAttempts(); len(at) != 1 || at[0].Mechanism != "LOGIN" || at[0].User != markerUser || at[0].Pass != markerPass {
		t.Fatalf("server saw %+v", at)
	}
}

// Long credentials exceed the command line limit and use the challenge form.
func TestAuthPlainLongCredentialsUseChallengeForm(t *testing.T) {
	pki := smtptest.NewPKI(t)
	long := &Credentials{Username: strings.Repeat("u", 255), Password: strings.Repeat("p", 255)}
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, AuthPost: "PLAIN", AuthUser: long.Username, AuthPass: long.Password})
	res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), long))
	if err != nil || !res.Accepted {
		t.Fatalf("%+v %v", res, err)
	}
}

// B. No STARTTLS at all: credentials are never sent, whatever the policy.
func TestAuthNeverSentWithoutSTARTTLS(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := smtptest.Start(t, smtptest.Options{AuthPre: "PLAIN LOGIN", AuthUser: markerUser, AuthPass: markerPass})
	res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds)) // opportunistic policy
	de := stageOf(t, err)
	if de.Stage != StageStartTLS || res.TLS.Outcome != OutcomeRequiredNoTLS || res.Accepted {
		t.Fatalf("stage=%s outcome=%s", de.Stage, res.TLS.Outcome)
	}
	if mx.SawAny("AUTH") || len(mx.AuthAttempts()) != 0 || mx.SawAny("MAIL") {
		t.Fatalf("credentials or mail reached a plaintext-only relay: %v", mx.Commands())
	}
}

// C. Any TLS failure means AUTH is never sent.
func TestAuthNeverSentAfterTLSFailure(t *testing.T) {
	pki := smtptest.NewPKI(t)
	other := smtptest.NewPKI(t)
	bad := other.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	good := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	cases := map[string]smtptest.Options{
		"untrusted cert":   {Advertise: true, Cert: &bad, AuthPost: "PLAIN", AuthUser: markerUser, AuthPass: markerPass},
		"handshake closed": {Advertise: true, Cert: &good, Handshake: smtptest.HSClose, AuthPost: "PLAIN"},
		"starttls refused": {Advertise: true, Cert: &good, StartTLSReply: "454 4.7.0 no", AuthPost: "PLAIN"},
	}
	for name, o := range cases {
		mx := smtptest.Start(t, o)
		_, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
		if err == nil || mx.SawAny("AUTH") || mx.SawAny("MAIL") {
			t.Fatalf("%s: err=%v cmds=%v", name, err, mx.Commands())
		}
		if l := leaks(err.Error(), markerUser, markerPass); len(l) > 0 {
			t.Fatalf("%s: error leaks %v", name, l)
		}
	}
}

// D/P. AUTH is judged from the post-TLS EHLO only.
func TestAuthUsesPostTLSCapabilitiesOnly(t *testing.T) {
	pki := smtptest.NewPKI(t)
	// Pre-TLS advertises AUTH, post-TLS does not: must not authenticate.
	pre := relayServer(t, pki, smtptest.Options{AuthPre: "PLAIN", AuthPost: "CRAM-MD5"})
	res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(pre.Addr("localhost"), markerCreds))
	de := stageOf(t, err)
	if de.Stage != StageAuth || res.Auth.Outcome != AuthNoMechanism || pre.SawAny("AUTH") || pre.SawAny("MAIL") {
		t.Fatalf("stale pre-TLS AUTH capability was used: stage=%s outcome=%s cmds=%v", de.Stage, res.Auth.Outcome, pre.Commands())
	}
	// Pre-TLS has no AUTH, post-TLS does: authenticates.
	post := relayServer(t, pki, smtptest.Options{AuthPost: "PLAIN"})
	if res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(post.Addr("localhost"), markerCreds)); err != nil || !res.Accepted {
		t.Fatalf("post-TLS AUTH not used: %+v %v", res, err)
	}
}

// D. Post-TLS EHLO without any AUTH line.
func TestAuthNotAdvertisedAfterTLS(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mx := smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, AuthPre: "PLAIN"}) // AuthPost empty
	obs := &authRec{}
	res, err := authClient(t, pki, obs).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	de := stageOf(t, err)
	if de.Stage != StageAuth || res.Auth.Outcome != AuthNotAdvertised || mx.SawAny("AUTH") || mx.SawAny("MAIL") {
		t.Fatalf("stage=%s outcome=%s cmds=%v", de.Stage, res.Auth.Outcome, mx.Commands())
	}
	if obs.last() != "none/not_advertised" {
		t.Fatalf("observer %q", obs.last())
	}
}

// E. Only mechanisms MailX deliberately does not implement.
func TestAuthNoSupportedMechanism(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{AuthPost: "CRAM-MD5 DIGEST-MD5 XOAUTH2 SCRAM-SHA-256"})
	res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	if de := stageOf(t, err); de.Stage != StageAuth || res.Auth.Outcome != AuthNoMechanism || mx.SawAny("AUTH") {
		t.Fatalf("stage=%s outcome=%s", de.Stage, res.Auth.Outcome)
	}
}

// G/H/I. Reply classification, no MAIL FROM, no secret in any surface, reply text dropped.
func TestAuthFailureClassificationAndSecrecy(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cases := []struct {
		name    string
		opts    smtptest.Options
		outcome AuthOutcome
		code    int
		temp    bool
	}{
		{"wrong credentials", smtptest.Options{AuthUser: "someone", AuthPass: "else", AuthEcho: true}, AuthRejected, 535, false},
		{"temporary 454", smtptest.Options{AuthReply: "454 4.7.0 try later " + markerPass}, AuthTemporary, 454, true},
		{"policy 534", smtptest.Options{AuthReply: "534 5.7.9 too weak"}, AuthRejected, 534, false},
		{"malformed reply", smtptest.Options{AuthReply: "this is not smtp"}, AuthProtocolError, 0, false},
		{"unexpected 250", smtptest.Options{AuthReply: "250 ok"}, AuthProtocolError, 250, false},
		{"challenge after final", smtptest.Options{AuthReply: "334 more?"}, AuthProtocolError, 334, false},
	}
	for _, tc := range cases {
		tc.opts.AuthPost = "PLAIN"
		mx := relayServer(t, pki, tc.opts)
		obs := &authRec{}
		res, err := authClient(t, pki, obs).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
		de := stageOf(t, err)
		if de.Stage != StageAuth || res.Auth.Outcome != tc.outcome || de.Code != tc.code || de.Temporary != tc.temp || res.Accepted {
			t.Fatalf("%s: stage=%s outcome=%s code=%d temp=%v", tc.name, de.Stage, res.Auth.Outcome, de.Code, de.Temporary)
		}
		if mx.SawAny("MAIL") || de.Remote != "" {
			t.Fatalf("%s: MAIL sent after failed AUTH or remote text kept (%q)", tc.name, de.Remote)
		}
		surface := fmt.Sprintf("%v|%+v|%#v|%s|%s", err, res, markerCreds, res.FinalMessage, obs.last())
		if l := leaks(surface, markerUser, markerPass); len(l) > 0 {
			t.Fatalf("%s: secrets in observable output: %v\n%s", tc.name, l, surface)
		}
	}
}

// J/K/L. Disconnect, stalls and cancellation are bounded and leak nothing.
func TestAuthDisconnectStallAndCancellation(t *testing.T) {
	pki := smtptest.NewPKI(t)
	for name, tc := range map[string]struct {
		opts    smtptest.Options
		outcome AuthOutcome
	}{
		"disconnect":      {smtptest.Options{AuthDrop: true, AuthPost: "PLAIN"}, AuthConnLost},
		"stall":           {smtptest.Options{AuthStall: true, AuthPost: "PLAIN"}, AuthTimeout},
		"stall mid-login": {smtptest.Options{AuthStallMid: true, AuthPost: "LOGIN"}, AuthTimeout},
	} {
		mx := relayServer(t, pki, tc.opts)
		before := runtime.NumGoroutine()
		start := time.Now()
		res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
		if time.Since(start) > 3*time.Second {
			t.Fatalf("%s took %v", name, time.Since(start))
		}
		de := stageOf(t, err)
		if de.Stage != StageAuth || res.Auth.Outcome != tc.outcome || !de.Temporary || mx.SawAny("MAIL") {
			t.Fatalf("%s: stage=%s outcome=%s temp=%v", name, de.Stage, res.Auth.Outcome, de.Temporary)
		}
		waitGoroutines(t, before)
	}
	// Cancellation while the server stalls the AUTH reply.
	mx := relayServer(t, pki, smtptest.Options{AuthStall: true, AuthPost: "PLAIN"})
	cfg := testClientConfig()
	cfg.ReadTimeout = time.Minute
	cfg.TLS = TLSConfig{RootCAs: pki.Pool}
	c, _ := NewClient(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	before := runtime.NumGoroutine()
	start := time.Now()
	res, err := c.Send(ctx, authRequest(mx.Addr("localhost"), markerCreds))
	if time.Since(start) > 3*time.Second || err == nil || res.Auth.Outcome != AuthCanceled {
		t.Fatalf("cancel: took %v outcome=%s err=%v", time.Since(start), res.Auth.Outcome, err)
	}
	waitGoroutines(t, before)
}

// M. AUTH + DATA accepted + QUIT failure: still delivered, exactly once.
func TestAcceptedThroughAuthenticatedRelaySurvivesQuitFailure(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{QuitCloses: true, RequireAuth: true})
	res, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	if err != nil || !res.Accepted || res.QuitError == nil || mx.Conns.Load() != 1 {
		t.Fatalf("%+v %v conns=%d", res, err, mx.Conns.Load())
	}
}

// Direct delivery (no credentials) never authenticates, even if AUTH is offered.
func TestNoCredentialsMeansNoAuthCommand(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{})
	res, err := authClient(t, pki, nil).Send(context.Background(), request(mx.Addr("localhost")))
	if err != nil || !res.Accepted || mx.SawAny("AUTH") || res.Auth.Attempted {
		t.Fatalf("%+v %v cmds=%v", res, err, mx.Commands())
	}
}

func TestInvalidCredentialsRejectedBeforeAnyConnection(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{})
	for _, bad := range []*Credentials{{"", "x"}, {"x", ""}, {"a\x00b", "x"}, {"x", "a\r\nb"}, {strings.Repeat("u", 256), "x"}} {
		_, err := authClient(t, pki, nil).Send(context.Background(), authRequest(mx.Addr("localhost"), bad))
		if de := stageOf(t, err); de.Stage != StageInvalidInput || strings.Contains(err.Error(), "\x00") {
			t.Fatalf("stage=%s err=%v", de.Stage, err)
		}
	}
	if mx.Conns.Load() != 0 {
		t.Fatal("invalid credentials must be rejected before dialing")
	}
}

func TestCredentialsNeverPrintTheirValues(t *testing.T) {
	c := Credentials{Username: markerUser, Password: markerPass}
	if s := fmt.Sprintf("%v %+v %#v %s", c, c, c, &c); strings.Contains(s, markerUser) || strings.Contains(s, markerPass) {
		t.Fatalf("credentials printed: %s", s)
	}
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "creds", c)
	if strings.Contains(sb.String(), markerUser) || strings.Contains(sb.String(), markerPass) {
		t.Fatalf("slog printed credentials: %s", sb.String())
	}
}

func TestAuthObserverPanicDoesNotAffectDelivery(t *testing.T) {
	pki := smtptest.NewPKI(t)
	mx := relayServer(t, pki, smtptest.Options{})
	res, err := authClient(t, pki, panicAuthObserver{}).Send(context.Background(), authRequest(mx.Addr("localhost"), markerCreds))
	if err != nil || !res.Accepted {
		t.Fatalf("%+v %v", res, err)
	}
}

// Q + cross-talk. One shared Client, many concurrent sends with DIFFERENT
// credentials to different relays: each relay must only ever see its own.
func TestConcurrentAuthenticatedSendsNeverMixCredentials(t *testing.T) {
	pki := smtptest.NewPKI(t)
	cert := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	const relays = 6
	type pair struct {
		mx    *smtptest.Server
		creds *Credentials
	}
	pairs := make([]pair, relays)
	for i := range pairs {
		c := &Credentials{Username: fmt.Sprintf("user-%d-%s", i, markerUser), Password: fmt.Sprintf("pass-%d-%s", i, markerPass)}
		pairs[i] = pair{smtptest.Start(t, smtptest.Options{Advertise: true, Cert: &cert, AuthPost: "PLAIN LOGIN", AuthUser: c.Username, AuthPass: c.Password, RequireAuth: true}), c}
	}
	client := authClient(t, pki, nil)
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, relays*20)
	for round := 0; round < 20; round++ {
		for _, p := range pairs {
			creds := *p.creds // callers reuse nothing: a fresh struct per send
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := client.Send(context.Background(), authRequest(p.mx.Addr("localhost"), &creds))
				if err != nil || !res.Accepted {
					errs <- fmt.Errorf("send failed: %v", err)
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for i, p := range pairs {
		at := p.mx.AuthAttempts()
		if len(at) != 20 {
			t.Fatalf("relay %d saw %d AUTH attempts, want 20", i, len(at))
		}
		for _, a := range at {
			if a.User != p.creds.Username || a.Pass != p.creds.Password {
				t.Fatalf("relay %d received credentials that are not its own: %q", i, a.User)
			}
		}
	}
	waitGoroutines(t, before)
}

// Robustness: every AUTH failure mode plus healthy relay and direct sends
// running concurrently through one Client. Failures must stay isolated.
func TestConcurrentMixedAuthFailuresAndDirectDeliveries(t *testing.T) {
	pki := smtptest.NewPKI(t)
	other := smtptest.NewPKI(t)
	good := pki.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	bad := other.Leaf(t, []string{"localhost"}, localhostIPs, time.Hour)
	mk := func(o smtptest.Options) *smtptest.Server {
		o.Advertise = true
		if o.Cert == nil {
			o.Cert = &good
		}
		if o.AuthUser == "" {
			o.AuthUser, o.AuthPass = markerUser, markerPass
		}
		if o.AuthPost == "" {
			o.AuthPost = "PLAIN LOGIN"
		}
		return smtptest.Start(t, o)
	}
	type target struct {
		mx      *smtptest.Server
		creds   *Credentials
		wantErr bool
	}
	targets := map[string]target{
		"relay ok":       {mk(smtptest.Options{RequireAuth: true}), markerCreds, false},
		"bad creds":      {mk(smtptest.Options{AuthUser: "x", AuthPass: "y"}), markerCreds, true},
		"temp":           {mk(smtptest.Options{AuthReply: "454 4.7.0 later"}), markerCreds, true},
		"stall":          {mk(smtptest.Options{AuthStall: true}), markerCreds, true},
		"drop":           {mk(smtptest.Options{AuthDrop: true}), markerCreds, true},
		"untrusted cert": {mk(smtptest.Options{Cert: &bad}), markerCreds, true},
		"direct":         {mk(smtptest.Options{}), nil, false},
	}
	client := authClient(t, pki, &authRec{})
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	failures := make(chan string, 200)
	for round := 0; round < 8; round++ {
		for name, tg := range targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var creds *Credentials
				if tg.creds != nil {
					c := *tg.creds
					creds = &c
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if name == "stall" && round%2 == 0 { // some sends are cancelled instead of timing out
					time.AfterFunc(80*time.Millisecond, cancel)
				}
				res, err := client.Send(ctx, authRequest(tg.mx.Addr("localhost"), creds))
				if (err != nil) != tg.wantErr || res.Accepted == tg.wantErr {
					failures <- fmt.Sprintf("%s: err=%v accepted=%v", name, err, res.Accepted)
				}
				if err != nil {
					if l := leaks(err.Error(), markerUser, markerPass); len(l) > 0 {
						failures <- fmt.Sprintf("%s: error leaks %v", name, l)
					}
				}
			}()
		}
	}
	wg.Wait()
	close(failures)
	for f := range failures {
		t.Fatal(f)
	}
	for name, tg := range targets {
		if name == "relay ok" || name == "direct" {
			continue
		}
		if tg.mx.SawAny("MAIL") {
			t.Fatalf("%s: message traffic reached a relay that failed TLS or AUTH", name)
		}
	}
	if d := targets["direct"].mx; d.SawAny("AUTH") {
		t.Fatal("direct delivery sent AUTH")
	}
	waitGoroutines(t, before)
}
