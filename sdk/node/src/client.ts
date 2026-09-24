import { MailXError, MailXErrorBody } from "./errors.js";
import type {
  Email,
  EmailList,
  SendEmailRequest,
  BatchSendRequest,
  BatchSendResponse,
  JSONObject,
} from "./types.js";

export interface MailXClientOptions {
  apiKey: string;
  baseUrl?: string;
  maxRetries?: number;
  timeoutMs?: number;
  fetchImpl?: typeof fetch;
}

const DEFAULT_BASE_URL = "https://api.mailx.dev/v1";

export class MailXClient {
  private apiKey: string;
  private baseUrl: string;
  private maxRetries: number;
  private timeoutMs: number;
  private fetchImpl: typeof fetch;

  constructor(opts: MailXClientOptions) {
    if (!opts.apiKey) throw new Error("MailXClient: apiKey is required");
    this.apiKey = opts.apiKey;
    this.baseUrl = (opts.baseUrl ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    this.maxRetries = opts.maxRetries ?? 3;
    this.timeoutMs = opts.timeoutMs ?? 30000;
    this.fetchImpl = opts.fetchImpl ?? fetch;
  }

  private async request<T>(
    method: string,
    path: string,
    opts: { body?: unknown; query?: Record<string, string | number | boolean | undefined>; idempotencyKey?: string } = {}
  ): Promise<T> {
    let url = this.baseUrl + path;
    if (opts.query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(opts.query)) {
        if (v !== undefined) qs.set(k, String(v));
      }
      const s = qs.toString();
      if (s) url += "?" + s;
    }

    let attempt = 0;
    for (;;) {
      const headers: Record<string, string> = { Authorization: `Bearer ${this.apiKey}` };
      if (opts.body !== undefined) headers["Content-Type"] = "application/json";
      if (opts.idempotencyKey) headers["Idempotency-Key"] = opts.idempotencyKey;

      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), this.timeoutMs);
      let res: Response;
      try {
        res = await this.fetchImpl(url, {
          method,
          headers,
          body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
          signal: controller.signal,
        });
      } finally {
        clearTimeout(timer);
      }

      if (res.ok) {
        if (res.status === 204) return undefined as T;
        return (await res.json()) as T;
      }

      const retryable = res.status === 429 || res.status >= 500;
      if (retryable && attempt < this.maxRetries) {
        const retryAfterHeader = res.headers.get("Retry-After");
        const delayMs = retryAfterHeader
          ? Number(retryAfterHeader) * 1000
          : Math.min(1000 * 2 ** attempt, 10000) + Math.random() * 250;
        attempt++;
        await new Promise((r) => setTimeout(r, delayMs));
        continue;
      }

