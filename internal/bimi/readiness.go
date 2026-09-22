package bimi

// Readiness states. Deliberately explicit/factual rather than a score —
// each answers a specific question about MailX's OWN checks, never about
// what a mailbox provider will do.
const (
	ReadinessUnchecked         = "unchecked"
	ReadinessNotConfigured     = "not_configured" // no BIMI record found at the default selector or org domain
	ReadinessDeclined          = "declined"       // l= published empty: an explicit statement of non-publication
	ReadinessInvalidRecord     = "invalid_record"
	ReadinessDMARCPrereqFailed = "dmarc_prerequisite_failed"
	ReadinessLogoIssue         = "logo_issue"        // asset validation was requested and the logo failed it
	ReadinessCertificateIssue  = "certificate_issue" // a= present, asset validation requested, certificate failed to parse
	ReadinessReady             = "ready"
	ReadinessUnknown           = "unknown" // DNS temporary failure; nothing concluded
)

// Disclaimer is included verbatim on every Result. MailX must never let a
// caller read "ready" as "the logo will display" — display is entirely at
// each mailbox provider's discretion (see the BIMI Internet-Draft: "MUAs
// have final control over the user interface... and MAY use alternate
// Indicators... or no Indicator at all").
const Disclaimer = "MailX readiness reflects DNS record, DMARC prerequisite, and (when requested) logo/certificate structural checks only. " +
	"It is not a guarantee that any mailbox provider will display the logo — display is entirely at each provider's discretion."

// DNSStatus mirrors dmarc's DNSStatus vocabulary for the BIMI record.
type DNSStatus string

const (
	StatusUnchecked     DNSStatus = "unchecked"
	StatusNotConfigured DNSStatus = "not_configured"
	StatusFound         DNSStatus = "found"
	StatusInvalid       DNSStatus = "invalid"
	StatusTempError     DNSStatus = "temporary_error"
)

// DMARCPrereq is the DMARC fact BIMI consumes — never re-derived. Per the
// BIMI draft, the Author Domain's DMARC result must be enforcing (policy
// not "none"). MailX's DMARC implementation follows RFC 9989, which
// removed the pct= partial-rollout tag entirely (see internal/dmarc's
// package doc) — so unlike the older RFC 7489-era "pct=100 required at
// p=quarantine" reading some BIMI guidance still quotes, MailX's DMARC
// state already represents full enforcement whenever policy != none; no
// separate pct check is meaningful or necessary here.
type DMARCPrereq struct {
	Checked         bool
	EffectivePolicy string // "" | "none" | "quarantine" | "reject"
	OrgDomain       string
}

func (d DMARCPrereq) satisfied() bool {
	return d.Checked && d.EffectivePolicy != "" && d.EffectivePolicy != "none"
}

// LogoCheck is the outcome of fetching+validating the logo, when asset
// validation was requested. Checked=false means it was not requested.
type LogoCheck struct {
	Checked  bool
	FetchErr string // bounded reason, empty on success
	SVG      SVGResult
}

// CertCheck is the outcome of fetching+parsing the authority certificate,
// when asset validation was requested and a= was present.
type CertCheck struct {
	Checked  bool
	FetchErr string
	Cert     CertInfo
}

// Result is one BIMI readiness assessment.
type Result struct {
	Domain     string
	Selector   string
	DNS        DNSFinding
	DMARC      DMARCPrereq
	Logo       LogoCheck
	Cert       CertCheck
	Readiness  string
	Disclaimer string
}

// DNSFinding is what MailX's DNS discovery found.
type DNSFinding struct {
	Status     DNSStatus
	Reason     string // bounded InvalidError.Reason when Status==invalid
	Source     string // "domain" | "organizational_domain"
	RecordName string
	Published  string
	Record     Record
}

// assess is the pure core: DNS finding + DMARC fact + optional asset
// checks -> Result. No I/O happens here.
func assess(domain, selector string, dns DNSFinding, dmarcFact DMARCPrereq, logo LogoCheck, cert CertCheck) Result {
	res := Result{
		Domain: domain, Selector: selector, DNS: dns, DMARC: dmarcFact, Logo: logo, Cert: cert,
		Disclaimer: Disclaimer,
	}
	res.Readiness = readiness(dns, dmarcFact, logo, cert)
	return res
}

func readiness(dns DNSFinding, dmarcFact DMARCPrereq, logo LogoCheck, cert CertCheck) string {
	switch dns.Status {
	case StatusUnchecked:
		return ReadinessUnchecked
	case StatusTempError:
		return ReadinessUnknown
	case StatusNotConfigured:
		return ReadinessNotConfigured
	case StatusInvalid:
		return ReadinessInvalidRecord
	}
	// StatusFound from here on.
	if dns.Record.Declined {
		return ReadinessDeclined
	}
	if !dmarcFact.satisfied() {
		return ReadinessDMARCPrereqFailed
	}
	if logo.Checked {
		if logo.FetchErr != "" || !logo.SVG.Valid {
			return ReadinessLogoIssue
		}
	}
	if cert.Checked && dns.Record.Authority != "" {
		if cert.FetchErr != "" || !cert.Cert.Parseable || !cert.Cert.CurrentlyValid {
			return ReadinessCertificateIssue
		}
	}
	return ReadinessReady
}
