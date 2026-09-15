package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, output io.Writer) error {
	switch len(args) {
	case 0:
		return serve()
	case 1:
		if args[0] == "list" {
			return listMessages(output)
		}
		if args[0] == "migrate" {
			return migrate()
		}
		if args[0] == "inspect" {
			return fmt.Errorf("usage: mailx inspect <mailx-id>")
		}
		return fmt.Errorf("unknown command %q; usage: mailx [list | migrate | inspect <mailx-id>]", args[0])
	case 2:
		if args[0] == "inspect" {
			return inspectMessage(output, args[1])
		}
	}
	return fmt.Errorf("usage: mailx [list | migrate | inspect <mailx-id>]")
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

func serve() error {
	store, e := storage.NewFileStore(storageRoot())
	if e != nil {
		return e
	}
	addr := smtpAddr()
	l, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	log.Printf("MailX SMTP server listening on %s", addr)
	server, e := smtp.NewServer(smtp.DefaultConfig(), func(s smtp.Session, m mail.Message) error {
		record, e := storage.NewMessageRecord(s.Envelope, m)
		if e != nil {
			return e
		}
		if e := store.Save(record); e != nil {
			return e
		}
		log.Printf("\n========== EMAIL RECEIVED ==========\nSMTP ENVELOPE\nMAIL FROM: %s\nRCPT TO: %v\nMESSAGE\nFrom: %s\nTo: %v\nCc: %v\nSubject: %s\nDate: %s\nMessage-ID: %s\nBODY\n%s\n====================================", s.Envelope.MailFrom, s.Envelope.Recipients, m.From, m.To, m.Cc, m.Subject, m.Date, m.MessageID, m.Body)
		return nil
	})
	if e != nil {
		l.Close()
		return e
	}

	// SIGTERM is what `docker compose stop`/`down` send; closing the
	// listener makes Serve return instead of the process being SIGKILLed.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-sigCtx.Done()
		log.Println("MailX SMTP server shutting down")
		l.Close()
	}()

	if err := server.Serve(l); err != nil && sigCtx.Err() == nil {
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
	log.Println("MailX: migrations applied")
	return nil
}
