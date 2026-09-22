package bimi

import (
	"encoding/xml"
	"io"
	"strconv"
	"strings"
)

// MaxLogoBytes is the BIMI Group's published SVG Tiny P/S size ceiling.
const MaxLogoBytes = 32 * 1024

// SVGResult reports what MailX's structural check found. This is NOT a
// certification of full SVG Tiny 1.2 Portable/Secure conformance — MailX
// checks the security-relevant subset (no scripts, no external references,
// no raster/style embedding, required root attributes, a title, a square
// viewBox, the size bound) that it can verify safely and truthfully. A
// provider's own renderer is the actual authority on full-profile
// conformance.
type SVGResult struct {
	Valid      bool
	Reasons    []string // bounded codes, never raw markup
	HasTitle   bool
	SquareView bool
}

// SVG validation reason codes.
const (
	SVGReasonTooLarge       = "logo_too_large"
	SVGReasonNotXML         = "not_well_formed_xml"
	SVGReasonNotSVGRoot     = "root_element_not_svg"
	SVGReasonBadVersion     = "missing_or_wrong_version"
	SVGReasonBadProfile     = "missing_or_wrong_base_profile"
	SVGReasonMissingViewBox = "missing_viewbox"
	SVGReasonNotSquare      = "viewbox_not_square"
	SVGReasonMissingTitle   = "missing_title_element"
	SVGReasonScript         = "contains_script_element"
	SVGReasonForeignObject  = "contains_foreignobject_element"
	SVGReasonRasterImage    = "contains_raster_image_element"
	SVGReasonStyleElement   = "contains_style_element"
	SVGReasonExternalRef    = "contains_external_reference"
	SVGReasonTooDeep        = "document_too_deeply_nested"
	SVGReasonTooManyNodes   = "document_too_many_elements"
)

const (
	maxSVGDepth = 64
	maxSVGNodes = 20000
)

// bannedElements cannot appear anywhere in a BIMI SVG: each is either a
// script/animation vector or an embedded-raster/external-CSS mechanism the
// Portable/Secure profile forbids.
var bannedElements = map[string]string{
	"script":           SVGReasonScript,
	"foreignObject":    SVGReasonForeignObject,
	"image":            SVGReasonRasterImage,
	"style":            SVGReasonStyleElement,
	"animate":          SVGReasonScript,
	"animateTransform": SVGReasonScript,
	"animateMotion":    SVGReasonScript,
	"set":              SVGReasonScript,
}

// ValidateSVG performs a bounded, safe structural check of a candidate BIMI
// logo. It never executes scripts, never resolves external entities or
// references (Go's encoding/xml does not fetch DTDs/entities), and stops
// reading well before an oversized or deeply-nested document could exhaust
// memory.
func ValidateSVG(data []byte) SVGResult {
	var res SVGResult
	if len(data) > MaxLogoBytes {
		res.Reasons = append(res.Reasons, SVGReasonTooLarge)
		return res
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	dec.Entity = map[string]string{} // no entity expansion at all

	depth := 0
	nodes := 0
	sawRoot := false
	rootOK := true

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			res.Reasons = append(res.Reasons, SVGReasonNotXML)
			return res
		}
		switch t := tok.(type) {
		case xml.StartElement:
			nodes++
			depth++
			if nodes > maxSVGNodes {
				res.Reasons = append(res.Reasons, SVGReasonTooManyNodes)
				return res
			}
			if depth > maxSVGDepth {
				res.Reasons = append(res.Reasons, SVGReasonTooDeep)
				return res
			}
			local := t.Name.Local
			if !sawRoot {
				sawRoot = true
				if local != "svg" {
					res.Reasons = append(res.Reasons, SVGReasonNotSVGRoot)
					rootOK = false
				} else {
					checkRootAttrs(t, &res)
				}
			}
			if reason, banned := bannedElements[local]; banned {
				res.Reasons = append(res.Reasons, reason)
			}
			if local == "title" {
				res.HasTitle = true
			}
			if hasExternalRef(t) {
				res.Reasons = append(res.Reasons, SVGReasonExternalRef)
			}
		case xml.EndElement:
			depth--
		}
	}
	if !sawRoot || !rootOK {
		if !sawRoot {
			res.Reasons = append(res.Reasons, SVGReasonNotXML)
		}
		return res
	}
	if !res.HasTitle {
		res.Reasons = append(res.Reasons, SVGReasonMissingTitle)
	}
	res.Valid = len(res.Reasons) == 0
	return res
}

func checkRootAttrs(t xml.StartElement, res *SVGResult) {
	var version, profile, viewBox string
	for _, a := range t.Attr {
		switch a.Name.Local {
		case "version":
			version = a.Value
		case "baseProfile":
			profile = a.Value
		case "viewBox":
			viewBox = a.Value
		}
	}
	if version != "1.2" {
		res.Reasons = append(res.Reasons, SVGReasonBadVersion)
	}
	if profile != "tiny-ps" {
		res.Reasons = append(res.Reasons, SVGReasonBadProfile)
	}
	if viewBox == "" {
		res.Reasons = append(res.Reasons, SVGReasonMissingViewBox)
		return
	}
	w, h, ok := parseViewBoxWH(viewBox)
	if !ok || w != h {
		res.Reasons = append(res.Reasons, SVGReasonNotSquare)
		return
	}
	res.SquareView = true
}

func parseViewBoxWH(v string) (w, h float64, ok bool) {
	fields := strings.Fields(v)
	if len(fields) != 4 {
		return 0, 0, false
	}
	w, err1 := strconv.ParseFloat(fields[2], 64)
	h, err2 := strconv.ParseFloat(fields[3], 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// hasExternalRef reports whether t carries an href/xlink:href attribute
// pointing somewhere other than a same-document fragment ("#..."), the only
// reference form the Portable/Secure profile allows.
func hasExternalRef(t xml.StartElement) bool {
	for _, a := range t.Attr {
		if a.Name.Local != "href" {
			continue
		}
		v := strings.TrimSpace(a.Value)
		if v != "" && !strings.HasPrefix(v, "#") {
			return true
		}
	}
	return false
}
