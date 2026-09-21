package dmarc

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// DNSStatus is the state of the domain's published DMARC policy. It says
// nothing about how any receiver treated a message.
type DNSStatus string

const (
	StatusUnchecked     DNSStatus = "unchecked"       // no DNS query was made
	StatusNotConfigured DNSStatus = "not_configured"  // no DMARC record governs the domain
	StatusMonitoring    DNSStatus = "monitoring"      // valid; effective policy none
	StatusEnforcing     DNSStatus = "enforcing"       // valid; effective policy quarantine or reject
	StatusConflict      DNSStatus = "conflict"        // multiple DMARC records (RFC 9989: all discarded)
	StatusInvalid       DNSStatus = "invalid"         // malformed or over bounds
	StatusTempError     DNSStatus = "temporary_error" // DNS failed or timed out; nothing concluded
)

// Readiness combines DNS state with MailX's aligned authentication paths.
// "ready" is a precondition for a receiver to pass DMARC, not a receiver result.
const (
	ReadinessUnchecked  = "unchecked"
	ReadinessReady      = "ready"
	ReadinessDNSAction  = "dns_action_required"
	ReadinessAuthIncomp = "authentication_incomplete"
	ReadinessUnknown    = "unknown"
)

// Path statuses for one authentication mechanism.
const (
	PathReady         = "ready"          // identity aligns and the mechanism is configured
	PathNotConfigured = "not_configured" // identity would align but the mechanism is not set up
	PathNotAligned    = "not_aligned"    // identity does not align under the record's mode
	PathUnknown       = "unknown"        // cannot be established (relay, DNS failure, not checked)
)

// Path reasons.
const (
	ReasonNoActiveKey     = "no_active_dkim_key"
	ReasonSPFNotVerified  = "spf_not_verified"
	ReasonRelayReturnPath = "relay_return_path_unknown"
	ReasonSPFUnavailable  = "spf_state_unavailable"
	ReasonNotChecked      = "not_checked"
	ReasonDNS             = "dns_error"
	ReasonTooManyTXT      = "too_many_txt_records"
	ReasonMultiple        = "multiple_dmarc_records"
)

// Sources of the effective policy.
const (
	SourceDomain = "domain"
	SourceOrg    = "organizational_domain"
)

// Path is one aligned-authentication path (DKIM or SPF).
type Path struct {
	Identity string // the authenticated domain MailX would use (d= or MAIL FROM domain)
	Aligned  bool   // identity aligns with the From domain under the record's mode
	Mode     Mode
	Status   string
	Reason   string
}

// Expected is what to publish. Action: create, none, fix_record, merge_records
// or retry_later. Value is only set for create: MailX never rewrites an existing
// policy and never proposes a second record.
type Expected struct{ Action, Type, Name, Value string }

// DNSResult describes the governing DMARC record.
type DNSResult struct {
	Status          DNSStatus
	Reason          string
	Source          string // domain | organizational_domain
	RecordName      string
	Policy          Policy // p as published
	SubdomainPolicy Policy // sp as published
	Effective       Policy // policy applying to this domain
	Testing         bool
	ReportURIs      int
	Published       string
}

// Result is one DMARC readiness assessment.
type Result struct {
	Domain    string
	OrgDomain string
	Mode      string // direct | relay | ""
	Checked   bool
	DNS       DNSResult
	DKIM      Path
	SPF       Path
	Readiness string
	Expected  Expected
	Warnings  []string
}

// SPFView is the SPF state DMARC consumes (strings only: dmarc never imports spf).
type SPFView struct {
	Mode   string // direct | relay
	Status string // spf status vocabulary, empty when not checked
}

// Store, DKIMState and SPFState are the seams to other components. DKIM and SPF
// are consumed as facts; DMARC owns neither.
type Store interface {
	GetDomain(ctx context.Context, tenantID, id string) (database.Domain, error)
}

