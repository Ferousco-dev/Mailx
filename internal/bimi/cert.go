package bimi

import (
	"crypto/x509"
	"encoding/pem"
	"time"
)

// MaxCertificateBytes bounds the fetched authority-evidence document
// (VMC/CMC). X.509 certificates, even certificate chains, are small; this
// is generous headroom, not a real-world expectation.
const MaxCertificateBytes = 64 * 1024

// CertInfo reports facts MailX can truthfully establish about a fetched
// Mark Certificate by parsing it — NEVER whether a mailbox provider's trust
// store would accept it. "Minimum Security Requirements for Issuance of
// Mark Certificates" is a separate specification with its own CA/trust
// model that MailX does not implement, does not verify against, and does
// not claim to satisfy. MailX has no private CA and never stores or
// generates a private key for BIMI (see .ilana/architecture.md).
type CertInfo struct {
	// Parseable is true when the fetched bytes decode as a well-formed
	// X.509 certificate (PEM or raw DER).
	Parseable bool
	// ParseError is a bounded reason code when Parseable is false.
	ParseError string
	Subject    string
	Issuer     string
	NotBefore  time.Time
	NotAfter   time.Time
	// CurrentlyValid reports only the NotBefore/NotAfter time window —
	// NOT chain-of-trust, revocation, or BIMI-authority validity.
	CurrentlyValid bool
}

const (
	CertReasonTooLarge     = "certificate_too_large"
	CertReasonNotParseable = "certificate_not_parseable"
)

// ParseCertificate structurally parses fetched authority-evidence bytes
// (PEM-encoded, or raw DER as a fallback). It performs no chain building,
// no revocation check and no BIMI-authority trust-anchor comparison.
func ParseCertificate(data []byte, now time.Time) CertInfo {
	if len(data) > MaxCertificateBytes {
		return CertInfo{ParseError: CertReasonTooLarge}
	}
	der := data
	if block, _ := pem.Decode(data); block != nil {
		der = block.Bytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return CertInfo{ParseError: CertReasonNotParseable}
	}
	info := CertInfo{
		Parseable: true,
		Subject:   cert.Subject.String(),
		Issuer:    cert.Issuer.String(),
		NotBefore: cert.NotBefore,
		NotAfter:  cert.NotAfter,
	}
	info.CurrentlyValid = !now.Before(cert.NotBefore) && now.Before(cert.NotAfter)
	return info
}
