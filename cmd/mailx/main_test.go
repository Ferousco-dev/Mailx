package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func TestRunMigrateRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if err := run([]string{"migrate"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error when DATABASE_URL is unset")
	}
}

func TestRunComponentsFailureCancelsPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peerStarted := make(chan struct{})
	peerStopped := make(chan struct{})
	listenerErr := errors.New("bind failed")
	err := runComponents(ctx,
		component{"smtp", func(context.Context) error {
			<-peerStarted
			return listenerErr
		}},
		component{"worker", func(ctx context.Context) error {
			close(peerStarted)
			<-ctx.Done()
			close(peerStopped)
			return nil
		}},
	)
	if !errors.Is(err, listenerErr) || !strings.Contains(err.Error(), "smtp") {
		t.Fatalf("listener error not returned: %v", err)
	}
	select {
	case <-peerStopped:
	default:
		t.Fatal("peer was not stopped before return")
	}
}

func TestRunComponentsUnexpectedExitAndGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runComponents(ctx, component{"api", func(context.Context) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "api: exited unexpectedly") {
		t.Fatalf("unexpected exit not reported: %v", err)
	}

	shutdownCtx, shutdown := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runComponents(shutdownCtx, component{"worker", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		}})
	}()
	<-started
	shutdown()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful shutdown hung")
	}
}

func TestRunListAndInspect(t *testing.T) {
	t.Chdir(t.TempDir())
	message, err := mail.ParseMessage("From: Sender <sender@example.com>\r\nTo: Recipient <recipient@example.com>\r\nSubject: persisted subject\r\nMessage-ID: <sender-id@example.com>\r\nContent-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\n\r\nbody marker\r\n--mailx\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nUERG\r\n--mailx--\r\n")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	record, err := storage.NewMessageRecord(mail.Envelope{MailFrom: "<bounce@example.com>", Recipients: []string{"<recipient@example.com>"}}, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	var listed bytes.Buffer
	if err := run([]string{"list"}, &listed); err != nil {
		t.Fatal(err)
	}
	if output := listed.String(); !strings.Contains(output, "ID\tRECEIVED") || !strings.Contains(output, record.ID) || !strings.Contains(output, "persisted subject") || strings.Contains(output, "body marker") {
		t.Fatalf("unexpected list output: %q", output)
	}

	var inspected bytes.Buffer
	if err := run([]string{"inspect", record.ID}, &inspected); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{record.ID, "MAIL FROM: <bounce@example.com>", "Subject: persisted subject", "MIME Type: multipart/mixed", "Attachments: 1", "report.pdf | application/pdf | 3 bytes | 0001.bin"} {
		if !strings.Contains(inspected.String(), expected) {
			t.Fatalf("inspect output missing %q: %q", expected, inspected.String())
		}
	}
	if strings.Contains(inspected.String(), "body marker") {
		t.Fatalf("inspect output exposed raw body: %q", inspected.String())
	}
}

func TestRunReportsCommandErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"inspect"}, {"inspect", "invalid-id"}, {"unknown"}, {"list", "extra"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("run(%q) returned no error", args)
		}
	}
}