// DKIMState returns the domain an ACTIVE key signs as, if any.
type DKIMState interface {
	ActiveSigningDomain(ctx context.Context, tenantID, domainID string) (signingDomain string, active bool, err error)
}

// SPFState returns the SPF mode and, when check is true, its verification status.
type SPFState interface {
	SPFView(ctx context.Context, tenantID, domainID string, check bool) (SPFView, error)
}

type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type Observer interface {
	DMARCResult(dnsStatus, readiness string)
}

var (
	ErrDomainNotVerified = database.ErrDomainNotVerified
	ErrBusy              = errors.New("dmarc: verification capacity exhausted")
)

const (
	defaultTimeout   = 5 * time.Second
	maxConcurrent    = 16
	maxQueries       = 8
	dmarcLabel       = "_dmarc."
	recommendedValue = "v=DMARC1; p=none"
)

type Service struct {
	store    Store
	dkim     DKIMState
	spf      SPFState
	resolver TXTResolver
	observer Observer
	timeout  time.Duration
	slots    chan struct{}
}

// NewService builds the service. dkim and spf may be nil (their paths then report
// unknown); nothing else about DMARC depends on them.
func NewService(store Store, resolver TXTResolver, dkim DKIMState, spf SPFState, observer Observer) (*Service, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("dmarc: store and resolver are required")
	}
	return &Service{store: store, dkim: dkim, spf: spf, resolver: resolver, observer: observer,
		timeout: defaultTimeout, slots: make(chan struct{}, maxConcurrent)}, nil
}

// Describe returns the recommendation and MailX's identity model without any DNS
// query (readiness unchecked).
func (s *Service) Describe(ctx context.Context, tenantID, id string) (Result, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	in, err := s.inputs(ctx, tenantID, dom, false)
	if err != nil {
		return Result{}, err
	}
	res := assess(in, dnsFinding{status: StatusUnchecked})
	res.Readiness = ReadinessUnchecked
	return res, nil
}

// Verify inspects public DNS for a VERIFIED domain the tenant owns: at most
// eight TXT queries from _dmarc.<domain> up to its organizational domain, plus
// the SPF state. It stores nothing.
func (s *Service) Verify(ctx context.Context, tenantID, id string) (Result, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	if dom.VerificationStatus != database.DomainVerified {
		return Result{}, ErrDomainNotVerified
	}
	in, err := s.inputs(ctx, tenantID, dom, true)
	if err != nil {
		return Result{}, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Result{}, ErrBusy
	}
	lctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	found := s.walk(lctx, dom.Name)
	if found.status == StatusTempError && ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	res := assess(in, found)
	s.observe(res)
	return res, nil
}

func (s *Service) observe(res Result) {
	if s.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.observer.DMARCResult(string(res.DNS.Status), res.Readiness)
}

// inputs gathers DKIM and SPF facts. Failures degrade a path to unknown; only
// caller cancellation aborts.
func (s *Service) inputs(ctx context.Context, tenantID string, dom database.Domain, checkSPF bool) (input, error) {
	in := input{from: dom.Name, checked: checkSPF}
	if s.dkim != nil {
		d, active, err := s.dkim.ActiveSigningDomain(ctx, tenantID, dom.ID)
		if err != nil {
			if ctx.Err() != nil {
				return in, ctx.Err()
			}
			in.dkimErr = true
		} else {
			in.dkimActive, in.dkimDomain = active, d
		}
	}
	if s.spf != nil {
		v, err := s.spf.SPFView(ctx, tenantID, dom.ID, checkSPF)
		if err != nil {
			if ctx.Err() != nil {
				return in, ctx.Err()
			}
			in.spfErr = true
		} else {
			in.spf = v
		}
	} else {
		in.spfErr = true
	}
	return in, nil
}

// dnsFinding is the outcome of the bounded DNS walk.
type dnsFinding struct {
	status  DNSStatus
	reason  string
	name    string // domain (without _dmarc.) whose record was found
	records []string
	rec     Record
}

