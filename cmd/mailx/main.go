package main

import (
	"log"
	"net"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
)

func main() {
	l, e := net.Listen("tcp", ":2525")
	if e != nil {
		log.Fatal(e)
	}
	defer l.Close()
	log.Println("MailX SMTP server listening on localhost:2525")
	for {
		c, e := l.Accept()
		if e != nil {
			log.Println(e)
			continue
		}
		go smtp.HandleConnection(c, func(s smtp.Session, m mail.Message) {
			log.Printf("\n========== EMAIL RECEIVED ==========\nSMTP ENVELOPE\nMAIL FROM: %s\nRCPT TO: %v\nMESSAGE\nFrom: %s\nTo: %v\nCc: %v\nSubject: %s\nDate: %s\nMessage-ID: %s\nBODY\n%s\n====================================", s.Envelope.MailFrom, s.Envelope.Recipients, m.From, m.To, m.Cc, m.Subject, m.Date, m.MessageID, m.Body)
		})
	}
}
