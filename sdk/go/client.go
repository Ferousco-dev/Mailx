package mailx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

const defaultBaseURL = "https://api.mailx.dev"

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	maxRetries int
}

type Option func(*Client)

func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = url }
}

func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		maxRetries: 3,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) request(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(respBody) > 0 {
				return json.Unmarshal(respBody, out)
			}
			return nil
		}

		apiErr := parseAPIError(resp.StatusCode, resp.Header.Get("Retry-After"), respBody)
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < c.maxRetries {
			lastErr = apiErr
			continue
		}
		return apiErr
	}
	return lastErr
}

func backoff(attempt int, lastErr error) time.Duration {
	if apiErr, ok := lastErr.(*APIError); ok && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	base := time.Duration(math.Min(1000*math.Pow(2, float64(attempt)), 10000)) * time.Millisecond
	jitter := time.Duration(rand.Intn(250)) * time.Millisecond
	return base + jitter
}

func parseAPIError(status int, retryAfterHeader string, body []byte) *APIError {
	var parsed struct {
		Type      string `json:"type"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(body, &parsed)
	var retryAfter time.Duration
	if retryAfterHeader != "" {
		if secs, err := strconv.Atoi(retryAfterHeader); err == nil {
			retryAfter = time.Duration(secs) * time.Second
		}
	}
	return &APIError{
		Status:     status,
		Type:       parsed.Type,
		Code:       parsed.Code,
		Message:    parsed.Message,
		RequestID:  parsed.RequestID,
		RetryAfter: retryAfter,
	}
}

func (c *Client) SendEmail(ctx context.Context, req SendEmailRequest) (*Email, error) {
	var out Email
	if err := c.request(ctx, http.MethodPost, "/v1/emails", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SendBatch(ctx context.Context, req BatchSendRequest) (*BatchSendResponse, error) {
	var out BatchSendResponse
	if err := c.request(ctx, http.MethodPost, "/v1/emails/batch", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetEmail(ctx context.Context, id string) (*Email, error) {
	var out Email
	if err := c.request(ctx, http.MethodGet, "/v1/emails/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListEmails(ctx context.Context, query string) (*EmailList, error) {
	var out EmailList
	if err := c.request(ctx, http.MethodGet, "/v1/emails"+query, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListEvents(ctx context.Context, emailID string) (JSON, error) {
	var out JSON
	if err := c.request(ctx, http.MethodGet, "/v1/emails/"+emailID+"/events", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) resource(ctx context.Context, method, path string, body any) (JSON, error) {
	var out JSON
	if err := c.request(ctx, method, path, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) CreateDomain(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/domains", body)
}
func (c *Client) GetDomain(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/domains/"+id, nil)
}
func (c *Client) ListDomains(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/domains", nil)
}
func (c *Client) DeleteDomain(ctx context.Context, id string) error {
	_, err := c.resource(ctx, http.MethodDelete, "/v1/domains/"+id, nil)
	return err
}
func (c *Client) VerifyDKIM(ctx context.Context, domainID string) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/domains/"+domainID+"/dkim/verify", nil)
}
func (c *Client) GetSPF(ctx context.Context, domainID string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/domains/"+domainID+"/spf", nil)
}
func (c *Client) GetDMARC(ctx context.Context, domainID string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/domains/"+domainID+"/dmarc", nil)
}
func (c *Client) SetBIMI(ctx context.Context, domainID string, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPut, "/v1/domains/"+domainID+"/bimi", body)
}

func (c *Client) CreateTemplate(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/templates", body)
}
func (c *Client) GetTemplate(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/templates/"+id, nil)
}
func (c *Client) ListTemplates(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/templates", nil)
}
func (c *Client) UpdateTemplate(ctx context.Context, id string, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPatch, "/v1/templates/"+id, body)
}
func (c *Client) DeleteTemplate(ctx context.Context, id string) error {
	_, err := c.resource(ctx, http.MethodDelete, "/v1/templates/"+id, nil)
	return err
}

func (c *Client) CreateContact(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/contacts", body)
}
func (c *Client) GetContact(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/contacts/"+id, nil)
}
func (c *Client) ListContacts(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/contacts", nil)
}
func (c *Client) UpdateContact(ctx context.Context, id string, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPatch, "/v1/contacts/"+id, body)
}
func (c *Client) DeleteContact(ctx context.Context, id string) error {
	_, err := c.resource(ctx, http.MethodDelete, "/v1/contacts/"+id, nil)
	return err
}

func (c *Client) CreateAudience(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/audiences", body)
}
func (c *Client) GetAudience(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/audiences/"+id, nil)
}
func (c *Client) ListAudiences(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/audiences", nil)
}
func (c *Client) DeleteAudience(ctx context.Context, id string) error {
	_, err := c.resource(ctx, http.MethodDelete, "/v1/audiences/"+id, nil)
	return err
}

func (c *Client) CreateBroadcast(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/broadcasts", body)
}
func (c *Client) GetBroadcast(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/broadcasts/"+id, nil)
}
func (c *Client) ListBroadcasts(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/broadcasts", nil)
}
func (c *Client) SendBroadcast(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/broadcasts/"+id+"/send", nil)
}

func (c *Client) GetAnalytics(ctx context.Context, query string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/analytics"+query, nil)
}

func (c *Client) ListSuppressions(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/suppressions", nil)
}
func (c *Client) CreateSuppression(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/suppressions", body)
}
func (c *Client) DeleteSuppression(ctx context.Context, id string) error {
	_, err := c.resource(ctx, http.MethodDelete, "/v1/suppressions/"+id, nil)
	return err
}

func (c *Client) CreateWebhook(ctx context.Context, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPost, "/v1/webhooks", body)
}
func (c *Client) GetWebhook(ctx context.Context, id string) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/webhooks/"+id, nil)
}
func (c *Client) ListWebhooks(ctx context.Context) (JSON, error) {
	return c.resource(ctx, http.MethodGet, "/v1/webhooks", nil)
}
func (c *Client) UpdateWebhook(ctx context.Context, id string, body JSON) (JSON, error) {
	return c.resource(ctx, http.MethodPatch, "/v1/webhooks/"+id, body)
}
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	_, err := c.resource(ctx, http.MethodDelete, "/v1/webhooks/"+id, nil)
	return err
}