// walk queries _dmarc.<name> for the domain and then each parent up to its
// organizational domain, stopping at the first name that has DMARC records.
func (s *Service) walk(ctx context.Context, from string) dnsFinding {
	for _, name := range walkNames(from, maxQueries) {
		records, err := s.resolver.LookupTXT(ctx, dmarcLabel+name)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound && !dnsErr.IsTemporary && !dnsErr.IsTimeout {
				continue
			}
			return dnsFinding{status: StatusTempError, reason: ReasonDNS}
		}
		if len(records) > MaxTXTRecords {
			return dnsFinding{status: StatusInvalid, reason: ReasonTooManyTXT, name: name}
		}
		var candidates []string
		for _, r := range records {
			if IsDMARC(r) {
				candidates = append(candidates, r)
			}
		}
		switch len(candidates) {
		case 0:
			continue
		case 1:
			rec, perr := Parse(candidates[0])
			if perr != nil {
				reason := ReasonBadTag
				var ie *InvalidError
				if errors.As(perr, &ie) {
					reason = ie.Reason
				}
				return dnsFinding{status: StatusInvalid, reason: reason, name: name}
			}
			return dnsFinding{status: StatusMonitoring, name: name, records: candidates, rec: rec}
		default:
			return dnsFinding{status: StatusConflict, reason: ReasonMultiple, name: name}
		}
	}
	return dnsFinding{status: StatusNotConfigured}
}

type input struct {
	from       string
	checked    bool
	dkimActive bool
	dkimDomain string
	dkimErr    bool
	spf        SPFView
	spfErr     bool
}

// assess is the pure core: DNS finding + DKIM/SPF facts -> Result.
func assess(in input, f dnsFinding) Result {
	res := Result{Domain: in.from, Mode: in.spf.Mode, Checked: f.status != StatusUnchecked}
	res.OrgDomain, _ = OrganizationalDomain(in.from)
	res.DNS = DNSResult{Status: f.status, Reason: f.reason, RecordName: recordName(f.name)}

	adkim, aspf := Relaxed, Relaxed // defaults when no usable record exists
	if f.status == StatusMonitoring {
		rec := f.rec
		adkim, aspf = rec.Adkim, rec.Aspf
		res.DNS.Published = f.records[0]
		res.DNS.Policy, res.DNS.SubdomainPolicy, res.DNS.Testing = rec.Policy, rec.SubPolicy, rec.Testing
		res.DNS.ReportURIs = len(rec.RUAHosts)
		res.DNS.Source, res.DNS.Effective = SourceDomain, rec.Policy
		if f.name != in.from { // inherited from a parent: sp applies to existing subdomains
			res.DNS.Source = SourceOrg
			if rec.SubPolicy != "" {
				res.DNS.Effective = rec.SubPolicy
			}
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
		for _, h := range rec.RUAHosts {
			if oh, err := OrganizationalDomain(h); err != nil || !strings.EqualFold(oh, res.OrgDomain) {
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
	dk.Aligned = Aligned(in.from, dk.Identity, adkim)
	switch {
	case in.dkimErr:
		dk.Status, dk.Reason = PathUnknown, ReasonNotChecked
	case !dk.Aligned:
		dk.Status = PathNotAligned
	case !in.dkimActive:
		dk.Status, dk.Reason = PathNotConfigured, ReasonNoActiveKey
	default:
		dk.Status = PathReady
	}

	sp = Path{Identity: in.from, Mode: aspf}
	sp.Aligned = Aligned(in.from, sp.Identity, aspf)
	switch {
	case in.spfErr:
		sp.Status, sp.Reason = PathUnknown, ReasonSPFUnavailable
	case in.spf.Mode == "relay":
		sp.Identity, sp.Aligned = "", false
		sp.Status, sp.Reason = PathUnknown, ReasonRelayReturnPath
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
