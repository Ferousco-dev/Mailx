package spf

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// Status is the bounded verification result, safe as a metric label and in an
// API response. It says nothing about ownership, DKIM or deliverability.
type Status string

const (
	StatusUnchecked      Status = "unchecked"                      // no DNS lookup was made
	StatusVerified       Status = "verified"                       // the published record literally authorizes MailX's infrastructure
	StatusNotConfigured  Status = "not_configured"                 // no SPF record is published
	StatusMismatch       Status = "mismatch"                       // a record exists but does not authorize MailX's infrastructure
	StatusConflict       Status = "conflict"                       // more than one SPF record (RFC 7208 4.5: permerror)
	StatusInvalid        Status = "invalid"                        // the published record is malformed or exceeds MailX's bounds
	StatusTemporaryError Status = "temporary_error"                // DNS failed or timed out; nothing is concluded
	StatusSendingUnknown Status = "sending_infrastructure_unknown" // MailX was not told its public IPs / relay include
)

// Expected describes what to publish. Action is one of: create,
// update_existing, none, merge_records, fix_record, declare_sending_ips,
// follow_relay_provider. Value is empty when MailX cannot truthfully give one.
type Expected struct {
	Action string
	Type   string
	Name   string
	Value  string
}

// Result is one SPF assessment for one domain. Published is the single SPF
// record found (already bounded); it is never logged.
type Result struct {
	Mode        Mode
	Domain      string
	Status      Status
	Reason      string
	Published   string
	Expected    Expected
	Warnings    []string
	Unevaluated bool
}

// Warning codes.
const (
	WarnUnevaluated   = "unevaluated_mechanisms"
	WarnPermitsAll    = "permits_all"
	WarnLookupLimit   = "dns_lookup_limit_risk"
	WarnDeprecatedPTR = "deprecated_ptr"
)

// Reason codes besides the parser's.
const (
	ReasonNotListed       = "sending_address_not_listed"
	ReasonExplicitlyDeny  = "sending_address_not_passed"
	ReasonIncludeMissing  = "relay_include_missing"
	ReasonMultipleRecords = "multiple_spf_records"
	ReasonDNS             = "dns_error"
	ReasonMergeTooLong    = "merged_record_too_long"
)

var (
	ErrDomainNotVerified = database.ErrDomainNotVerified
	// ErrBusy means the verification concurrency bound was hit; retry later.
	ErrBusy = errors.New("spf: verification capacity exhausted")
)

// Store is the persistence SPF needs: a tenant-scoped domain read. SPF keeps
// no state of its own; DNS is the source of truth and is never cached.
type Store interface {
	GetDomain(ctx context.Context, tenantID, id string) (database.Domain, error)
}

// TXTResolver is the DNS boundary (same shape as the ownership and DKIM ones).
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// Observer receives one bounded event per verification.
type Observer interface {
	SPFResult(mode, outcome string)
}

const (
	defaultTimeout   = 5 * time.Second
	maxConcurrentDNS = 16
)

// Service inspects and verifies SPF for a tenant's domains. Safe for
// concurrent use; it holds no per-domain state.
type Service struct {
	store    Store
	resolver TXTResolver
	cfg      Config
	observer Observer
	timeout  time.Duration
	slots    chan struct{}
}

func NewService(store Store, resolver TXTResolver, cfg Config, observer Observer) (*Service, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("spf: store and resolver are required")
	}
	if cfg.Mode != ModeDirect && cfg.Mode != ModeRelay {
		return nil, errors.New("spf: mode must be direct or relay")
	}
	for _, ip := range cfg.IPs {
		if err := CheckPublicIP(ip); err != nil {
			return nil, errors.New("spf: sending address is not public")
		}
	}
	return &Service{store: store, resolver: resolver, cfg: cfg, observer: observer,
		timeout: defaultTimeout, slots: make(chan struct{}, maxConcurrentDNS)}, nil
}

// Config returns the declared sending infrastructure.
func (s *Service) Config() Config { return s.cfg }

// Describe returns what to publish for a domain the tenant owns, without any
// DNS query (status unchecked).
func (s *Service) Describe(ctx context.Context, tenantID, id string) (Result, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	res := s.unchecked(dom.Name)
	return res, nil
}

func (s *Service) unchecked(domain string) Result {
	res := Result{Mode: s.cfg.Mode, Domain: domain, Status: StatusUnchecked}
	if !s.cfg.Ready() {
		res.Status = StatusSendingUnknown
		res.Expected = unknownExpected(s.cfg.Mode)
		return res
	}
	res.Expected = createExpected(domain, s.cfg)
	return res
}

// Verify performs one bounded TXT lookup for a VERIFIED domain the tenant owns
// and assesses the result. Ownership is required so the endpoint cannot be used
// to probe arbitrary domains' DNS.
func (s *Service) Verify(ctx context.Context, tenantID, id string) (Result, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	if dom.VerificationStatus != database.DomainVerified {
		return Result{}, ErrDomainNotVerified
	}
	if !s.cfg.Ready() {
		res := s.unchecked(dom.Name)
		s.observe(res)
		return res, nil
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Result{}, ErrBusy
	}
	lctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	records, lerr := s.resolver.LookupTXT(lctx, dom.Name)
	if lerr != nil && ctx.Err() != nil {
		return Result{}, ctx.Err() // the caller went away; nothing to report
	}
	res := Analyze(s.cfg, dom.Name, records, lerr)
	s.observe(res)
	return res, nil
}

