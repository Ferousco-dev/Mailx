package main

import (
	"context"
	"os"

	"github.com/Ferousco-dev/mailx/internal/api"
)

// systemMailer adapts api.SubmissionAcceptor to humanauth.Mailer, letting
// MailX send its own system emails (password reset, etc.) through its own
// outbound pipeline instead of a separate ad hoc path.
type systemMailer struct {
	acceptor *api.SubmissionAcceptor
	tenantID string
	fromAddr string
}

func (m *systemMailer) SendSystemEmail(ctx context.Context, to, subject, text, html string) error {
	_, err := m.acceptor.Accept(ctx, m.tenantID, m.fromAddr, []string{to}, nil, nil, "", subject, text, html)
	return err
}

// buildSystemMailer wires up the system mailer from MAILX_SYSTEM_TENANT_ID
// and MAILX_SYSTEM_FROM_ADDRESS. Both must be set (the from address must
// belong to a domain verified for that tenant); otherwise nil is returned
// and callers must degrade gracefully rather than fail startup, since
// system email is optional for a self-hosted instance.
func buildSystemMailer(acceptor *api.SubmissionAcceptor) *systemMailer {
	tenantID := os.Getenv("MAILX_SYSTEM_TENANT_ID")
	fromAddr := os.Getenv("MAILX_SYSTEM_FROM_ADDRESS")
	if tenantID == "" || fromAddr == "" {
		return nil
	}
	return &systemMailer{acceptor: acceptor, tenantID: tenantID, fromAddr: fromAddr}
}

func dashboardBaseURL() string {
	return os.Getenv("MAILX_DASHBOARD_BASE_URL")
}
