package smtp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Stage names each protocol step the outbound client can fail at.
type Stage string

const (
	StageDial         Stage = "dial"
	StageGreeting     Stage = "greeting"
	StageEHLO         Stage = "ehlo"
	StageHELO         Stage = "helo"
	StageStartTLS     Stage = "starttls"      // STARTTLS unavailable (required), refused, or its reply failed
	StageTLS          Stage = "tls_handshake" // the TLS handshake itself
	StageEHLOTLS      Stage = "ehlo_tls"      // the mandatory EHLO after a successful handshake
	StageMailFrom     Stage = "mail_from"
	StageRcptTo       Stage = "rcpt_to"
	StageData         Stage = "data"
	StageMessage      Stage = "message_transfer"
	StageDataResponse Stage = "data_response"
	StageQuit         Stage = "quit"
	StageInvalidInput Stage = "invalid_input"
)

// DeliveryError captures a failure at one SMTP stage. When it originated from
// a remote reply, Code, Enhanced, and Remote carry the parsed reply. Temporary
// distinguishes transient (4xx) from permanent (5xx) SMTP failures.
type DeliveryError struct {
	Stage     Stage
	Code      int
	Enhanced  string
	Remote    string
	Recipient string
	Temporary bool
	Err       error
}

func (e *DeliveryError) Error() string {
	var parts []string
	parts = append(parts, string(e.Stage))
	if e.Recipient != "" {
		parts = append(parts, "recipient="+e.Recipient)
	}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code=%d", e.Code))
	}
	if e.Enhanced != "" {
		parts = append(parts, "status="+e.Enhanced)
	}
	if e.Remote != "" {
		parts = append(parts, "remote="+strconv.Quote(e.Remote))
	}
	if e.Err != nil {
		parts = append(parts, "err="+e.Err.Error())
	}
	return "smtp outbound: " + strings.Join(parts, " ")
}

func (e *DeliveryError) Unwrap() error { return e.Err }

// IsTemporary reports whether err classifies as a transient SMTP failure.
func IsTemporary(err error) bool {
	var d *DeliveryError
	if errors.As(err, &d) {
		return d.Temporary
	}
	return false
}

// replyError builds a DeliveryError from a parsed remote reply.
func replyError(stage Stage, code int, enhanced, msg string) *DeliveryError {
	return &DeliveryError{
		Stage:     stage,
		Code:      code,
		Enhanced:  enhanced,
		Remote:    msg,
		Temporary: code >= 400 && code < 500,
	}
}

// deliveryErrorFromRead wraps a transport read/write failure with its stage.
func deliveryErrorFromRead(stage Stage, err error) *DeliveryError {
	return &DeliveryError{Stage: stage, Err: err}
}
