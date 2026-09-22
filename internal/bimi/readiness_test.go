package bimi

import "testing"

func TestReadinessNotConfigured(t *testing.T) {
	res := assess("example.com", DefaultSelector, DNSFinding{Status: StatusNotConfigured}, DMARCPrereq{}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessNotConfigured {
		t.Fatalf("got %q", res.Readiness)
	}
	if res.Disclaimer == "" {
		t.Fatal("every result must carry the disclaimer")
	}
}

func TestReadinessDeclined(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Declined: true}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "reject"}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessDeclined {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestReadinessDMARCPrereqFailedWhenPolicyNone(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg"}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "none"}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessDMARCPrereqFailed {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestReadinessDMARCPrereqFailedWhenNotChecked(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg"}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessDMARCPrereqFailed {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestReadinessReadyWithoutAssetCheck(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg"}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "reject"}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessReady {
		t.Fatalf("got %q — DNS+DMARC alone must be enough when assets were never checked", res.Readiness)
	}
}

func TestReadinessLogoIssueWhenAssetCheckedAndInvalid(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg"}}
	logo := LogoCheck{Checked: true, SVG: SVGResult{Valid: false, Reasons: []string{SVGReasonMissingTitle}}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "reject"}, logo, CertCheck{})
	if res.Readiness != ReadinessLogoIssue {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestReadinessCertificateIssueOnlyWhenAuthorityPresent(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg"}} // no Authority
	cert := CertCheck{Checked: true, Cert: CertInfo{Parseable: false}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "reject"},
		LogoCheck{Checked: true, SVG: SVGResult{Valid: true}}, cert)
	if res.Readiness != ReadinessReady {
		t.Fatalf("a cert check result must be ignored when the record published no a=, got %q", res.Readiness)
	}
}

func TestReadinessCertificateIssueWhenAuthorityPresentAndUnparseable(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg", Authority: "https://example.com/vmc.pem"}}
	cert := CertCheck{Checked: true, Cert: CertInfo{Parseable: false}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "reject"},
		LogoCheck{Checked: true, SVG: SVGResult{Valid: true}}, cert)
	if res.Readiness != ReadinessCertificateIssue {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestReadinessCertificateIssueWhenExpired(t *testing.T) {
	dns := DNSFinding{Status: StatusFound, Record: Record{Location: "https://example.com/logo.svg", Authority: "https://example.com/vmc.pem"}}
	cert := CertCheck{Checked: true, Cert: CertInfo{Parseable: true, CurrentlyValid: false}}
	res := assess("example.com", DefaultSelector, dns, DMARCPrereq{Checked: true, EffectivePolicy: "reject"},
		LogoCheck{Checked: true, SVG: SVGResult{Valid: true}}, cert)
	if res.Readiness != ReadinessCertificateIssue {
		t.Fatalf("a parseable but expired/not-yet-valid certificate must not report ready, got %q", res.Readiness)
	}
}

func TestReadinessInvalidRecord(t *testing.T) {
	res := assess("example.com", DefaultSelector, DNSFinding{Status: StatusInvalid, Reason: ReasonMissingL}, DMARCPrereq{}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessInvalidRecord {
		t.Fatalf("got %q", res.Readiness)
	}
}

func TestReadinessUnknownOnTempError(t *testing.T) {
	res := assess("example.com", DefaultSelector, DNSFinding{Status: StatusTempError}, DMARCPrereq{}, LogoCheck{}, CertCheck{})
	if res.Readiness != ReadinessUnknown {
		t.Fatalf("got %q", res.Readiness)
	}
}
