package api

import "mime"

// acceptsJSONContentType preserves the API's existing allowance for a missing
// Content-Type while accepting valid application/json parameters such as a
// charset. ParseMediaType also rejects malformed header values safely.
func acceptsJSONContentType(value string) bool {
	if value == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}
