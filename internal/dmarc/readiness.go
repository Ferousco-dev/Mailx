package dmarc

import "strings"

// The pure readiness model: DNS findings and DKIM/SPF facts in, Result out. No
// I/O happens here, so every rule is tested without a resolver.

// dnsFinding is the DNS-derived part of an assessment.
type dnsFinding struct {
	status    DNSStatus
	reason    string
	org       string
	name      string // domain (without _dmarc.) whose record governs, when any
	source    string
	raw       string
	rec       Record
	conflicts []string
	authorHit bool // the starting domain's own name held a discarded multiple-record set
	start     string
}

// findingFrom derives status and policy facts from a tree-walk result.
func findingFrom(w *walkResult) dnsFinding {
	f := dnsFinding{org: w.org, conflicts: w.conflicts, start: w.start, status: StatusNotConfigured}
	for _, c := range w.conflicts {
		if c == w.start {
			f.authorHit = true
		}
	}
	switch {
	case f.authorHit || (w.policy == nil && len(w.conflicts) > 0):
		// Receivers discard the conflicting set (RFC 9989 4.10), which silently
		// changes which policy applies; the owner must fix it.
		f.status, f.reason, f.name = StatusConflict, ReasonMultiple, w.conflicts[0]
	case w.policy == nil:
	case w.policy.invalid != "":
		f.status, f.reason, f.name, f.source = StatusInvalid, w.policy.invalid, w.policy.name, w.policy.source
	default:
		f.status, f.name, f.source, f.raw, f.rec = StatusMonitoring, w.policy.name, w.policy.source, w.policy.raw, w.policy.rec
	}
	return f
}

type input struct {
	from       string
	checked    bool
	dkimActive bool
	dkimDomain string
	dkimErr    bool
	spf        SPFView
	spfErr     bool
	// spfIdentity is the authenticated SPF domain (the MAIL FROM domain). In MailX
	// direct mode it equals the From domain (the envelope sender is the API From
	// address; see TestMailFromEqualsFromDomain); it is a separate fact so that
	// alignment is computed between two identities, never assumed.
	spfIdentity string
	// align relates the From domain to an identity; known=false means relaxed
	// alignment needed an Organizational Domain that DNS could not provide.
	align func(from, id string, mode Mode) (aligned, known bool)
}

// assess is the pure core: DNS finding + DKIM/SPF facts -> Result.
func assess(in input, f dnsFinding) Result {
	res := Result{Domain: in.from, Mode: in.spf.Mode, Checked: f.status != StatusUnchecked}
	res.OrgDomain = f.org
	res.DNS = DNSResult{Status: f.status, Reason: f.reason, RecordName: recordName(f.name), Source: f.source}

	adkim, aspf := Relaxed, Relaxed // defaults when no usable record exists
	if f.status == StatusMonitoring {
		rec := f.rec
		adkim, aspf = rec.Adkim, rec.Aspf
		res.DNS.Published = f.raw
		res.DNS.Policy, res.DNS.SubdomainPolicy, res.DNS.Testing = rec.Policy, rec.SubPolicy, rec.Testing
		res.DNS.ReportURIs = len(rec.RUAHosts)
		res.DNS.Effective = rec.Policy
		if f.source != SourceDomain && rec.SubPolicy != "" { // RFC 9989 4.10.1: sp for existing subdomains
			res.DNS.Effective = rec.SubPolicy
		}
		if res.DNS.Effective != PolicyNone {
			res.DNS.Status = StatusEnforcing
		}
		res.Warnings = append(res.Warnings, rec.Warnings...)
		if res.DNS.Effective == PolicyNone {
			res.Warnings = appendOnce(res.Warnings, WarnMonitoringOnly)
		}
		if rec.Testing {
			res.Warnings = appendOnce(res.Warnings, WarnTesting)
		}
		if len(f.conflicts) > 0 {
			res.Warnings = appendOnce(res.Warnings, WarnConflictDiscarded)
		}
		for _, h := range rec.RUAHosts {
			if !withinOrg(h, f.org) {
				res.Warnings = appendOnce(res.Warnings, WarnExternalReportDest)
			}
		}
	}
	res.DKIM, res.SPF = paths(in, adkim, aspf)
	if in.spf.Mode == "relay" && res.DKIM.Status == PathReady {
		res.Warnings = appendOnce(res.Warnings, WarnRelayAlters)
	}
	res.Expected = expected(in.from, res.DNS.Status)
	res.Readiness = readiness(res.DNS.Status, res.DKIM, res.SPF)
	return res
}

