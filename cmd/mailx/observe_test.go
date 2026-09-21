package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func TestNewObsRejectsInvalidLoggingConfiguration(t *testing.T) {
	t.Setenv("MAILX_LOG_LEVEL", "loud")
	if _, err := newObs(); err == nil {
		t.Fatal("invalid MAILX_LOG_LEVEL must fail startup")
	}
	t.Setenv("MAILX_LOG_LEVEL", "info")
	t.Setenv("MAILX_LOG_FORMAT", "yaml")
	if _, err := newObs(); err == nil {
		t.Fatal("invalid MAILX_LOG_FORMAT must fail startup")
	}
	t.Setenv("MAILX_LOG_FORMAT", "text")
	if o, err := newObs(); err != nil || o.log == nil || o.metrics == nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestObservabilityAddrDefaultOverrideAndDisable(t *testing.T) {
	if got := observabilityAddr(); got != ":9090" {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("MAILX_OBSERVABILITY_ADDR", "127.0.0.1:9191")
	if got := observabilityAddr(); got != "127.0.0.1:9191" {
		t.Fatalf("override = %q", got)
	}
	t.Setenv("MAILX_OBSERVABILITY_ADDR", "")
	if got := observabilityAddr(); got != "" {
		t.Fatalf("explicit empty must disable, got %q", got)
	}
}

func testObs(t *testing.T) (obs, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, _ := observability.NewLogger(&buf, "debug", "json")
	return obs{log: logger}, &buf
}

func TestSMTPSinkLogsIDsOnlyNotEnvelopeOrContent(t *testing.T) {
	o, buf := testObs(t)
	store, err := storage.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := o.smtpSink(store)
	env := mail.Envelope{MailFrom: "<PRIVATE-FROM@example.com>", Recipients: []string{"<PRIVATE-TO@example.com>"}}
	msg, err := mail.ParseMessage("From: PRIVATE-FROM@example.com\r\nTo: PRIVATE-TO@example.com\r\nSubject: PRIVATE-SUBJECT\r\n\r\nPRIVATE-BODY\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink(smtp.Session{ID: "sess_test", Envelope: env}, msg); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"session_id":"sess_test"`) || !strings.Contains(out, `"msg":"smtp_message_stored"`) || !strings.Contains(out, `"message_id"`) {
		t.Fatalf("log = %s", out)
	}
	for _, marker := range []string{"PRIVATE-FROM", "PRIVATE-TO", "PRIVATE-SUBJECT", "PRIVATE-BODY"} {
		if strings.Contains(out, marker) {
			t.Fatalf("SMTP log leaked %s: %s", marker, out)
		}
	}
}

func TestSMTPSinkStorageFailureIsLoggedAndReturned(t *testing.T) {
	o, buf := testObs(t)
	dir := t.TempDir()
	store, err := storage.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	messages := filepath.Join(dir, "messages")
	if err := os.Chmod(messages, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(messages, 0o700) })
	msg, _ := mail.ParseMessage("Subject: x\r\n\r\nPRIVATE-BODY\r\n")
	env := mail.Envelope{MailFrom: "<a@example.com>", Recipients: []string{"<b@example.com>"}}
	if err := o.smtpSink(store)(smtp.Session{ID: "sess_fail", Envelope: env}, msg); err == nil {
		t.Skip("filesystem permits writes despite read-only directory (running as root?)")
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"smtp_storage_failed"`) || !strings.Contains(out, "sess_fail") || strings.Contains(out, "PRIVATE-BODY") || strings.Contains(out, dir) {
		t.Fatalf("log = %s", out)
	}
}

func TestErrLoggerAndComponentLoggingDoNotChangeResults(t *testing.T) {
	o, buf := testObs(t)
	o.errLogger("worker")(errors.New("db unavailable"))
	if !strings.Contains(buf.String(), `"component":"worker"`) {
		t.Fatalf("log = %s", buf.String())
	}
	c := o.logged("api", func(context.Context) error { return errors.New("bind failed") })
	if err := c.run(nil); err == nil || err.Error() != "bind failed" {
		t.Fatalf("component result changed: %v", err)
	}
}
