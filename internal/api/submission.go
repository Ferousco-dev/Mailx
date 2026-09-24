package api

import (
	"context"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

type SubmissionAcceptor struct{ h *emailHandler }

func NewSubmissionAcceptor(db *database.DB, store *storage.FileStore, dkimSvc *dkim.Service, abuse *AbuseControls, msgDomain string) *SubmissionAcceptor {
	h := newEmailHandler(db, store)
	h.dkim = dkimSvc
	h.abuse = abuse
	if msgDomain != "" {
		h.msgDomain = msgDomain
	}
	return &SubmissionAcceptor{h: h}
}

func (a *SubmissionAcceptor) Accept(ctx context.Context, tenantID, from string, to, cc, bcc []string, replyTo, subject, text, html string) (string, error) {
	req := sendEmailRequest{From: from, To: to, Cc: cc, Bcc: bcc, ReplyTo: replyTo, Subject: subject, Text: text, HTML: html}
	now := a.h.now()
	scheduledAt, verr := req.validate(now, a.h.recipientLimit())
	if verr != nil {
		return "", verr
	}
	resp, _, aerr := a.h.acceptOne(ctx, tenantID, req, "", now, scheduledAt)
	if aerr != nil {
		return "", aerr
	}
	return resp.ID, nil
}
