package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"strings"
)

const (
	defaultMediaType = "text/plain"
	defaultCharset   = "us-ascii"
	maxMIMEDepth     = 16
)

// ParseMessage preserves raw email content and extracts MailX's basic message
// fields. It preserves the original message while interpreting supported MIME
// body content.
func ParseMessage(raw string) (Message, error) {
	headers, body, err := splitHeaderAndBody(raw)
	if err != nil {
		return Message{}, err
	}

	values, err := parseHeaders(headers)
	if err != nil {
		return Message{}, err
	}

	to, err := parseAddressList(values["to"])
	if err != nil {
		return Message{}, fmt.Errorf("parse To header: %w", err)
	}
	cc, err := parseAddressList(values["cc"])
	if err != nil {
		return Message{}, fmt.Errorf("parse Cc header: %w", err)
	}
	bcc, err := parseAddressList(values["bcc"])
	if err != nil {
		return Message{}, fmt.Errorf("parse Bcc header: %w", err)
	}

	message := Message{
		From:                    first(values["from"]),
		To:                      to,
		Cc:                      cc,
		Bcc:                     bcc,
		Subject:                 first(values["subject"]),
		Date:                    first(values["date"]),
		MessageID:               first(values["message-id"]),
		MIMEVersion:             first(values["mime-version"]),
		ContentType:             first(values["content-type"]),
		ContentTransferEncoding: first(values["content-transfer-encoding"]),
		ContentDisposition:      first(values["content-disposition"]),
		Body:                    body,
		Raw:                     raw,
	}
	if err := interpretBody(&message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func interpretBody(message *Message) error {
	mediaType, parameters, err := mime.ParseMediaType(message.ContentType)
	if message.ContentType == "" || err != nil {
		message.MediaType = defaultMediaType
		message.Charset = defaultCharset
		return interpretTopLevelBody(message, defaultMediaType, map[string]string{"charset": defaultCharset})
	}

	message.MediaType = mediaType
	message.Charset = parameters["charset"]
	return interpretTopLevelBody(message, mediaType, parameters)
}

func interpretTopLevelBody(message *Message, mediaType string, parameters map[string]string) error {
	disposition, dispositionParameters, err := parseDisposition(message.ContentDisposition)
	if err != nil {
		return err
	}
	if disposition == "attachment" {
		return addAttachment(message, []byte(message.Body), mediaType, parameters, dispositionParameters, message.ContentTransferEncoding)
	}

	switch mediaType {
	case "text/plain":
		return setTextBody(message, []byte(message.Body), message.ContentTransferEncoding)
	case "text/html":
		return setHTMLBody(message, []byte(message.Body), message.ContentTransferEncoding)
	case "multipart/alternative":
		if err := requireIdentityTransferEncoding(message.ContentTransferEncoding); err != nil {
			return err
		}
		return parseMultipartAlternative(message, parameters["boundary"])
	case "multipart/mixed":
		if err := requireIdentityTransferEncoding(message.ContentTransferEncoding); err != nil {
			return err
		}
		return parseMultipartMixed(message, parameters["boundary"])
	}
	return nil
}

func parseMultipartAlternative(message *Message, boundary string) error {
	return parseMultipart(message, message.Body, "multipart/alternative", boundary, 1)
}

func parseMultipartMixed(message *Message, boundary string) error {
	return parseMultipart(message, message.Body, "multipart/mixed", boundary, 1)
}

func parseMultipart(message *Message, body, mediaType, boundary string, depth int) error {
	if depth > maxMIMEDepth {
		return fmt.Errorf("MIME nesting exceeds maximum depth of %d", maxMIMEDepth)
	}
	return walkMultipart(body, boundary, mediaType, func(part *multipart.Part, content []byte) error {
		return interpretPart(message, part, content, depth)
	})
}

func interpretPart(message *Message, part *multipart.Part, content []byte, depth int) error {
	disposition, dispositionParameters, err := parseDisposition(part.Header.Get("Content-Disposition"))
	if err != nil {
		return err
	}
	mediaType, parameters := partMediaType(part)
	if disposition == "attachment" {
		return addAttachment(message, content, mediaType, parameters, dispositionParameters, part.Header.Get("Content-Transfer-Encoding"))
	}

	switch mediaType {
	case "text/plain":
		return setTextBody(message, content, part.Header.Get("Content-Transfer-Encoding"))
	case "text/html":
		return setHTMLBody(message, content, part.Header.Get("Content-Transfer-Encoding"))
	case "multipart/alternative", "multipart/mixed":
		if err := requireIdentityTransferEncoding(part.Header.Get("Content-Transfer-Encoding")); err != nil {
			return err
		}
		return parseMultipart(message, string(content), mediaType, parameters["boundary"], depth+1)
	}
	return nil
}

func walkMultipart(body, boundary, mediaType string, visit func(*multipart.Part, []byte) error) error {
	if boundary == "" {
		return fmt.Errorf("%s is missing a boundary", mediaType)
	}

	reader := multipart.NewReader(bytes.NewReader([]byte(body)), boundary)
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s part: %w", mediaType, err)
		}
		content, err := io.ReadAll(part)
		if err != nil {
			return fmt.Errorf("read %s content: %w", mediaType, err)
		}
		if err := visit(part, content); err != nil {
			return err
		}
	}
}

