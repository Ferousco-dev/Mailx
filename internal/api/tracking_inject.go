package api

import (
	"regexp"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/tracking"
)

// hrefPattern matches both valid HTML attribute-quoting styles
// (href="..." and href='...') - only recognizing double quotes would
// silently leave single-quoted links untracked.
var hrefPattern = regexp.MustCompile(`(?i)href\s*=\s*(?:"(https?://[^"]+)"|'(https?://[^']+)')`)

func (h *emailHandler) injectTracking(tenantID, messageID, recipient, html string, trackOpens, trackClicks bool) (string, error) {
	if html == "" || h.trackingSecret == nil || h.trackingBaseURL == "" {
		return html, nil
	}
	out := html
	if trackClicks {
		var rewriteErr error
		out = hrefPattern.ReplaceAllStringFunc(out, func(m string) string {
			if rewriteErr != nil {
				return m
			}
			sub := hrefPattern.FindStringSubmatch(m)
			url := sub[1]
			if url == "" {
				url = sub[2]
			}
			if strings.Contains(strings.ToLower(url), "unsubscribe") {
				return m
			}
			token, err := tracking.Sign(h.trackingSecret, tracking.Payload{TenantID: tenantID, MessageID: messageID, Recipient: recipient, URL: url})
			if err != nil {
				rewriteErr = err
				return m
			}
			return `href="` + h.trackingBaseURL + "/track/click/" + token + `"`
		})
		if rewriteErr != nil {
			return "", rewriteErr
		}
	}
	if trackOpens {
		token, err := tracking.Sign(h.trackingSecret, tracking.Payload{TenantID: tenantID, MessageID: messageID, Recipient: recipient})
		if err != nil {
			return "", err
		}
		pixel := `<img src="` + h.trackingBaseURL + "/track/open/" + token + `" width="1" height="1" alt="" style="display:none">`
		if idx := strings.LastIndex(strings.ToLower(out), "</body>"); idx >= 0 {
			out = out[:idx] + pixel + out[idx:]
		} else {
			out += pixel
		}
	}
	return out, nil
}
