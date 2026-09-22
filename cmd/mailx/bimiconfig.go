package main

import (
	"context"
	"time"

	"github.com/Ferousco-dev/mailx/internal/bimi"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dmarc"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/webhook"
)

// buildBIMI wires sender-side BIMI readiness. Like buildDMARC, it needs no
// configuration: DNS is the source of truth. DMARC state reaches it as a
// fact through the adapter below, so bimi never imports dmarc's internal
// types (mirrors dmarc's own DKIMState/SPFState seam). The asset fetcher
// always uses the strict default URLPolicy (HTTPS + public addresses
// only, DNS-rebinding-safe) — there is no insecure override, unlike
// webhooks: a BIMI logo/certificate is always meant to be a real public
// asset a mailbox provider itself will fetch, so a private-network
// exception would only mask misconfiguration.
func buildBIMI(db *database.DB, dmarcSvc *dmarc.Service) (*bimi.Service, error) {
	var dm bimi.DMARCState
	if dmarcSvc != nil {
		dm = dmarcState{svc: dmarcSvc}
	}
	fetcher := bimi.NewAssetFetcher(webhook.URLPolicy{}, bimi.MaxLogoBytes, 8*time.Second)
	return bimi.NewService(db, maildomain.NewNetTXTResolver(), dm, fetcher, nil)
}

type dmarcState struct{ svc *dmarc.Service }

// DMARCView asks the existing DMARC service for a full DNS-checked result
// and maps only the two facts BIMI's prerequisite needs — the effective
// policy and the Organizational Domain (for BIMI's own org-domain DNS
// fallback) — never re-deriving DMARC semantics itself.
func (a dmarcState) DMARCView(ctx context.Context, tenantID, domainID string) (bimi.DMARCPrereq, error) {
	res, err := a.svc.Verify(ctx, tenantID, domainID)
	if err != nil {
		return bimi.DMARCPrereq{}, err
	}
	return bimi.DMARCPrereq{
		Checked:         res.DNS.Status == dmarc.StatusMonitoring || res.DNS.Status == dmarc.StatusEnforcing,
		EffectivePolicy: string(res.DNS.Effective),
		OrgDomain:       res.OrgDomain,
	}, nil
}
