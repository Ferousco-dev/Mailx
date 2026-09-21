package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

const (
	APIVersion       = "2026-09-01"
	MaxAttempts      = 8
	maxResponseBytes = 4096
	maxRetryAfter    = time.Hour
)

type Client struct {
	httpClient *http.Client
	now        func() time.Time
}

type EventEnvelope struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	APIVersion string         `json:"api_version"`
	CreatedAt  time.Time      `json:"created_at"`
	Data       map[string]any `json:"data"`
}

type AttemptOutcome struct {
	Status        string
	ResponseCode  *int
	ErrorCategory string
	NextRetryAt   *time.Time
	Duration      time.Duration
}

func NewClient(policy URLPolicy) *Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           policy.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: func() time.Time { return time.Now().UTC() },
	}
}

func payloadFor(event database.Event) (EventEnvelope, error) {
	publicType, ok := database.PublicEventType(event.Type)
	if !ok {
		return EventEnvelope{}, errors.New("webhook: event is not public")
	}
	data := map[string]any{"email_id": event.MessageID, "status": publicTypeStatus(publicType)}
	if event.DeliveryAttemptNumber != nil {
		data["attempt_number"] = *event.DeliveryAttemptNumber
	}
	if value, ok := event.Metadata["next_retry_at"]; ok && publicType == EventDeliveryDelayed {
		data["next_retry_at"] = value
	}
	return EventEnvelope{ID: event.ID, Type: publicType, APIVersion: APIVersion, CreatedAt: event.OccurredAt, Data: data}, nil
}

func publicTypeStatus(eventType string) string {
	switch eventType {
	case EventDeliveryDelayed:
		return "retrying"
	default:
		return eventType[len("email."):]
	}
}

// BuildRequest serializes once, signs those exact bytes, and installs those
// same bytes as the request body.
func (c *Client) BuildRequest(ctx context.Context, claim database.ClaimedWebhookDelivery, secret string) (*http.Request, []byte, error) {
	payload, err := payloadFor(claim.Event)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claim.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	timestamp := strconv.FormatInt(c.now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "MailX-Webhooks/1")
	req.Header.Set("MailX-Webhook-Id", claim.ID)
	req.Header.Set("MailX-Event-Id", claim.Event.ID)
	req.Header.Set("MailX-Webhook-Timestamp", timestamp)
	req.Header.Set("MailX-Webhook-Signature", Signature(secret, timestamp, body))
	return req, body, nil
}

func (c *Client) Deliver(ctx context.Context, claim database.ClaimedWebhookDelivery, secret string) AttemptOutcome {
	started := c.now()
	req, _, err := c.BuildRequest(ctx, claim, secret)
	if err != nil {
		return c.retryOrFail(claim.AttemptCount, "request_build", nil, "", started)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.retryOrFail(claim.AttemptCount, "network", nil, "", started)
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseBytes+1)
	code := resp.StatusCode
	if code >= 200 && code < 300 {
		return AttemptOutcome{Status: "succeeded", ResponseCode: &code, Duration: c.now().Sub(started)}
	}
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 {
		return c.retryOrFail(claim.AttemptCount, "http_retryable", &code, resp.Header.Get("Retry-After"), started)
	}
	return AttemptOutcome{Status: "failed", ResponseCode: &code, ErrorCategory: "http_terminal", Duration: c.now().Sub(started)}
}

func (c *Client) retryOrFail(attempt int, category string, code *int, retryAfter string, started time.Time) AttemptOutcome {
	result := AttemptOutcome{ResponseCode: code, ErrorCategory: category, Duration: c.now().Sub(started)}
	if attempt >= MaxAttempts {
		result.Status = "failed"
		return result
	}
	result.Status = "retrying"
	delay := retryDelay(attempt, randomUint64())
	if serverDelay, ok := parseRetryAfter(retryAfter, c.now()); ok && serverDelay > delay {
		delay = serverDelay
	}
	if delay > maxRetryAfter {
		delay = maxRetryAfter
	}
	next := c.now().Add(delay)
	result.NextRetryAt = &next
	return result
}

func retryDelay(attempt int, entropy uint64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Minute
	for n := 1; n < attempt && delay < time.Hour; n++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	// Equal jitter in [50%,100%] avoids retry waves while retaining a floor.
	half := delay / 2
	return half + time.Duration(entropy%uint64(half+1))
}

func randomUint64() uint64 {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0
	}
	return binary.BigEndian.Uint64(raw[:])
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter), true
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0, false
	}
	return min(when.Sub(now), maxRetryAfter), true
}

func (c *Client) CloseIdleConnections() { c.httpClient.CloseIdleConnections() }
