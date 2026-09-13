// Package mail contains MailX's transport-independent email domain types.
package mail

// Envelope contains SMTP transport information. It is intentionally separate
// from the message headers because delivery uses envelope recipients.
type Envelope struct {
	MailFrom   string
	Recipients []string
}

// Attachment is a MIME part explicitly marked for attachment presentation.
// Content is decoded but remains in memory; Filename is untrusted metadata.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// Message contains the email content transmitted during SMTP DATA. Its fields
// are separate from Envelope because message headers do not control delivery.
type Message struct {
	From                    string
	To                      []string
	Cc                      []string
	Bcc                     []string
	Subject                 string
	Date                    string
	MessageID               string
	MIMEVersion             string
	ContentType             string
	ContentTransferEncoding string
	ContentDisposition      string
	MediaType               string
	Charset                 string
	Body                    string
	TextBody                string
	HTMLBody                string
	Attachments             []Attachment
	Raw                     string
}
