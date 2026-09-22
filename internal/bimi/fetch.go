package bimi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Ferousco-dev/mailx/internal/webhook"
)

// AssetFetcher retrieves a BIMI-referenced asset (logo SVG or authority
// certificate) safely: HTTPS only, bounded response size, bounded timeout,
// no redirects followed (the BIMI draft does not specify redirect
// handling, so MailX takes the conservative reading — l=/a= must name the
// asset directly), and SSRF-hardened (reuses internal/webhook's
// DNS-rebinding-safe URLPolicy: every dial re-resolves and re-validates the
// target address, rejecting private/loopback/link-local/reserved ranges).
type AssetFetcher struct {
	client  *http.Client
	maxSize int64
}

var (
	ErrAssetTooLarge = errors.New("bimi: fetched asset exceeds the size limit")
	ErrAssetNotHTTPS = errors.New("bimi: asset URL must be HTTPS")
	ErrAssetFetch    = errors.New("bimi: could not fetch asset")
)

// NewAssetFetcher builds a fetcher bounded to maxSize bytes and the given
// policy (SSRF/DNS-rebinding protection — see internal/webhook.URLPolicy).
func NewAssetFetcher(policy webhook.URLPolicy, maxSize int64, timeout time.Duration) *AssetFetcher {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	return &AssetFetcher{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// The BIMI draft does not address redirects for l=/a=; MailX
			// takes the conservative reading and never follows one.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxSize: maxSize,
	}
}

// Fetch retrieves rawURL (already validated as an https:// URL by
// record.Parse) and returns at most maxSize+1 bytes — callers must treat a
// response of exactly maxSize+1 bytes as ErrAssetTooLarge, which this
// method already does. A non-2xx or redirect response is ErrAssetFetch
// (the specific status is not exposed, since it is a remote-server detail
// that must never gate MailX's own delivery pipeline).
func (f *AssetFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	if !isHTTPSURL(rawURL) {
		return nil, ErrAssetNotHTTPS
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAssetFetch, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAssetFetch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ErrAssetFetch
	}
	limited := io.LimitReader(resp.Body, f.maxSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAssetFetch, err)
	}
	if int64(len(body)) > f.maxSize {
		return nil, ErrAssetTooLarge
	}
	return body, nil
}
