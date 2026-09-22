package bimi

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func selfSignedCert(t *testing.T, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Acme Corp"}, CommonName: "Acme Corp"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestParseCertificateValid(t *testing.T) {
	now := time.Now().UTC()
	der := selfSignedCert(t, now.Add(-time.Hour), now.Add(time.Hour))
	info := ParseCertificate(der, now)
	if !info.Parseable || !info.CurrentlyValid {
		t.Fatalf("unexpected result: %+v", info)
	}
	if info.Subject == "" {
		t.Fatal("expected a subject")
	}
}

func TestParseCertificateExpired(t *testing.T) {
	now := time.Now().UTC()
	der := selfSignedCert(t, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	info := ParseCertificate(der, now)
	if !info.Parseable || info.CurrentlyValid {
		t.Fatalf("expected parseable but expired: %+v", info)
	}
}

func TestParseCertificateMalformed(t *testing.T) {
	info := ParseCertificate([]byte("not a certificate"), time.Now())
	if info.Parseable || info.ParseError != CertReasonNotParseable {
		t.Fatalf("expected not-parseable: %+v", info)
	}
}

func TestParseCertificateTooLarge(t *testing.T) {
	huge := make([]byte, MaxCertificateBytes+1)
	info := ParseCertificate(huge, time.Now())
	if info.Parseable || info.ParseError != CertReasonTooLarge {
		t.Fatalf("expected too-large: %+v", info)
	}
}
