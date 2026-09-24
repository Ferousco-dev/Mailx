import sys
import os
import unittest
from unittest.mock import MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from mailx_sdk import MailXClient, MailXError


def fake_response(status_code, json_body=None, headers=None, ok=None):
    resp = MagicMock()
    resp.status_code = status_code
    resp.ok = ok if ok is not None else 200 <= status_code < 300
    resp.headers = headers or {}
    resp.content = b"1" if json_body is not None else b""
    resp.json.return_value = json_body or {}
    resp.reason = "error"
    return resp


class TestMailXClient(unittest.TestCase):
    def test_send_email_success(self):
        session = MagicMock()
        session.request.return_value = fake_response(
            202, {"id": "m1", "from": "a@x.com", "to": ["b@x.com"], "subject": "s", "status": "queued"}
        )
        client = MailXClient(api_key="mx_test", base_url="https://x/v1", session=session)
        email = client.send_email("a@x.com", ["b@x.com"], subject="s", text="t")
        self.assertEqual(email["id"], "m1")
        args, kwargs = session.request.call_args
        self.assertEqual(kwargs["headers"]["Authorization"], "Bearer mx_test")
        self.assertEqual(args[1], "https://x/v1/emails")

    def test_retries_on_429_then_succeeds(self):
        session = MagicMock()
        session.request.side_effect = [
            fake_response(429, {"error": {"type": "rate_limited", "code": "x", "message": "m"}}, headers={"Retry-After": "0"}),
            fake_response(202, {"id": "m2"}),
        ]
        client = MailXClient(api_key="mx_test", base_url="https://x/v1", session=session)
        result = client.get_email("m2")
        self.assertEqual(result["id"], "m2")
        self.assertEqual(session.request.call_count, 2)

    def test_raises_mailx_error_on_422(self):
        session = MagicMock()
        session.request.return_value = fake_response(
            422, {"error": {"type": "validation_error", "code": "missing_from", "message": "from is required"}}
        )
        client = MailXClient(api_key="mx_test", base_url="https://x/v1", session=session)
        with self.assertRaises(MailXError) as ctx:
            client.send_email("", [])
        self.assertEqual(ctx.exception.status, 422)
        self.assertEqual(ctx.exception.code, "missing_from")

    def test_requires_api_key(self):
        with self.assertRaises(ValueError):
            MailXClient(api_key="")


if __name__ == "__main__":
    unittest.main()