func partMediaType(part *multipart.Part) (string, map[string]string) {
	mediaType, parameters, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
	if part.Header.Get("Content-Type") == "" || err != nil {
		return defaultMediaType, map[string]string{"charset": defaultCharset}
	}
	return mediaType, parameters
}

func parseDisposition(value string) (string, map[string]string, error) {
	if value == "" {
		return "", nil, nil
	}
	disposition, parameters, err := mime.ParseMediaType(value)
	if err != nil {
		return "", nil, fmt.Errorf("parse Content-Disposition: %w", err)
	}
	if disposition != "inline" && disposition != "attachment" {
		return "attachment", parameters, nil
	}
	return disposition, parameters, nil
}

func addAttachment(message *Message, content []byte, contentType string, contentParameters, dispositionParameters map[string]string, transferEncoding string) error {
	decoded, err := decodeTransferEncoding(content, transferEncoding)
	if err != nil {
		return err
	}

	filename := dispositionParameters["filename"]
	if filename == "" {
		filename = contentParameters["name"]
	}
	message.Attachments = append(message.Attachments, Attachment{
		Filename:    filename,
		ContentType: contentType,
		Content:     decoded,
	})
	return nil
}

func setTextBody(message *Message, content []byte, transferEncoding string) error {
	decoded, err := decodeTransferEncoding(content, transferEncoding)
	if err != nil {
		return err
	}
	message.TextBody = string(decoded)
	return nil
}

func setHTMLBody(message *Message, content []byte, transferEncoding string) error {
	decoded, err := decodeTransferEncoding(content, transferEncoding)
	if err != nil {
		return err
	}
	message.HTMLBody = string(decoded)
	return nil
}

func decodeTransferEncoding(content []byte, transferEncoding string) ([]byte, error) {
	switch normalizeTransferEncoding(transferEncoding) {
	case "", "7bit", "8bit", "binary":
		return content, nil
	case "base64":
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(content)))
		if err != nil {
			return nil, fmt.Errorf("decode base64 body: %w", err)
		}
		return decoded, nil
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(content)))
		if err != nil {
			return nil, fmt.Errorf("decode quoted-printable body: %w", err)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("unsupported Content-Transfer-Encoding %q", transferEncoding)
	}
}

func requireIdentityTransferEncoding(transferEncoding string) error {
	switch normalizeTransferEncoding(transferEncoding) {
	case "", "7bit", "8bit", "binary":
		return nil
	case "base64", "quoted-printable":
		return fmt.Errorf("multipart body must not use Content-Transfer-Encoding %q", transferEncoding)
	default:
		return fmt.Errorf("unsupported Content-Transfer-Encoding %q", transferEncoding)
	}
}

func normalizeTransferEncoding(transferEncoding string) string {
	return strings.ToLower(strings.TrimSpace(transferEncoding))
}

func splitHeaderAndBody(raw string) (string, string, error) {
	if strings.HasPrefix(raw, "\r\n") {
		return "", raw[2:], nil
	}
	if strings.HasPrefix(raw, "\n") {
		return "", raw[1:], nil
	}
	if index := strings.Index(raw, "\r\n\r\n"); index >= 0 {
		return raw[:index], raw[index+4:], nil
	}
	if index := strings.Index(raw, "\n\n"); index >= 0 {
		return raw[:index], raw[index+2:], nil
	}
	return "", "", fmt.Errorf("message is missing the header/body separator")
}

func parseHeaders(raw string) (map[string][]string, error) {
	values := make(map[string][]string)
	if raw == "" {
		return values, nil
	}
	var currentName string

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if currentName == "" {
				return nil, fmt.Errorf("header continuation has no preceding header")
			}
			last := len(values[currentName]) - 1
			values[currentName][last] += " " + strings.TrimSpace(line)
			continue
		}

		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, fmt.Errorf("malformed header %q", line)
		}
		currentName = strings.ToLower(strings.TrimSpace(line[:colon]))
		values[currentName] = append(values[currentName], strings.TrimSpace(line[colon+1:]))
	}

	return values, nil
}

func parseAddressList(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	addresses, err := stdmail.ParseAddressList(strings.Join(values, ", "))
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address.Name == "" {
			result = append(result, address.Address)
			continue
		}
		result = append(result, address.Name+" <"+address.Address+">")
	}
	return result, nil
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