      let body: MailXErrorBody;
      try {
        const parsed = (await res.json()) as { error: MailXErrorBody };
        body = parsed.error;
      } catch {
        body = { type: "unknown_error", code: "unknown_error", message: res.statusText };
      }
      const retryAfterHeader = res.headers.get("Retry-After");
      throw new MailXError(res.status, body, retryAfterHeader ? Number(retryAfterHeader) : undefined);
    }
  }

  // ---- Emails ----
  sendEmail(req: SendEmailRequest, idempotencyKey?: string): Promise<Email> {
    return this.request("POST", "/emails", { body: req, idempotencyKey });
  }
  sendBatch(req: BatchSendRequest): Promise<BatchSendResponse> {
    return this.request("POST", "/emails/batch", { body: req });
  }
  getEmail(id: string): Promise<Email> {
    return this.request("GET", `/emails/${encodeURIComponent(id)}`);
  }
  listEmails(opts?: { limit?: number; cursor?: string; status?: string }): Promise<EmailList> {
    return this.request("GET", "/emails", { query: opts });
  }

  // ---- Events ----
  listEvents(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/events", { query: opts });
  }

  // ---- Domains ----
  createDomain(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/domains", { body });
  }
  getDomain(id: string): Promise<JSONObject> {
    return this.request("GET", `/domains/${encodeURIComponent(id)}`);
  }
  listDomains(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/domains", { query: opts });
  }
  deleteDomain(id: string): Promise<void> {
    return this.request("DELETE", `/domains/${encodeURIComponent(id)}`);
  }
  verifyDomain(id: string): Promise<JSONObject> {
    return this.request("POST", `/domains/${encodeURIComponent(id)}/verify`);
  }
  getDomainDKIM(id: string): Promise<JSONObject> {
    return this.request("GET", `/domains/${encodeURIComponent(id)}/dkim`);
  }
  createDomainDKIM(id: string): Promise<JSONObject> {
    return this.request("POST", `/domains/${encodeURIComponent(id)}/dkim`);
  }
  verifyDomainDKIM(id: string): Promise<JSONObject> {
    return this.request("POST", `/domains/${encodeURIComponent(id)}/dkim/verify`);
  }
  getDomainSPF(id: string): Promise<JSONObject> {
    return this.request("GET", `/domains/${encodeURIComponent(id)}/spf`);
  }
  verifyDomainSPF(id: string): Promise<JSONObject> {
    return this.request("POST", `/domains/${encodeURIComponent(id)}/spf/verify`);
  }
  getDomainDMARC(id: string): Promise<JSONObject> {
    return this.request("GET", `/domains/${encodeURIComponent(id)}/dmarc`);
  }
  verifyDomainDMARC(id: string): Promise<JSONObject> {
    return this.request("POST", `/domains/${encodeURIComponent(id)}/dmarc/verify`);
  }
  getDomainBIMI(id: string): Promise<JSONObject> {
    return this.request("GET", `/domains/${encodeURIComponent(id)}/bimi`);
  }
  verifyDomainBIMI(id: string, validateAssets = false): Promise<JSONObject> {
    return this.request("POST", `/domains/${encodeURIComponent(id)}/bimi/verify`, {
      query: { validate_assets: validateAssets || undefined },
    });
  }

  // ---- Templates ----
  createTemplate(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/templates", { body });
  }
  getTemplate(id: string): Promise<JSONObject> {
    return this.request("GET", `/templates/${encodeURIComponent(id)}`);
  }
  updateTemplate(id: string, body: JSONObject): Promise<JSONObject> {
    return this.request("PATCH", `/templates/${encodeURIComponent(id)}`, { body });
  }
  deleteTemplate(id: string): Promise<void> {
    return this.request("DELETE", `/templates/${encodeURIComponent(id)}`);
  }
  listTemplates(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/templates", { query: opts });
  }

  // ---- Contacts ----
  createContact(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/contacts", { body });
  }
  getContact(id: string): Promise<JSONObject> {
    return this.request("GET", `/contacts/${encodeURIComponent(id)}`);
  }
  updateContact(id: string, body: JSONObject): Promise<JSONObject> {
    return this.request("PATCH", `/contacts/${encodeURIComponent(id)}`, { body });
  }
  deleteContact(id: string): Promise<void> {
    return this.request("DELETE", `/contacts/${encodeURIComponent(id)}`);
  }
  listContacts(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/contacts", { query: opts });
  }

  // ---- Audiences ----
  createAudience(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/audiences", { body });
  }
  getAudience(id: string): Promise<JSONObject> {
    return this.request("GET", `/audiences/${encodeURIComponent(id)}`);
  }
  updateAudience(id: string, body: JSONObject): Promise<JSONObject> {
    return this.request("PATCH", `/audiences/${encodeURIComponent(id)}`, { body });
  }
  deleteAudience(id: string): Promise<void> {
    return this.request("DELETE", `/audiences/${encodeURIComponent(id)}`);
  }
  listAudiences(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/audiences", { query: opts });
  }
  addAudienceContact(id: string, body: JSONObject): Promise<JSONObject> {
    return this.request("POST", `/audiences/${encodeURIComponent(id)}/contacts`, { body });
  }
  removeAudienceContact(id: string, contactId: string): Promise<void> {
    return this.request("DELETE", `/audiences/${encodeURIComponent(id)}/contacts/${encodeURIComponent(contactId)}`);
  }
  listAudienceContacts(id: string, opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", `/audiences/${encodeURIComponent(id)}/contacts`, { query: opts });
  }

  // ---- Broadcasts ----
  createBroadcast(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/broadcasts", { body });
  }
  getBroadcast(id: string): Promise<JSONObject> {
    return this.request("GET", `/broadcasts/${encodeURIComponent(id)}`);
  }
  listBroadcasts(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/broadcasts", { query: opts });
  }
  listBroadcastRecipients(id: string, opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", `/broadcasts/${encodeURIComponent(id)}/recipients`, { query: opts });
  }

  // ---- Analytics ----
  analyticsOverview(from: string, to: string): Promise<JSONObject> {
    return this.request("GET", "/analytics/overview", { query: { from, to } });
  }
  analyticsTimeseries(from: string, to: string, interval: "hour" | "day"): Promise<JSONObject> {
    return this.request("GET", "/analytics/timeseries", { query: { from, to, interval } });
  }
  analyticsBroadcast(id: string): Promise<JSONObject> {
    return this.request("GET", `/analytics/broadcasts/${encodeURIComponent(id)}`);
  }

  // ---- Suppressions ----
  createSuppression(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/suppressions", { body });
  }
  deleteSuppression(id: string): Promise<void> {
    return this.request("DELETE", `/suppressions/${encodeURIComponent(id)}`);
  }
  listSuppressions(opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", "/suppressions", { query: opts });
  }

  // ---- Webhooks ----
  createWebhook(body: JSONObject): Promise<JSONObject> {
    return this.request("POST", "/webhooks", { body });
  }
  getWebhook(id: string): Promise<JSONObject> {
    return this.request("GET", `/webhooks/${encodeURIComponent(id)}`);
  }
  deleteWebhook(id: string): Promise<void> {
    return this.request("DELETE", `/webhooks/${encodeURIComponent(id)}`);
  }
  listWebhooks(): Promise<JSONObject> {
    return this.request("GET", "/webhooks");
  }
  rotateWebhookSecret(id: string): Promise<JSONObject> {
    return this.request("POST", `/webhooks/${encodeURIComponent(id)}/rotate-secret`);
  }
  listWebhookDeliveries(id: string, opts?: { limit?: number; cursor?: string }): Promise<JSONObject> {
    return this.request("GET", `/webhooks/${encodeURIComponent(id)}/deliveries`, { query: opts });
  }
}
