import random
import time
from typing import Any, Dict, List, Optional

import requests

from .errors import MailXError

DEFAULT_BASE_URL = "https://api.mailx.dev/v1"


class MailXClient:
    def __init__(self, api_key: str, base_url: str = DEFAULT_BASE_URL, max_retries: int = 3, timeout: float = 30.0, session: Optional[requests.Session] = None):
        if not api_key:
            raise ValueError("MailXClient: api_key is required")
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.max_retries = max_retries
        self.timeout = timeout
        self.session = session or requests.Session()

    def _request(self, method: str, path: str, json: Any = None, params: Dict[str, Any] = None, idempotency_key: str = None) -> Any:
        url = self.base_url + path
        headers = {"Authorization": f"Bearer {self.api_key}"}
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        if params:
            params = {k: v for k, v in params.items() if v is not None}

        attempt = 0
        while True:
            resp = self.session.request(method, url, json=json, params=params, headers=headers, timeout=self.timeout)
            if resp.ok:
                if resp.status_code == 204 or not resp.content:
                    return None
                return resp.json()

            retryable = resp.status_code == 429 or resp.status_code >= 500
            if retryable and attempt < self.max_retries:
                retry_after = resp.headers.get("Retry-After")
                delay = float(retry_after) if retry_after else min(1.0 * (2 ** attempt), 10.0) + random.random() * 0.25
                attempt += 1
                time.sleep(delay)
                continue

            try:
                body = resp.json().get("error", {})
            except ValueError:
                body = {"type": "unknown_error", "code": "unknown_error", "message": resp.reason}
            retry_after = resp.headers.get("Retry-After")
            raise MailXError(
                resp.status_code,
                body.get("type", "unknown_error"),
                body.get("code", "unknown_error"),
                body.get("message", ""),
                body.get("request_id"),
                float(retry_after) if retry_after else None,
            )

    # ---- Emails ----
    def send_email(self, from_: str, to: List[str], idempotency_key: str = None, **kwargs) -> Dict[str, Any]:
        body = {"from": from_, "to": to, **kwargs}
        return self._request("POST", "/emails", json=body, idempotency_key=idempotency_key)

    def send_batch(self, emails: List[Dict[str, Any]]) -> Dict[str, Any]:
        return self._request("POST", "/emails/batch", json={"emails": emails})

    def get_email(self, email_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/emails/{email_id}")

    def list_emails(self, limit: int = None, cursor: str = None, status: str = None) -> Dict[str, Any]:
        return self._request("GET", "/emails", params={"limit": limit, "cursor": cursor, "status": status})

    # ---- Events ----
    def list_events(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/events", params={"limit": limit, "cursor": cursor})

    # ---- Domains ----
    def create_domain(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/domains", json=body)

    def get_domain(self, domain_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/domains/{domain_id}")

    def list_domains(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/domains", params={"limit": limit, "cursor": cursor})

    def delete_domain(self, domain_id: str) -> None:
        return self._request("DELETE", f"/domains/{domain_id}")

    def verify_domain(self, domain_id: str) -> Dict[str, Any]:
        return self._request("POST", f"/domains/{domain_id}/verify")

    def get_domain_dkim(self, domain_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/domains/{domain_id}/dkim")

    def create_domain_dkim(self, domain_id: str) -> Dict[str, Any]:
        return self._request("POST", f"/domains/{domain_id}/dkim")

    def verify_domain_dkim(self, domain_id: str) -> Dict[str, Any]:
        return self._request("POST", f"/domains/{domain_id}/dkim/verify")

    def get_domain_spf(self, domain_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/domains/{domain_id}/spf")

    def verify_domain_spf(self, domain_id: str) -> Dict[str, Any]:
        return self._request("POST", f"/domains/{domain_id}/spf/verify")

    def get_domain_dmarc(self, domain_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/domains/{domain_id}/dmarc")

    def verify_domain_dmarc(self, domain_id: str) -> Dict[str, Any]:
        return self._request("POST", f"/domains/{domain_id}/dmarc/verify")

    def get_domain_bimi(self, domain_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/domains/{domain_id}/bimi")

    def verify_domain_bimi(self, domain_id: str, validate_assets: bool = False) -> Dict[str, Any]:
        return self._request("POST", f"/domains/{domain_id}/bimi/verify", params={"validate_assets": "true" if validate_assets else None})

    # ---- Templates ----
    def create_template(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/templates", json=body)

    def get_template(self, template_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/templates/{template_id}")

    def update_template(self, template_id: str, **body) -> Dict[str, Any]:
        return self._request("PATCH", f"/templates/{template_id}", json=body)

    def delete_template(self, template_id: str) -> None:
        return self._request("DELETE", f"/templates/{template_id}")

    def list_templates(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/templates", params={"limit": limit, "cursor": cursor})

    # ---- Contacts ----
    def create_contact(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/contacts", json=body)

    def get_contact(self, contact_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/contacts/{contact_id}")

    def update_contact(self, contact_id: str, **body) -> Dict[str, Any]:
        return self._request("PATCH", f"/contacts/{contact_id}", json=body)

    def delete_contact(self, contact_id: str) -> None:
        return self._request("DELETE", f"/contacts/{contact_id}")

    def list_contacts(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/contacts", params={"limit": limit, "cursor": cursor})

    # ---- Audiences ----
    def create_audience(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/audiences", json=body)

    def get_audience(self, audience_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/audiences/{audience_id}")

    def update_audience(self, audience_id: str, **body) -> Dict[str, Any]:
        return self._request("PATCH", f"/audiences/{audience_id}", json=body)

    def delete_audience(self, audience_id: str) -> None:
        return self._request("DELETE", f"/audiences/{audience_id}")

    def list_audiences(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/audiences", params={"limit": limit, "cursor": cursor})

    def add_audience_contact(self, audience_id: str, **body) -> Dict[str, Any]:
        return self._request("POST", f"/audiences/{audience_id}/contacts", json=body)

    def remove_audience_contact(self, audience_id: str, contact_id: str) -> None:
        return self._request("DELETE", f"/audiences/{audience_id}/contacts/{contact_id}")

    def list_audience_contacts(self, audience_id: str, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", f"/audiences/{audience_id}/contacts", params={"limit": limit, "cursor": cursor})

    # ---- Broadcasts ----
    def create_broadcast(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/broadcasts", json=body)

    def get_broadcast(self, broadcast_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/broadcasts/{broadcast_id}")

    def list_broadcasts(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/broadcasts", params={"limit": limit, "cursor": cursor})

    def list_broadcast_recipients(self, broadcast_id: str, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", f"/broadcasts/{broadcast_id}/recipients", params={"limit": limit, "cursor": cursor})

    # ---- Analytics ----
    def analytics_overview(self, from_: str, to: str) -> Dict[str, Any]:
        return self._request("GET", "/analytics/overview", params={"from": from_, "to": to})

    def analytics_timeseries(self, from_: str, to: str, interval: str) -> Dict[str, Any]:
        return self._request("GET", "/analytics/timeseries", params={"from": from_, "to": to, "interval": interval})

    def analytics_broadcast(self, broadcast_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/analytics/broadcasts/{broadcast_id}")

    # ---- Suppressions ----
    def create_suppression(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/suppressions", json=body)

    def delete_suppression(self, suppression_id: str) -> None:
        return self._request("DELETE", f"/suppressions/{suppression_id}")

    def list_suppressions(self, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", "/suppressions", params={"limit": limit, "cursor": cursor})

    # ---- Webhooks ----
    def create_webhook(self, **body) -> Dict[str, Any]:
        return self._request("POST", "/webhooks", json=body)

    def get_webhook(self, webhook_id: str) -> Dict[str, Any]:
        return self._request("GET", f"/webhooks/{webhook_id}")

    def delete_webhook(self, webhook_id: str) -> None:
        return self._request("DELETE", f"/webhooks/{webhook_id}")

    def list_webhooks(self) -> Dict[str, Any]:
        return self._request("GET", "/webhooks")

    def rotate_webhook_secret(self, webhook_id: str) -> Dict[str, Any]:
        return self._request("POST", f"/webhooks/{webhook_id}/rotate-secret")

    def list_webhook_deliveries(self, webhook_id: str, limit: int = None, cursor: str = None) -> Dict[str, Any]:
        return self._request("GET", f"/webhooks/{webhook_id}/deliveries", params={"limit": limit, "cursor": cursor})
