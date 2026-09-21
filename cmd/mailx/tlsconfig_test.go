package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/smtp"
)

func TestOutboundTLSDefaultsToOpportunisticWithVerification(t *testing.T) {
	t.Setenv("MAILX_SMTP_TLS_POLICY", "")
	t.Setenv("MAILX_SMTP_TLS_CA_FILE", "")
	cfg, err := outboundTLS(obs{})
	if err != nil || cfg.Policy != smtp.TLSOpportunistic || cfg.RootCAs != nil {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestOutboundTLSPolicyValidation(t *testing.T) {
	t.Setenv("MAILX_SMTP_TLS_POLICY", "required")
	if cfg, err := outboundTLS(obs{}); err != nil || cfg.Policy != smtp.TLSRequired {
		t.Fatalf("required: %+v %v", cfg, err)
	}
	for _, bad := range []string{"off", "none", "insecure", "true"} {
		t.Setenv("MAILX_SMTP_TLS_POLICY", bad)
		if _, err := outboundTLS(obs{}); err == nil {
			t.Fatalf("policy %q must be rejected at startup", bad)
		}
	}
}

func TestOutboundTLSCAFile(t *testing.T) {
	t.Setenv("MAILX_SMTP_TLS_POLICY", "")
	dir := t.TempDir()
	t.Setenv("MAILX_SMTP_TLS_CA_FILE", filepath.Join(dir, "missing.pem"))
	if _, err := outboundTLS(obs{}); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("missing file must fail without echoing the path: %v", err)
	}
	junk := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	t.Setenv("MAILX_SMTP_TLS_CA_FILE", junk)
	if _, err := outboundTLS(obs{}); err == nil {
		t.Fatal("non-PEM file must be rejected")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	good := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	t.Setenv("MAILX_SMTP_TLS_CA_FILE", good)
	cfg, err := outboundTLS(obs{})
	if err != nil || cfg.RootCAs == nil {
		t.Fatalf("valid CA file: %+v %v", cfg, err)
	}
}
