package mailx

type SendEmailRequest struct {
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	HTML        string            `json:"html,omitempty"`
	Text        string            `json:"text,omitempty"`
	TemplateID  string            `json:"template_id,omitempty"`
	Variables   map[string]string `json:"variables,omitempty"`
	ScheduledAt *string           `json:"scheduled_at,omitempty"`
	TrackOpens  bool              `json:"track_opens,omitempty"`
	TrackClicks bool              `json:"track_clicks,omitempty"`
}

type Email struct {
	ID          string   `json:"id"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Cc          []string `json:"cc,omitempty"`
	Bcc         []string `json:"bcc,omitempty"`
	ReplyTo     string   `json:"reply_to,omitempty"`
	Subject     string   `json:"subject"`
	HTML        *string  `json:"html,omitempty"`
	Text        *string  `json:"text,omitempty"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"created_at"`
	QueuedAt    *string  `json:"queued_at,omitempty"`
	DeliveredAt *string  `json:"delivered_at,omitempty"`
}

type EmailList struct {
	Data       []Email `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type BatchSendItem struct {
	SendEmailRequest
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type BatchSendRequest struct {
	Emails []BatchSendItem `json:"emails"`
}

type BatchItemError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BatchSendResultItem struct {
	Index int             `json:"index"`
	Email *Email          `json:"email,omitempty"`
	Error *BatchItemError `json:"error,omitempty"`
}

type BatchSendResponse struct {
	Data     []BatchSendResultItem `json:"data"`
	Accepted int                   `json:"accepted"`
	Rejected int                   `json:"rejected"`
}

// JSON is a loosely-typed body/result for resources without a hand-typed
// Go struct in this SDK yet (domains, DKIM/SPF/DMARC/BIMI, webhooks,
// templates, contacts, audiences, broadcasts, analytics, suppressions,
// events) — see GET /openapi.json on your server for exact shapes.
type JSON = map[string]any
