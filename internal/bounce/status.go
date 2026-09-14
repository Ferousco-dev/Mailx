package bounce

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidEnhancedStatus indicates a value outside the RFC 3463 grammar.
var ErrInvalidEnhancedStatus = errors.New("bounce: invalid enhanced status code")

// EnhancedStatus is the structured class.subject.detail form defined by RFC
// 3463. The zero value represents missing or invalid status metadata.
type EnhancedStatus struct {
	Class   int
	Subject int
	Detail  int
}

// ParseEnhancedStatus parses an isolated enhanced-status token. It does not
// parse or extract status codes from complete SMTP reply lines.
func ParseEnhancedStatus(value string) (EnhancedStatus, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(parts[0]) != 1 {
		return EnhancedStatus{}, invalidEnhancedStatus(value)
	}
	class, ok := parseStatusComponent(parts[0])
	if !ok || (class != 2 && class != 4 && class != 5) {
		return EnhancedStatus{}, invalidEnhancedStatus(value)
	}
	subject, ok := parseStatusComponent(parts[1])
	if !ok {
		return EnhancedStatus{}, invalidEnhancedStatus(value)
	}
	detail, ok := parseStatusComponent(parts[2])
	if !ok {
		return EnhancedStatus{}, invalidEnhancedStatus(value)
	}
	return EnhancedStatus{Class: class, Subject: subject, Detail: detail}, nil
}

func parseStatusComponent(value string) (int, bool) {
	if value == "" || len(value) > 3 || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	number, err := strconv.Atoi(value)
	return number, err == nil
}

func invalidEnhancedStatus(value string) error {
	return fmt.Errorf("%w: %q", ErrInvalidEnhancedStatus, value)
}

// Valid reports whether the value can be represented by the RFC 3463 grammar.
func (s EnhancedStatus) Valid() bool {
	return (s.Class == 2 || s.Class == 4 || s.Class == 5) &&
		s.Subject >= 0 && s.Subject <= 999 &&
		s.Detail >= 0 && s.Detail <= 999
}

// String returns canonical class.subject.detail text. An invalid value,
// including the zero value used for missing metadata, formats as an empty string.
func (s EnhancedStatus) String() string {
	if !s.Valid() {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", s.Class, s.Subject, s.Detail)
}

func (s EnhancedStatus) IsSuccess() bool   { return s.Valid() && s.Class == 2 }
func (s EnhancedStatus) IsTemporary() bool { return s.Valid() && s.Class == 4 }
func (s EnhancedStatus) IsPermanent() bool { return s.Valid() && s.Class == 5 }

// StatusCategory is the broad source area described by the subject sub-code.
// Unknown includes subject zero and future/unregistered subject values.
type StatusCategory uint8

const (
	CategoryUnknown StatusCategory = iota
	CategoryAddress
	CategoryMailbox
	CategorySystem
	CategoryNetwork
	CategoryProtocol
	CategoryContent
	CategoryPolicy
)

// Category maps only the broad RFC subject groups and makes no policy or
// suppression decision from the status code.
func (s EnhancedStatus) Category() StatusCategory {
	if !s.Valid() {
		return CategoryUnknown
	}
	switch s.Subject {
	case 1:
		return CategoryAddress
	case 2:
		return CategoryMailbox
	case 3:
		return CategorySystem
	case 4:
		return CategoryNetwork
	case 5:
		return CategoryProtocol
	case 6:
		return CategoryContent
	case 7:
		return CategoryPolicy
	default:
		return CategoryUnknown
	}
}
