export interface MailXErrorBody {
  type: string;
  code: string;
  message: string;
  request_id?: string;
}

export class MailXError extends Error {
  readonly status: number;
  readonly type: string;
  readonly code: string;
  readonly requestId?: string;
  readonly retryAfter?: number;

  constructor(status: number, body: MailXErrorBody, retryAfter?: number) {
    super(body.message);
    this.name = "MailXError";
    this.status = status;
    this.type = body.type;
    this.code = body.code;
    this.requestId = body.request_id;
    this.retryAfter = retryAfter;
  }
}
