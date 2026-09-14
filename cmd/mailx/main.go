package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

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
		if args[0] == "inspect" {
			return fmt.Errorf("usage: mailx inspect <mailx-id>")
		}
		return fmt.Errorf("unknown command %q; usage: mailx [list | inspect <mailx-id>]", args[0])
	case 2:
		if args[0] == "inspect" {
			return inspectMessage(output, args[1])
		}
	}
	return fmt.Errorf("usage: mailx [list | inspect <mailx-id>]")
}

func listMessages(output io.Writer) error {
	store, e := storage.NewFileStore("")
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
	store, err := storage.NewFileStore("")
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

func serve() error {
	store, e := storage.NewFileStore("")
	if e != nil {
		return e
	}
	l, e := net.Listen("tcp", ":2525")
	if e != nil {
		return e
	}
	defer l.Close()
	log.Println("MailX SMTP server listening on localhost:2525")
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
		return e
	}
	return server.Serve(l)
}
