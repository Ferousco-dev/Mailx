// Package emailtemplate is MailX's deliberately small, deterministic
// {{variable}} substitution engine for reusable email templates (v0.33).
//
// It is NOT text/template or html/template: those support actions
// (if/range/pipelines/function calls), which is more language than a mail
// personalization field needs and a bigger attack surface than this milestone
// wants. This package supports exactly one construct — {{identifier}} — and
// nothing else is ever executed or expanded.
package emailtemplate

import (
	"errors"
	"html"
	"regexp"
	"strings"
)

const (
	MaxNameLen          = 200
	MaxSubjectLen       = 500
	MaxBodyLen          = 2 << 20 // matches the migration CHECK and the email API's own body limit
	MaxVariables        = 50
	MaxVariableKeyLen   = 64
	MaxVariableValueLen = 4096
)

var (
	ErrEmptyName       = errors.New("emailtemplate: name is required")
	ErrNameTooLong     = errors.New("emailtemplate: name is too long")
	ErrEmptyBody       = errors.New("emailtemplate: at least one of text or html is required")
	ErrTooLarge        = errors.New("emailtemplate: content exceeds the maximum size")
	ErrTooManyVars     = errors.New("emailtemplate: too many variables")
	ErrVarKeyTooLong   = errors.New("emailtemplate: a variable name is too long")
	ErrVarValueTooLong = errors.New("emailtemplate: a variable value is too long")
)

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Validate checks a template's own fields (not variables) against MailX's
// bounds. Subject bounds match the plain-send API (maxSubjectLen) so a
// template can never produce something plain sends could not.
func Validate(name, subject, text, htmlBody string) error {
	if strings.TrimSpace(name) == "" {
		return ErrEmptyName
	}
	if len(name) > MaxNameLen {
		return ErrNameTooLong
	}
	if len(subject) > MaxSubjectLen {
		return errors.New("emailtemplate: subject is too long")
	}
	if len(text) > MaxBodyLen || len(htmlBody) > MaxBodyLen {
		return ErrTooLarge
	}
	if strings.TrimSpace(text) == "" && strings.TrimSpace(htmlBody) == "" {
		return ErrEmptyBody
	}
	return nil
}

// ValidateVariables bounds the caller-supplied substitution map before any
// rendering happens: count, key length, value length. Rejecting oversized
// input here means Render itself never has to allocate for a hostile map.
func ValidateVariables(vars map[string]string) error {
	if len(vars) > MaxVariables {
		return ErrTooManyVars
	}
	for k, v := range vars {
		if len(k) > MaxVariableKeyLen {
			return ErrVarKeyTooLong
		}
		if len(v) > MaxVariableValueLen {
			return ErrVarValueTooLong
		}
	}
	return nil
}

// Rendered is the result of substituting variables into a template's three
// fields, ready to feed straight into outbound.Build — no further rendering
// ever happens after this (retries resend these exact bytes, matching the
// existing send-once-render-once invariant).
type Rendered struct {
	Subject string
	Text    string
	HTML    string
}

// Render substitutes {{name}} tokens in subject/text/html. Text and subject
// get the RAW variable value (subject-header-injection safety is the
// existing outbound.Build's job, via rejectControlChars, applied to whatever
// string it is given — templated or not). HTML gets the value
// HTML-ESCAPED: this is the only variable-safety model v0.33 implements —
// there is no raw/unescaped-HTML variable mode. A missing variable renders
// as an empty string; a malformed token (not a valid identifier, or an
// unmatched "{{") is left in the output verbatim rather than erroring, so a
// literal "{{" in body copy does not break sending.
func Render(subject, text, htmlBody string, vars map[string]string) (Rendered, error) {
	s, err := substitute(subject, vars, false)
	if err != nil {
		return Rendered{}, err
	}
	t, err := substitute(text, vars, false)
	if err != nil {
		return Rendered{}, err
	}
	h, err := substitute(htmlBody, vars, true)
	if err != nil {
		return Rendered{}, err
	}
	return Rendered{Subject: s, Text: t, HTML: h}, nil
}

// substitute does one linear left-to-right pass over tpl: each iteration
// consumes a strictly non-empty prefix of what remains, so total work is
// O(len(tpl)) regardless of how many "{{" it contains (no quadratic
// rescanning, no recursive expansion — a substituted value is never
// re-scanned for further "{{...}}" tokens). out is capped at MaxBodyLen so a
// pathological template cannot force an unbounded allocation.
func substitute(tpl string, vars map[string]string, escapeHTML bool) (string, error) {
	var out strings.Builder
	rest := tpl
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if err := grow(&out, rest); err != nil {
				return "", err
			}
			break
		}
		if err := grow(&out, rest[:i]); err != nil {
			return "", err
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 { // unmatched "{{": emit the remainder verbatim and stop
			if err := grow(&out, rest[i:]); err != nil {
				return "", err
			}
			break
		}
		name := strings.TrimSpace(rest[i+2 : i+j])
		if identRE.MatchString(name) {
			v := vars[name]
			if escapeHTML {
				v = html.EscapeString(v)
			}
			if err := grow(&out, v); err != nil {
				return "", err
			}
		} else {
			if err := grow(&out, rest[i:i+j+2]); err != nil { // malformed token, kept literal
				return "", err
			}
		}
		rest = rest[i+j+2:]
	}
	return out.String(), nil
}

func grow(out *strings.Builder, s string) error {
	if out.Len()+len(s) > MaxBodyLen {
		return ErrTooLarge
	}
	out.WriteString(s)
	return nil
}
