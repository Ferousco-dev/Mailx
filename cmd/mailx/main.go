package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

const usage = "usage: mailx [list | migrate | inspect <mailx-id> | " +
	"create-tenant | create-api-key | rotate-api-key | revoke-api-key | list-api-keys | check-smtp-identity]"

func run(args []string, output io.Writer) error {
	if len(args) == 0 {
		return runFull()
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf(usage)
		}
		return listMessages(output)
	case "migrate":
		if len(args) != 1 {
			return fmt.Errorf(usage)
		}
		return migrate()
	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("usage: mailx inspect <mailx-id>")
		}
		return inspectMessage(output, args[1])
	// Bootstrap/admin commands for v0.19 API-key authentication — see
	// apikeys.go's doc for why these are CLI-only, never a public
	// unauthenticated REST endpoint.
	case "create-tenant":
		return cmdCreateTenant(args[1:], output)
	case "create-api-key":
		return cmdCreateAPIKey(args[1:], output)
	case "rotate-api-key":
		return cmdRotateAPIKey(args[1:], output)
	case "revoke-api-key":
		return cmdRevokeAPIKey(args[1:], output)
	case "list-api-keys":
		return cmdListAPIKeys(args[1:], output)
	case "check-smtp-identity":
		return cmdCheckSMTPIdentity(args[1:], output)
	}
	return fmt.Errorf("unknown command %q; %s", args[0], usage)
}

// storageRoot honors MAILX_STORAGE_ROOT so containerized deployments can
// point FileStore at a mounted volume; empty falls through to
// storage.DefaultRoot.
func storageRoot() string {
	return os.Getenv("MAILX_STORAGE_ROOT")
}

func listMessages(output io.Writer) error {
	store, e := storage.NewFileStore(storageRoot())
	if e != nil {
		return e
	}
	messages, e := store.List()
	if e != nil {
		return e
	}
	fmt.Fprintln(output, "ID\tRECEIVED\tFROM\tSUBJECT")
	for _, message := range messages {
		fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", message.ID, message.ReceivedAt.Format("2006-01-02T15:04:05Z07:00"), message.Message.From, message.Message.Subject)
	}
	return nil
}

func inspectMessage(output io.Writer, id string) error {
	store, err := storage.NewFileStore(storageRoot())
	if err != nil {
		return err
	}
	message, err := store.Load(id)
	if err != nil {
		return err
	}
	metadata := message.Metadata
	fmt.Fprintf(output, "MailX ID: %s\nReceivedAt: %s\nMAIL FROM: %s\nRCPT TO: %s\nFrom: %s\nTo: %s\n", metadata.ID, metadata.ReceivedAt.Format("2006-01-02T15:04:05Z07:00"), metadata.Envelope.MailFrom, strings.Join(metadata.Envelope.RcptTo, ", "), metadata.Message.From, strings.Join(metadata.Message.To, ", "))
	if len(metadata.Message.Cc) > 0 {
		fmt.Fprintf(output, "Cc: %s\n", strings.Join(metadata.Message.Cc, ", "))
	}
	if len(metadata.Message.Bcc) > 0 {
		fmt.Fprintf(output, "Bcc: %s\n", strings.Join(metadata.Message.Bcc, ", "))
	}
	fmt.Fprintf(output, "Subject: %s\nDate: %s\nMessage-ID: %s\nMIME Type: %s\nCharset: %s\nAttachments: %d\n", metadata.Message.Subject, metadata.Message.Date, metadata.Message.MessageID, metadata.MIME.MediaType, metadata.MIME.Charset, len(metadata.Attachments))
	for _, attachment := range metadata.Attachments {
		fmt.Fprintf(output, "- %s | %s | %d bytes | %s\n", attachment.Filename, attachment.ContentType, attachment.Size, attachment.StoredName)
	}
	return nil
}

// smtpAddr honors MAILX_SMTP_ADDR (e.g. ":2525" or "0.0.0.0:2525") so a
// container can bind the same port developers already use locally.
func smtpAddr() string {
	if addr := os.Getenv("MAILX_SMTP_ADDR"); addr != "" {
		return addr
	}
	return ":2525"
}

// notifyShutdown returns a context canceled on SIGTERM/SIGINT — what
// `docker compose stop`/`down` and Ctrl-C send — so every long-running
// component (SMTP listener, worker pool, dispatcher, API server) shuts
// down from ONE signal source instead of each installing its own handler.
func notifyShutdown() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}

// serve is the original v0.1-v0.14 SMTP-only entry point, used when
// DATABASE_URL is not configured (runFull falls back to this).
func serve() error {
	o, err := newObs()
	if err != nil {
		return err
	}
	store, err := storage.NewFileStore(storageRoot())
	if err != nil {
		return err
	}
	slog.SetDefault(o.log)
	ctx, stop := notifyShutdown()
	defer stop()
	o.log.Info("mailx_start", "mode", "smtp-only", "smtp_addr", smtpAddr(), "observability_addr", observabilityAddr())
	defer o.log.Info("mailx_stop")
	components := []component{
		o.logged("smtp", func(ctx context.Context) error { return runSMTPReceiver(ctx, store, o) }),
	}
	// SMTP-only mode has no PostgreSQL or Redis, so readiness has nothing to
	// check; the operator listener still serves metrics and liveness.
	if addr := observabilityAddr(); addr != "" {
		op := observability.NewServer(addr, observability.OperatorMux(o.metrics, observability.NewReadiness(nil)))
		components = append(components, o.logged("observability", op.Run))
	}
	return runComponents(ctx, components...)
}

// runSMTPReceiver blocks until ctx is canceled, at which point it closes
// its listener so Serve returns instead of relying on SIGKILL.
func runSMTPReceiver(ctx context.Context, store *storage.FileStore, o obs) error {
	addr := smtpAddr()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	o.log.Info("smtp_listening", "addr", addr)
	server, err := smtp.NewServer(o.smtpConfig(), o.smtpSink(store))
	if err != nil {
		l.Close()
		return err
	}

	go func() {
		<-ctx.Done()
		o.log.Info("smtp_shutting_down")
		l.Close()
	}()

	if err := server.Serve(l); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// migrate applies every pending PostgreSQL migration and exits; it is run
// as a one-shot Compose step before the SMTP server starts, so schema
// setup is deterministic and visible rather than racing app startup.
func migrate() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("migrate: DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Open(ctx, database.Config{DSN: dsn})
	if err != nil {
		return fmt.Errorf("migrate: connect: %w", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	fmt.Println("MailX: migrations applied")
	return nil
}
