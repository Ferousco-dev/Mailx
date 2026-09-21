package dmarc

import (
	"context"
	"errors"
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

// WarnConflictDiscarded: a set of multiple records at another name was discarded
// (RFC 9989 4.10) while the walk found a policy elsewhere.
const WarnConflictDiscarded = "conflicting_records_discarded"

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
	Source          string // domain | organizational_domain | public_suffix_domain
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
	in.align = func(from, id string, mode Mode) (bool, bool) { return Aligned(from, id, mode, nil) }
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
	from, ok := canonical(dom.Name)
	if !ok {
		return Result{}, errors.New("dmarc: stored domain is not a valid name")
	}
	// One tree walk from the From domain yields both the governing policy and its
	// Organizational Domain. Other identities are walked only when relaxed
	// alignment needs their Organizational Domain (never in normal MailX operation,
	// where every identity equals the From domain). Walks are memoized per call.
	walks := map[string]*walkResult{}
	orgOf := func(d string) (string, bool) {
		if w, ok := walks[d]; ok {
			return w.org, true
		}
		w, err := s.treeWalk(lctx, d)
		if err != nil {
			return "", false
		}
		walks[d] = w
		return w.org, true
	}
	found := dnsFinding{status: StatusTempError, reason: ReasonDNS}
	if w, err := s.treeWalk(lctx, from); err == nil {
		walks[from] = w
		found = findingFrom(w)
	} else if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	in.align = func(f, id string, mode Mode) (bool, bool) { return Aligned(f, id, mode, orgOf) }
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
	in := input{from: dom.Name, checked: checkSPF, spfIdentity: dom.Name}
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
