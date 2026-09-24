package main

import (
	"context"
	"crypto/tls"
	"os"

	"github.com/Ferousco-dev/mailx/internal/api"
	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func submissionAddr() string {
	return os.Getenv("MAILX_SUBMISSION_ADDR")
}

// submissionEnvelope picks the SMTP envelope (MAIL FROM / RCPT TO) over the
// parsed message headers as the actual return path and recipient list a
// normal SMTP client negotiated - a Bcc recipient in particular has no
// header representation at all, and the header From can legitimately
// differ from the envelope sender. Falls back to the header values only if
// the envelope is somehow empty (should not happen once DATA is reached).
func submissionEnvelope(s smtp.Session, m mail.Message) (from string, to []string) {
	from = s.Envelope.MailFrom
	if from == "" {
		from = m.From
	}
	to = s.Envelope.Recipients
	if len(to) == 0 {
		to = m.To
	}
	return from, to
}

func runSubmissionReceiver(ctx context.Context, db *database.DB, store *storage.FileStore, dkimSvc *dkim.Service, abuse *api.AbuseControls, authSvc *auth.Service, msgDomain string, o obs) error {
	addr := submissionAddr()
	certFile := os.Getenv("MAILX_SUBMISSION_TLS_CERT")
	keyFile := os.Getenv("MAILX_SUBMISSION_TLS_KEY")
	if addr == "" || certFile == "" || keyFile == "" {
		if addr != "" {
			o.log.Warn("submission_disabled", "detail", "MAILX_SUBMISSION_TLS_CERT/MAILX_SUBMISSION_TLS_KEY not set")
		}
		<-ctx.Done()
		return nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	l, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	o.log.Info("submission_listening", "addr", addr)

	acceptor := api.NewSubmissionAcceptor(db, store, dkimSvc, abuse, msgDomain)
	cfg := o.smtpConfig()
	cfg.RequireAuth = true
	cfg.Authenticator = func(_, pass string) (string, bool) {
		key, err := authSvc.Authenticate(context.Background(), pass)
		if err != nil {
			return "", false
		}
		for _, sc := range key.Scopes {
			if sc == string(auth.ScopeEmailsSend) {
				return key.TenantID, true
			}
		}
		return "", false
	}
	sink := func(s smtp.Session, m mail.Message) error {
		from, to := submissionEnvelope(s, m)
		_, err := acceptor.Accept(context.Background(), s.TenantID, from, to, nil, nil, "", m.Subject, m.TextBody, m.HTMLBody)
		return err
	}
	server, err := smtp.NewServer(cfg, sink)
	if err != nil {
		l.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	if err := server.Serve(l); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
