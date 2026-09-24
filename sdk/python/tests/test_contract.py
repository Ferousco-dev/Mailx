import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from mailx_sdk import MailXClient

BASE_URL = os.environ.get("MAILX_SDK_TEST_BASE_URL")
API_KEY = os.environ.get("MAILX_SDK_TEST_API_KEY")


@unittest.skipUnless(BASE_URL and API_KEY, "MAILX_SDK_TEST_BASE_URL/MAILX_SDK_TEST_API_KEY not set")
class TestContract(unittest.TestCase):
    def test_send_email_against_real_server(self):
        client = MailXClient(api_key=API_KEY, base_url=BASE_URL)
        email = client.send_email(
            os.environ.get("MAILX_SDK_TEST_FROM", "a@example.com"),
            ["bob@example.com"],
            subject="sdk contract test",
            text="hello",
        )
        self.assertTrue(email["id"])
        self.assertEqual(email["status"], "queued")


if __name__ == "__main__":
    unittest.main()
