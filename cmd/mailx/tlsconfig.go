package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/Ferousco-dev/mailx/internal/smtp"
)

// outboundTLS builds the outbound SMTP TLS settings from the environment:
//
//	MAILX_SMTP_TLS_POLICY   opportunistic (default) | required
//	MAILX_SMTP_TLS_CA_FILE  optional PEM bundle of ADDITIONAL trusted roots
//	                        (private CAs, development); the system roots stay
//
// Invalid values are startup errors. There is deliberately no setting that
// disables certificate verification.
func outboundTLS(o obs) (smtp.TLSConfig, error) {
	policy, err := smtp.ParseTLSPolicy(os.Getenv("MAILX_SMTP_TLS_POLICY"))
	if err != nil {
		return smtp.TLSConfig{}, fmt.Errorf("MAILX_SMTP_TLS_POLICY: %w", err)
	}
	cfg := smtp.TLSConfig{Policy: policy}
	if o.metrics != nil {
		cfg.Observer = o.metrics
	}
	path := os.Getenv("MAILX_SMTP_TLS_CA_FILE")
	if path == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return smtp.TLSConfig{}, errors.New("MAILX_SMTP_TLS_CA_FILE: cannot read file")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return smtp.TLSConfig{}, errors.New("MAILX_SMTP_TLS_CA_FILE: no PEM certificates found")
	}
	cfg.RootCAs = pool
	return cfg, nil
}
