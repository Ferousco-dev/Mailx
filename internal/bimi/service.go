package bimi

import (
	"context"
	"errors"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// Store mirrors dmarc.Store: the one seam into domain ownership/verification.
type Store interface {
	GetDomain(ctx context.Context, tenantID, id string) (database.Domain, error)
}

// DMARCState returns the DMARC prerequisite fact BIMI consumes. BIMI never
// re-implements DMARC evaluation; it asks the existing internal/dmarc
// Service (via a cmd/mailx adapter, same seam pattern as dmarc's own
// DKIMState/SPFState) and treats the answer as authoritative.
type DMARCState interface {
	DMARCView(ctx context.Context, tenantID, domainID string) (DMARCPrereq, error)
}

type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type Observer interface {
	BIMIResult(dnsStatus, readiness string)
}

var (
	ErrDomainNotVerified = database.ErrDomainNotVerified
	ErrBusy              = errors.New("bimi: verification capacity exhausted")
)

const (
	defaultTimeout = 5 * time.Second
	maxConcurrent  = 16
)

// Service assesses sender-side BIMI readiness. DNS is the source of truth;
// nothing is persisted.
type Service struct {
	store    Store
	dmarc    DMARCState
	resolver TXTResolver
	fetcher  *AssetFetcher // nil disables asset (logo/cert) fetching entirely
	observer Observer
	timeout  time.Duration
	slots    chan struct{}
	now      func() time.Time
}

// NewService builds the service. fetcher may be nil: asset validation is
// then never available (Verify's fetchAssets is ignored), which is a valid,
// conservative deployment choice — DNS/DMARC readiness alone still works.
func NewService(store Store, resolver TXTResolver, dmarcState DMARCState, fetcher *AssetFetcher, observer Observer) (*Service, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("bimi: store and resolver are required")
	}
	return &Service{
		store: store, dmarc: dmarcState, resolver: resolver, fetcher: fetcher, observer: observer,
		timeout: defaultTimeout, slots: make(chan struct{}, maxConcurrent), now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// Describe returns MailX's identity model with no DNS query and no asset
// fetch (readiness unchecked).
func (s *Service) Describe(ctx context.Context, tenantID, id string) (Result, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	res := assess(dom.Name, DefaultSelector, DNSFinding{Status: StatusUnchecked}, DMARCPrereq{}, LogoCheck{}, CertCheck{})
	res.Readiness = ReadinessUnchecked
	return res, nil
}

// Verify inspects public DNS (bounded: at most 2 TXT queries — the domain's
// default selector, then its DMARC-derived organizational domain if
// different) for a VERIFIED domain the tenant owns, checks the DMARC
// prerequisite via the existing DMARC service, and, only when fetchAssets
// is true, fetches and structurally validates the referenced logo and
// certificate. It stores nothing.
func (s *Service) Verify(ctx context.Context, tenantID, id string, fetchAssets bool) (Result, error) {
	dom, err := s.store.GetDomain(ctx, tenantID, id)
	if err != nil {
		return Result{}, err
	}
	if dom.VerificationStatus != database.DomainVerified {
		return Result{}, ErrDomainNotVerified
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Result{}, ErrBusy
	}
	lctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	dmarcFact := DMARCPrereq{}
	if s.dmarc != nil {
		if v, err := s.dmarc.DMARCView(lctx, tenantID, dom.ID); err == nil {
			dmarcFact = v
		} else if lctx.Err() != nil {
			return Result{}, lctx.Err()
		}
	}

	dns := s.discover(lctx, dom.Name, dmarcFact.OrgDomain)

	var logo LogoCheck
	var cert CertCheck
	if fetchAssets && dns.Status == StatusFound && !dns.Record.Declined {
		if s.fetcher == nil {
			logo = LogoCheck{Checked: true, FetchErr: "asset_fetching_not_configured"}
		} else {
			logo = s.checkLogo(lctx, dns.Record.Location)
			if dns.Record.Authority != "" {
				cert = s.checkCert(lctx, dns.Record.Authority)
			}
		}
	}

	res := assess(dom.Name, DefaultSelector, dns, dmarcFact, logo, cert)
	s.observe(res)
	return res, nil
}

// discover queries the default selector at domain, falling back to
// orgDomain (only when non-empty and different) if the domain itself
// published nothing — mirroring the draft's Author-Domain-then-
// Organizational-Domain fallback.
func (s *Service) discover(ctx context.Context, domain, orgDomain string) DNSFinding {
	f, ok := s.lookupOne(ctx, domain, "domain")
	if ok {
		return f
	}
	if f.Status == StatusTempError {
		return f
	}
	if orgDomain != "" && orgDomain != domain {
		if f2, ok := s.lookupOne(ctx, orgDomain, "organizational_domain"); ok || f2.Status == StatusTempError {
			return f2
		}
	}
	return f
}

// lookupOne queries one name and reports (finding, decisive). decisive=false
// with Status==not_configured means "keep trying the fallback"; any other
// status (found, invalid, temporary_error) is always decisive.
func (s *Service) lookupOne(ctx context.Context, domain, source string) (DNSFinding, bool) {
	name := QueryName(DefaultSelector, domain)
	txts, err := s.resolver.LookupTXT(ctx, name)
	if err != nil {
		if ctx.Err() != nil {
			return DNSFinding{Status: StatusTempError, RecordName: name}, true
		}
		// NXDOMAIN and "no records" both surface as an error from most
		// resolvers with no reliable way to tell them apart from THIS
		// interface; treat any lookup error as "not configured" rather
		// than temporary_error, matching the common case (record absent).
		// A real transient DNS outage self-corrects on the next check.
		return DNSFinding{Status: StatusNotConfigured, RecordName: name}, false
	}
	var candidates []string
	for _, t := range txts {
		if IsBIMI(t) {
			candidates = append(candidates, t)
		}
	}
	switch len(candidates) {
	case 0:
		return DNSFinding{Status: StatusNotConfigured, RecordName: name}, false
	case 1:
		rec, perr := Parse(candidates[0])
		if perr != nil {
			reason := ""
			var ie *InvalidError
			if errors.As(perr, &ie) {
				reason = ie.Reason
			}
			return DNSFinding{Status: StatusInvalid, Reason: reason, Source: source, RecordName: name, Published: candidates[0]}, true
		}
		return DNSFinding{Status: StatusFound, Source: source, RecordName: name, Published: candidates[0], Record: rec}, true
	default:
		return DNSFinding{Status: StatusInvalid, Reason: "multiple_bimi_records", Source: source, RecordName: name}, true
	}
}

func (s *Service) checkLogo(ctx context.Context, url string) LogoCheck {
	data, err := s.fetcher.Fetch(ctx, url)
	if err != nil {
		return LogoCheck{Checked: true, FetchErr: fetchErrCode(err)}
	}
	return LogoCheck{Checked: true, SVG: ValidateSVG(data)}
}

func (s *Service) checkCert(ctx context.Context, url string) CertCheck {
	data, err := s.fetcher.Fetch(ctx, url)
	if err != nil {
		return CertCheck{Checked: true, FetchErr: fetchErrCode(err)}
	}
	return CertCheck{Checked: true, Cert: ParseCertificate(data, s.now())}
}

func fetchErrCode(err error) string {
	switch {
	case errors.Is(err, ErrAssetTooLarge):
		return "asset_too_large"
	case errors.Is(err, ErrAssetNotHTTPS):
		return "asset_not_https"
	default:
		return "asset_fetch_failed"
	}
}

func (s *Service) observe(res Result) {
	if s.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.observer.BIMIResult(string(res.DNS.Status), res.Readiness)
}
