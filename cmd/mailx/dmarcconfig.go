package main

import (
	"context"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/spf"
)

// buildDMARC wires sender-side DMARC readiness. It needs no configuration: DNS is
// the source of truth and the recommendation is fixed. DKIM and SPF state reach
// it as facts through the adapters below, so dmarc never imports either package.
func buildDMARC(db *database.DB, dkimSvc *dkim.Service, spfSvc *spf.Service, o obs) (*dmarc.Service, error) {
	var observer dmarc.Observer
	if o.metrics != nil {
		observer = o.metrics
	}
	var sp dmarc.SPFState
	if spfSvc != nil {
		sp = spfState{svc: spfSvc}
	}
	var dk dmarc.DKIMState
	if dkimSvc != nil {
		dk = dkimState{svc: dkimSvc}
	}
	return dmarc.NewService(db, maildomain.NewNetTXTResolver(), dk, sp, observer)
}

type dkimState struct{ svc *dkim.Service }

// ActiveSigningDomain reports the domain MailX's ACTIVE key signs as (d= is the
// From domain by construction). Pending and retired keys do not sign.
func (a dkimState) ActiveSigningDomain(ctx context.Context, tenantID, domainID string) (string, bool, error) {
	dom, keys, err := a.svc.Keys(ctx, tenantID, domainID)
	if err != nil {
		return "", false, err
	}
	for _, k := range keys {
		if k.Status == database.DKIMActive {
			return dom.Name, true, nil
		}
	}
	return dom.Name, false, nil
}

type spfState struct{ svc *spf.Service }

// SPFView returns the SPF routing mode and, when check is set, one bounded SPF
// verification status. Errors other than "domain not verified" degrade to an
// unknown SPF path in the caller.
func (a spfState) SPFView(ctx context.Context, tenantID, domainID string, check bool) (dmarc.SPFView, error) {
	view := dmarc.SPFView{Mode: string(a.svc.Config().Mode)}
	if !check {
		return view, nil
	}
	res, err := a.svc.Verify(ctx, tenantID, domainID)
	if err != nil {
		return view, err
	}
	view.Status = string(res.Status)
	return view, nil
}
