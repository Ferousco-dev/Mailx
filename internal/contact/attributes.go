package contact

import "errors"

// Attributes are bounded string key/value pairs, deliberately not arbitrary
// JSON: no nesting, no arrays, no executable values — a flat profile blob
// future template/audience features can read safely.
const (
	MaxAttributes        = 20
	MaxAttributeKeyLen   = 64
	MaxAttributeValueLen = 500
	MaxNameLen           = 200
)

var (
	ErrTooManyAttributes     = errors.New("contact: too many attributes")
	ErrAttributeKeyTooLong   = errors.New("contact: an attribute key is too long")
	ErrAttributeValueTooLong = errors.New("contact: an attribute value is too long")
	ErrNameTooLong           = errors.New("contact: name is too long")
)

func ValidateAttributes(attrs map[string]string) error {
	if len(attrs) > MaxAttributes {
		return ErrTooManyAttributes
	}
	for k, v := range attrs {
		if len(k) == 0 || len(k) > MaxAttributeKeyLen {
			return ErrAttributeKeyTooLong
		}
		if len(v) > MaxAttributeValueLen {
			return ErrAttributeValueTooLong
		}
	}
	return nil
}

func ValidateName(name string) error {
	if len(name) > MaxNameLen {
		return ErrNameTooLong
	}
	return nil
}