// withinOrg reports whether host is the Organizational Domain or a subdomain of
// it, comparing whole labels (never a string suffix).
func withinOrg(host, org string) bool {
	h, ok1 := canonical(host)
	if !ok1 || org == "" {
		return false
	}
	hl, ol := strings.Split(h, "."), strings.Split(org, ".")
	if len(hl) < len(ol) {
		return false
	}
	for i := range ol {
		if hl[len(hl)-len(ol)+i] != ol[i] {
			return false
		}
	}
	return true
}

func recordName(name string) string {
	if name == "" {
		return ""
	}
	return dmarcLabel + name
}

func expected(from string, st DNSStatus) Expected {
	switch st {
	case StatusNotConfigured, StatusUnchecked:
		return Expected{Action: "create", Type: "TXT", Name: dmarcLabel + from, Value: recommendedValue}
	case StatusMonitoring, StatusEnforcing:
		return Expected{Action: "none"} // an existing policy is never rewritten
	case StatusConflict:
		return Expected{Action: "merge_records"}
	case StatusInvalid:
		return Expected{Action: "fix_record"}
	}
	return Expected{Action: "retry_later"}
}

// paths models both aligned-authentication paths. DKIM: MailX signs with the
// From domain once a key is active. SPF: MAIL FROM equals the API From address
// in direct mode, so its domain is the From domain; in relay mode the relay may
// rewrite the return-path, which MailX cannot know.
func paths(in input, adkim, aspf Mode) (dk, sp Path) {
	dk = Path{Identity: in.from, Mode: adkim}
	if in.dkimActive && in.dkimDomain != "" {
		dk.Identity = in.dkimDomain
	}
	dkKnown := true
	dk.Aligned, dkKnown = in.align(in.from, dk.Identity, adkim)
	switch {
	case in.dkimErr:
		dk.Status, dk.Reason = PathUnknown, ReasonNotChecked
	case !dkKnown:
		dk.Status, dk.Reason = PathUnknown, ReasonDNS
	case !dk.Aligned:
		dk.Status = PathNotAligned
	case !in.dkimActive:
		dk.Status, dk.Reason = PathNotConfigured, ReasonNoActiveKey
	default:
		dk.Status = PathReady
	}

	sp = Path{Identity: in.spfIdentity, Mode: aspf}
	if sp.Identity == "" {
		sp.Identity = in.from
	}
	spKnown := true
	sp.Aligned, spKnown = in.align(in.from, sp.Identity, aspf)
	switch {
	case in.spfErr:
		sp.Status, sp.Reason = PathUnknown, ReasonSPFUnavailable
	case in.spf.Mode == "relay":
		sp.Identity, sp.Aligned = "", false
		sp.Status, sp.Reason = PathUnknown, ReasonRelayReturnPath
	case !spKnown:
		sp.Status, sp.Reason = PathUnknown, ReasonDNS
	case !sp.Aligned:
		sp.Status = PathNotAligned
	case !in.checked:
		sp.Status, sp.Reason = PathUnknown, ReasonNotChecked
	case in.spf.Status == "verified":
		sp.Status = PathReady
	case in.spf.Status == "temporary_error" || in.spf.Status == "sending_infrastructure_unknown" || in.spf.Status == "":
		sp.Status, sp.Reason = PathUnknown, ReasonSPFUnavailable
	default: // not_configured, mismatch, conflict, invalid
		sp.Status, sp.Reason = PathNotConfigured, ReasonSPFNotVerified
	}
	return dk, sp
}

func readiness(st DNSStatus, dk, sp Path) string {
	switch st {
	case StatusUnchecked:
		return ReadinessUnchecked
	case StatusTempError:
		return ReadinessUnknown
	case StatusNotConfigured, StatusInvalid, StatusConflict:
		return ReadinessDNSAction
	}
	switch {
	case dk.Status == PathReady || sp.Status == PathReady:
		return ReadinessReady
	case dk.Status == PathUnknown || sp.Status == PathUnknown:
		return ReadinessUnknown
	}
	return ReadinessAuthIncomp
}
