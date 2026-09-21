package dkim

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	msgauth "github.com/emersion/go-msgauth/dkim"

	"github.com/Ferousco-dev/mailx/internal/outbound"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func sharedKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		testKey = k
	})
	return testKey
}

func signOpts(t testing.TB, domain string) Options {
	return Options{Domain: domain, Selector: "mx20260921ab12", Key: sharedKey(t), Now: time.Unix(1790000000, 0)}
}

// verify checks a signed message with an INDEPENDENT implementation
// (go-msgauth) against the public key as it would be published in DNS.
func verify(t testing.TB, signed []byte, domain, selector string, pub *rsa.PublicKey) error {
	t.Helper()
	p, err := PublicKeyBase64(pub)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) ([]string, error) {
		if name == DNSName(selector, domain) {
			return []string{DNSValue(p)}, nil
		}
		return nil, errors.New("no such record")
	}
	res, err := msgauth.VerifyWithOptions(strings.NewReader(string(signed)), &msgauth.VerifyOptions{LookupTXT: lookup})
	if err != nil {
		return err
	}
	if len(res) != 1 {
		return errors.New("expected exactly one signature")
	}
	if res[0].Domain != domain {
		t.Fatalf("d= is %q, want %q", res[0].Domain, domain)
	}
	return res[0].Err
}

func built(t testing.TB, req outbound.Request) []byte {
	t.Helper()
	req.MessageID = "<abc123@mailx.local>"
	req.Date = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	b, err := outbound.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(b.Raw)
}