func (s *Service) observe(res Result) {
	if s.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.observer.SPFResult(string(res.Mode), string(res.Status))
}

// Analyze is the pure core: given the TXT answer (or lookup error) for a
// domain, classify it. Exported for tests; it never touches the network.
func Analyze(cfg Config, domain string, records []string, lookupErr error) Result {
	res := Result{Mode: cfg.Mode, Domain: domain}
	if !cfg.Ready() {
		res.Status, res.Expected = StatusSendingUnknown, unknownExpected(cfg.Mode)
		return res
	}
	if lookupErr != nil {
		var dnsErr *net.DNSError
		if errors.As(lookupErr, &dnsErr) && dnsErr.IsNotFound && !dnsErr.IsTemporary && !dnsErr.IsTimeout {
			return notConfigured(res, cfg)
		}
		// Timeouts, SERVFAIL, resolver trouble, cancellation: nothing is
		// concluded about the domain. Never reported as a misconfiguration.
		res.Status, res.Reason = StatusTemporaryError, ReasonDNS
		return res
	}
	if len(records) > MaxTXTRecords {
		res.Status, res.Reason = StatusInvalid, ReasonTooManyTXT
		res.Expected = Expected{Action: "fix_record"}
		return res
	}
	var spfRecords []string
	for _, r := range records {
		if IsSPF(r) {
			spfRecords = append(spfRecords, r)
		}
	}
	switch len(spfRecords) {
	case 0:
		return notConfigured(res, cfg)
	case 1:
	default:
		res.Status, res.Reason = StatusConflict, ReasonMultipleRecords
		res.Expected = Expected{Action: "merge_records"}
		return res
	}
	raw := spfRecords[0]
	rec, err := Parse(raw)
	if err != nil {
		var ie *InvalidError
		res.Status, res.Reason = StatusInvalid, ReasonBadEncoding
		if errors.As(err, &ie) {
			res.Reason = ie.Reason
		}
		res.Expected = Expected{Action: "fix_record"}
		return res
	}
	res.Published = raw
	res.Unevaluated = rec.Unevaluated()
	res.Warnings = warnings(rec, cfg)

	missing, denied := assess(rec, cfg)
	if len(missing) == 0 {
		res.Status = StatusVerified
		res.Expected = Expected{Action: "none", Type: "TXT", Name: domain, Value: raw}
		return res
	}
	res.Status = StatusMismatch
	switch {
	case cfg.Mode == ModeRelay:
		res.Reason = ReasonIncludeMissing
	case denied:
		res.Reason = ReasonExplicitlyDeny
	default:
		res.Reason = ReasonNotListed
	}
	merged, merr := rec.Merge(missing)
	if merr != nil {
		res.Reason = ReasonMergeTooLong
		res.Expected = Expected{Action: "fix_record"}
		return res
	}
	// One record, never two: the existing record is extended, not duplicated.
	res.Expected = Expected{Action: "update_existing", Type: "TXT", Name: domain, Value: merged}
	return res
}

func notConfigured(res Result, cfg Config) Result {
	res.Status = StatusNotConfigured
	res.Expected = createExpected(res.Domain, cfg)
	return res
}

// createExpected is the record for a domain with none. "~all" (softfail) is the
// conservative default: it does not tell receivers to reject other senders the
// tenant may already use. The tenant chooses the final qualifier.
func createExpected(domain string, cfg Config) Expected {
	parts := "v=spf1"
	for _, m := range cfg.Mechanisms() {
		parts += " " + m
	}
	return Expected{Action: "create", Type: "TXT", Name: domain, Value: parts + " ~all"}
}

func unknownExpected(m Mode) Expected {
	if m == ModeRelay {
		return Expected{Action: "follow_relay_provider"}
	}
	return Expected{Action: "declare_sending_ips"}
}

// assess returns the mechanisms MailX needs added, and whether any sending
// address is explicitly non-passing in the existing record.
func assess(rec Record, cfg Config) (missing []string, denied bool) {
	if cfg.Mode == ModeRelay {
		if !rec.Includes(cfg.RelayInclude) {
			missing = cfg.Mechanisms()
		}
		return missing, false
	}
	for _, ip := range cfg.IPs {
		switch rec.Covers(ip) {
		case Authorized:
		case Denied:
			denied = true
			missing = append(missing, mechanism(ip))
		default:
			missing = append(missing, mechanism(ip))
		}
	}
	return missing, denied
}

func mechanism(ip netip.Addr) string {
	if ip.Is4() {
		return "ip4:" + ip.String()
	}
	return "ip6:" + ip.String()
}

func warnings(rec Record, cfg Config) []string {
	var w []string
	if rec.Unevaluated() {
		w = append(w, WarnUnevaluated)
	}
	if rec.PermitsAll() {
		w = append(w, WarnPermitsAll)
	}
	extra := 0
	if cfg.Mode == ModeRelay && !rec.Includes(cfg.RelayInclude) {
		extra = 1 // the include MailX would add costs one more lookup
	}
	if rec.DNSTerms()+extra >= 8 {
		w = append(w, WarnLookupLimit)
	}
	for _, t := range rec.Terms {
		if !t.Modifier && t.Name == "ptr" {
			w = append(w, WarnDeprecatedPTR)
			break
		}
	}
	return w
}
