package smtp

import (
	"fmt"
	"strconv"
	"strings"
)

type enhancedStatus string

const (
	statusOtherSuccess        enhancedStatus = "2.0.0"
	statusSenderAccepted      enhancedStatus = "2.1.0"
	statusRecipientAccepted   enhancedStatus = "2.1.5"
	statusMessageAccepted     enhancedStatus = "2.6.0"
	statusTemporarySystem     enhancedStatus = "4.3.0"
	statusTooManyRecipients   enhancedStatus = "4.5.3"
	statusInvalidCommand      enhancedStatus = "5.5.1"
	statusSyntaxError         enhancedStatus = "5.5.2"
	statusInvalidArguments    enhancedStatus = "5.5.4"
	statusMessageTooLarge     enhancedStatus = "5.3.4"
	statusMessageContentError enhancedStatus = "5.6.0"
)

type reply struct {
	code     int
	enhanced enhancedStatus
	lines    []string
}

func standardReply(code int, message string) reply {
	return reply{code: code, lines: []string{message}}
}

func multilineReply(code int, lines ...string) reply {
	return reply{code: code, lines: lines}
}

func enhancedReply(code int, status enhancedStatus, message string) reply {
	return reply{code: code, enhanced: status, lines: []string{message}}
}

func (r reply) wire() (string, error) {
	if r.code < 200 || r.code > 599 {
		return "", fmt.Errorf("invalid SMTP reply code %d", r.code)
	}
	if len(r.lines) == 0 {
		return "", fmt.Errorf("SMTP reply must contain at least one line")
	}
	if err := validateEnhancedStatus(r.code, r.enhanced); err != nil {
		return "", err
	}

	var wire strings.Builder
	for index, line := range r.lines {
		if line == "" || strings.ContainsAny(line, "\r\n") {
			return "", fmt.Errorf("invalid SMTP reply text")
		}
		separator := ' '
		if index < len(r.lines)-1 {
			separator = '-'
		}
		wire.WriteString(strconv.Itoa(r.code))
		wire.WriteRune(separator)
		if r.enhanced != "" {
			wire.WriteString(string(r.enhanced))
			wire.WriteByte(' ')
		}
		wire.WriteString(line)
		wire.WriteString("\r\n")
	}
	return wire.String(), nil
}

func validateEnhancedStatus(code int, status enhancedStatus) error {
	if status == "" {
		return nil
	}
	parts := strings.Split(string(status), ".")
	if len(parts) != 3 || len(parts[0]) != 1 {
		return fmt.Errorf("invalid enhanced SMTP status %q", status)
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 3 || (len(part) > 1 && part[0] == '0') {
			return fmt.Errorf("invalid enhanced SMTP status %q", status)
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return fmt.Errorf("invalid enhanced SMTP status %q", status)
			}
		}
	}
	if int(parts[0][0]-'0') != code/100 || (parts[0][0] != '2' && parts[0][0] != '4' && parts[0][0] != '5') {
		return fmt.Errorf("enhanced SMTP status %q does not match reply code %d", status, code)
	}
	return nil
}