func TestSignedMessagesVerifyIndependently(t *testing.T) {
	cases := map[string]outbound.Request{
		"text":        {From: "Alice <alice@example.com>", To: []string{"bob@example.net"}, Subject: "hello", Text: "plain body\nline two"},
		"html":        {From: "alice@example.com", To: []string{"bob@example.net"}, Subject: "html", HTML: "<b>hi</b>"},
		"alternative": {From: "alice@example.com", To: []string{"bob@example.net"}, Cc: []string{"c@example.net"}, ReplyTo: "r@example.com", Subject: "ünïcode", Text: "t", HTML: "<p>h</p>"},
	}
	for name, req := range cases {
		signed, err := Sign(built(t, req), signOpts(t, "example.com"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := verify(t, signed, "example.com", "mx20260921ab12", &sharedKey(t).PublicKey); err != nil {
			t.Fatalf("%s: independent verification failed: %v\n%s", name, err, signed)
		}
	}
}

const mixedMessage = "From: Alice <alice@example.com>\r\nTo: bob@example.net\r\nSubject: report\r\nDate: Mon, 21 Sep 2026 12:00:00 +0000\r\n" +
	"Message-ID: <m1@mailx.local>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"BOUND\"\r\n\r\n" +
	"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSee attached.\r\n" +
	"--BOUND\r\nContent-Type: application/octet-stream; name=\"data.bin\"\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"data.bin\"\r\n\r\n" +
	"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+P0BB\r\n--BOUND--\r\n"

func TestMultipartMixedWithAttachmentVerifiesAndAttachmentIsProtected(t *testing.T) {
	signed, err := Sign([]byte(mixedMessage), signOpts(t, "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, signed, "example.com", "mx20260921ab12", &sharedKey(t).PublicKey); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(signed), "AAECAwQF", "AAECAwQG", 1)
	if err := verify(t, []byte(tampered), "example.com", "mx20260921ab12", &sharedKey(t).PublicKey); err == nil {
		t.Fatal("changing attachment content must break the signature")
	}
}

// Mutation: the signature must actually protect the message.
func TestMutationsBreakTheSignature(t *testing.T) {
	signed, err := Sign(built(t, outbound.Request{From: "alice@example.com", To: []string{"bob@example.net"}, Subject: "original subject", Text: "original body"}), signOpts(t, "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	pub := &sharedKey(t).PublicKey
	s := string(signed)
	mutations := map[string]string{
		"From":    strings.Replace(s, "alice@example.com", "mallory@example.com", 1),
		"To":      strings.Replace(s, "bob@example.net", "eve@example.net", 1),
		"Subject": strings.Replace(s, "original subject", "changed subject", 1),
		"Date":    strings.Replace(s, "12:00:00", "13:00:00", 1),
		"body":    strings.Replace(s, "b3JpZ2luYWwgYm9keQ", "Y2hhbmdlZCBib2R5AA", 1),
	}
	for name, m := range mutations {
		if m == s {
			t.Fatalf("%s: mutation did not change the message", name)
		}
		if err := verify(t, []byte(m), "example.com", "mx20260921ab12", pub); err == nil {
			t.Fatalf("modified %s still verifies", name)
		}
	}
	if err := verify(t, signed, "example.com", "mx20260921ab12", pub); err != nil {
		t.Fatalf("unmodified message must verify: %v", err)
	}
}

// Relaxed canonicalization tolerates re-wrapping and trailing whitespace.
func TestRelaxedCanonicalizationToleratesPermittedChanges(t *testing.T) {
	msg := "From: alice@example.com\r\nTo: bob@example.net\r\nSubject: spaced   subject\r\nDate: Mon, 21 Sep 2026 12:00:00 +0000\r\n\r\nline one  \r\nline\ttwo\r\n\r\n\r\n"
	signed, err := Sign([]byte(msg), signOpts(t, "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	pub := &sharedKey(t).PublicKey
	s := string(signed)
	changes := map[string]string{
		"trailing empty lines": s + "\r\n\r\n",
		"header rewrap":        strings.Replace(s, "Subject: spaced   subject", "Subject: spaced\r\n subject", 1),
		"trailing body spaces": strings.Replace(s, "line one  \r\n", "line one\r\n", 1),
		"tab to space in body": strings.Replace(s, "line\ttwo", "line two", 1),
		"header name case":     strings.Replace(s, "\r\nSubject:", "\r\nSUBJECT:", 1),
	}
	for name, c := range changes {
		if c == s {
			t.Fatalf("%s: no change applied", name)
		}
		if err := verify(t, []byte(c), "example.com", "mx20260921ab12", pub); err != nil {
			t.Fatalf("%s should still verify under relaxed/relaxed: %v", name, err)
		}
	}
}

// RFC 6376 section 3.4.5 worked examples.
func TestCanonicalizationMatchesRFC6376Examples(t *testing.T) {
	if got := relaxedHeader("B : Y\t\r\n\tZ  "); got != "b:Y Z" {
		t.Fatalf("header B: %q", got)
	}
	if got := relaxedHeader("A: X"); got != "a:X" {
		t.Fatalf("header A: %q", got)
	}
	if got := string(relaxedBody([]byte(" C \r\nD \t E\r\n\r\n\r\n"))); got != " C\r\nD E\r\n" {
		t.Fatalf("body: %q", got)
	}
	if relaxedBody(nil) != nil || relaxedBody([]byte("\r\n\r\n")) != nil {
		t.Fatal("empty and blank-only bodies canonicalize to nothing")
	}
	if got := string(relaxedBody([]byte("no trailing crlf"))); got != "no trailing crlf\r\n" {
		t.Fatalf("missing final CRLF must be added: %q", got)
	}
}

func TestSigningDomainMustMatchFromAndFromMustBeUnique(t *testing.T) {
	good := built(t, outbound.Request{From: "alice@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t"})
	if _, err := Sign(good, signOpts(t, "other.example")); !errors.Is(err, ErrDomainMismatch) {
		t.Fatalf("wrong-domain signing must be refused, got %v", err)
	}
	for name, msg := range map[string]string{
		"no from":      "To: a@example.net\r\nSubject: s\r\n\r\nbody\r\n",
		"two from":     "From: a@example.com\r\nFrom: b@example.com\r\n\r\nbody\r\n",
		"unparseable":  "From: not an address\r\n\r\nbody\r\n",
		"no separator": "From: a@example.com\r\nSubject: s",
		"bad header":   "From: a@example.com\r\nnot a header line\r\n\r\nbody\r\n",
	} {
		if _, err := Sign([]byte(msg), signOpts(t, "example.com")); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	// Subdomain From with a parent-domain key is refused too (exact match).
	sub := built(t, outbound.Request{From: "a@mail.example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t"})
	if _, err := Sign(sub, signOpts(t, "example.com")); !errors.Is(err, ErrDomainMismatch) {
		t.Fatalf("subdomain message must not be signed with the parent key: %v", err)
	}
}

func TestBccNeverAppearsInSignedMessage(t *testing.T) {
	raw := built(t, outbound.Request{From: "alice@example.com", To: []string{"bob@example.net"}, Bcc: []string{"hidden-recipient@secret.example"}, Subject: "s", Text: "t"})
	signed, err := Sign(raw, signOpts(t, "example.com"))
	if err != nil {
		t.Fatal(err)
	}
	// The hidden address must appear nowhere, and no header may be named Bcc
	// (checked on header names and the h= list, not on the random signature text).
	if strings.Contains(strings.ToLower(string(signed)), "hidden-recipient") {
		t.Fatalf("hidden Bcc recipient leaked into the signed message:\n%s", signed)
	}
	head, _, _ := strings.Cut(string(signed), "\r\n\r\n")
	unfolded := strings.NewReplacer("\r\n\t", " ", "\r\n ", " ").Replace(head)
	for _, line := range strings.Split(unfolded, "\r\n") {
		name, value, _ := strings.Cut(line, ":")
		if strings.EqualFold(strings.TrimSpace(name), "bcc") {
			t.Fatalf("a Bcc header was written: %q", line)
		}
		if strings.EqualFold(name, "DKIM-Signature") {
			for _, tag := range strings.Split(value, ";") {
				if k, v, _ := strings.Cut(strings.TrimSpace(tag), "="); k == "h" && strings.Contains(strings.ToLower(strings.ReplaceAll(v, " ", "")), "bcc") {
					t.Fatalf("h= lists bcc: %s", v)
				}
			}
		}
	}
}

func TestSignatureHeaderTagsAndSignedHeaderPolicy(t *testing.T) {
	signed, _ := Sign(built(t, outbound.Request{From: "alice@example.com", To: []string{"b@example.net"}, Cc: []string{"c@example.net"}, Subject: "s", Text: "t"}), signOpts(t, "example.com"))
	head, _, _ := strings.Cut(string(signed), "\r\nFrom:")
	flat := strings.ReplaceAll(strings.NewReplacer("\r\n", "", "\t", " ").Replace(head), ": ", ":")
	flat = strings.ReplaceAll(flat, "DKIM-Signature:v=1", "DKIM-Signature: v=1")
	for _, want := range []string{"v=1;", "a=rsa-sha256;", "c=relaxed/relaxed;", "d=example.com;", "s=mx20260921ab12;", "t=1790000000;", "h=from:to:cc:subject:date:message-id:mime-version:content-type:content-transfer-encoding;"} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q in %s", want, flat)
		}
	}
	for _, banned := range []string{"rsa-sha1", "l=", "x=", "i=", "Received", "Return-Path"} {
		if strings.Contains(flat, banned) {
			t.Errorf("unexpected %q in signature header", banned)
		}
	}
	for _, line := range strings.Split(head, "\r\n") {
		if len(line) > 78 {
			t.Errorf("signature header line exceeds 78 chars (%d)", len(line))
		}
	}
}

func TestKeyPolicyAndFormats(t *testing.T) {
	k := sharedKey(t)
	if k.N.BitLen() != 2048 {
		t.Fatalf("generated key has %d bits", k.N.BitLen())
	}
	der, _ := MarshalPrivate(k)
	back, err := ParsePrivate(der)
	if err != nil || back.N.Cmp(k.N) != 0 {
		t.Fatalf("round trip: %v", err)
	}
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	weakDER, _ := x509.MarshalPKCS8PrivateKey(weak)
	if _, err := ParsePrivate(weakDER); err == nil {
		t.Fatal("1024-bit keys must be refused")
	}
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	edDER, _ := x509.MarshalPKCS8PrivateKey(edKey)
	if _, err := ParsePrivate(edDER); err == nil {
		t.Fatal("non-RSA keys must be refused")
	}
	if _, err := ParsePrivate([]byte("garbage")); err == nil {
		t.Fatal("garbage must be refused")
	}
	other, _ := GenerateKey()
	if other.N.Cmp(k.N) == 0 {
		t.Fatal("two generated keys must differ")
	}
}

func TestDNSRecordFormat(t *testing.T) {
	p, _ := PublicKeyBase64(&sharedKey(t).PublicKey)
	v := DNSValue(p)
	if !strings.HasPrefix(v, "v=DKIM1; k=rsa; p=") || strings.Contains(v, "PRIVATE") || strings.ContainsAny(v, "\r\n\"") {
		t.Fatalf("bad record %q", v)
	}
	if DNSName("mx1", "example.com") != "mx1._domainkey.example.com" {
		t.Fatal("record name")
	}
	chunks := DNSChunks(v)
	if len(chunks) < 2 || strings.Join(chunks, "") != v {
		t.Fatalf("2048-bit record should split into >=2 strings that rejoin exactly, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 255 {
			t.Fatal("chunk over 255 bytes")
		}
	}
}

func TestConcurrentSigningIsRaceFreeAndAlwaysVerifies(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signed, err := Sign(built(t, outbound.Request{From: "alice@example.com", To: []string{"b@example.net"}, Subject: "s", Text: strings.Repeat("x", 1+i)}), signOpts(t, "example.com"))
			if err == nil {
				err = verify(t, signed, "example.com", "mx20260921ab12", &sharedKey(t).PublicKey)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

var selectorPattern = regexp.MustCompile(`^mx[0-9]{8}[0-9a-f]{4}$`)

func parsePublic(der []byte) (*rsa.PublicKey, error) {
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	return pub.(*rsa.PublicKey), nil
}
