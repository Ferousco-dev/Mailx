package bimi

import (
	"strings"
	"testing"
)

func validSVG() string {
	return `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100" xmlns="http://www.w3.org/2000/svg"><title>Acme Corp</title><circle cx="50" cy="50" r="40"/></svg>`
}

func TestValidateSVGAcceptsCompliantLogo(t *testing.T) {
	res := ValidateSVG([]byte(validSVG()))
	if !res.Valid {
		t.Fatalf("expected valid, got reasons: %v", res.Reasons)
	}
	if !res.HasTitle || !res.SquareView {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestValidateSVGRejectsScript(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100"><title>x</title><script>alert(1)</script></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonScript)
}

func TestValidateSVGRejectsForeignObject(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100"><title>x</title><foreignObject><div/></foreignObject></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonForeignObject)
}

func TestValidateSVGRejectsRasterImage(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100"><title>x</title><image href="logo.png"/></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonRasterImage)
}

func TestValidateSVGRejectsExternalReference(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100"><title>x</title><use href="https://evil.example/x.svg#y"/></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonExternalRef)
}

func TestValidateSVGAllowsInternalFragmentReference(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100"><title>x</title><defs><circle id="c"/></defs><use href="#c"/></svg>`
	res := ValidateSVG([]byte(svg))
	if !res.Valid {
		t.Fatalf("expected valid (internal fragment ref allowed), got: %v", res.Reasons)
	}
}

func TestValidateSVGRejectsNonSquareViewBox(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 50"><title>x</title></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonNotSquare)
}

func TestValidateSVGRejectsMissingTitle(t *testing.T) {
	svg := `<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 100 100"></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonMissingTitle)
}

func TestValidateSVGRejectsWrongProfile(t *testing.T) {
	svg := `<svg version="1.1" viewBox="0 0 100 100"><title>x</title></svg>`
	res := ValidateSVG([]byte(svg))
	assertSVGReason(t, res, SVGReasonBadVersion)
	assertSVGReason(t, res, SVGReasonBadProfile)
}

func TestValidateSVGRejectsNonSVGRoot(t *testing.T) {
	res := ValidateSVG([]byte(`<html><body>not svg</body></html>`))
	assertSVGReason(t, res, SVGReasonNotSVGRoot)
}

func TestValidateSVGRejectsMalformedXML(t *testing.T) {
	res := ValidateSVG([]byte(`<svg version="1.2"`))
	assertSVGReason(t, res, SVGReasonNotXML)
}

func TestValidateSVGRejectsOversized(t *testing.T) {
	huge := "<svg version=\"1.2\" baseProfile=\"tiny-ps\" viewBox=\"0 0 1 1\"><title>" + strings.Repeat("a", MaxLogoBytes) + "</title></svg>"
	res := ValidateSVG([]byte(huge))
	assertSVGReason(t, res, SVGReasonTooLarge)
}

func TestValidateSVGBoundedAgainstDeepNesting(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 1 1"><title>x</title>`)
	for i := 0; i < 1000; i++ {
		b.WriteString("<g>")
	}
	res := ValidateSVG([]byte(b.String()))
	assertSVGReason(t, res, SVGReasonTooDeep)
}

func TestValidateSVGDoesNotExpandEntities(t *testing.T) {
	// A billion-laughs-shaped payload must not hang or panic; encoding/xml
	// with no Entity map configured for expansion should simply fail to parse
	// (or complete quickly) rather than expanding anything.
	svg := `<?xml version="1.0"?><!DOCTYPE svg [<!ENTITY a "x"><!ENTITY b "&a;&a;">]><svg version="1.2" baseProfile="tiny-ps" viewBox="0 0 1 1"><title>&b;</title></svg>`
	res := ValidateSVG([]byte(svg))
	if res.Valid {
		t.Fatalf("expected entity-bearing document to fail validation, got valid: %+v", res)
	}
}

func assertSVGReason(t *testing.T, res SVGResult, want string) {
	t.Helper()
	for _, r := range res.Reasons {
		if r == want {
			return
		}
	}
	t.Fatalf("expected reason %q in %v", want, res.Reasons)
}
