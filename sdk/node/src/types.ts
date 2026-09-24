export interface SendEmailRequest {
  from: string;
  to: string[];
  cc?: string[];
  bcc?: string[];
  reply_to?: string;
  subject?: string;
  html?: string;
  text?: string;
  template_id?: string;
  variables?: Record<string, string>;
  scheduled_at?: string | null;
  track_opens?: boolean;
  track_clicks?: boolean;
}

export interface Email {
  id: string;
  from: string;
  to: string[];
  cc?: string[];
  bcc?: string[];
  reply_to?: string;
  subject: string;
  html?: string;
  text?: string;
  status: string;
  created_at: string;
  queued_at?: string;
  delivered_at?: string;
}

export interface EmailList {
  data: Email[];
  next_cursor: string | null;
}

export interface BatchSendItem extends SendEmailRequest {
  idempotency_key?: string;
}

export interface BatchSendRequest {
  emails: BatchSendItem[];
}

export interface BatchItemError {
  type: string;
  code: string;
  message: string;
}

export interface BatchSendResultItem {
  index: number;
  email?: Email;
  error?: BatchItemError;
}

export interface BatchSendResponse {
  data: BatchSendResultItem[];
  accepted: number;
  rejected: number;
}

// Every other resource (domains, DKIM, SPF, DMARC, BIMI, webhooks,
// templates, contacts, audiences, broadcasts, analytics, suppressions,
// events) uses loosely-typed JSON bodies/results — see the OpenAPI spec
// (GET /openapi.json on your MailX server) for the exact shape of each.
export type JSONObject = Record<string, unknown>;
