import { test } from "node:test";
import { MailXClient } from "../dist/index.js";

const baseUrl = process.env.MAILX_SDK_TEST_BASE_URL;
const apiKey = process.env.MAILX_SDK_TEST_API_KEY;

test("send an email against a real running server", { skip: !baseUrl || !apiKey }, async (t) => {
  const client = new MailXClient({ apiKey, baseUrl });
  const email = await client.sendEmail({
    from: process.env.MAILX_SDK_TEST_FROM ?? "a@example.com",
    to: ["bob@example.com"],
    subject: "sdk contract test",
    text: "hello",
  });
  t.assert.ok(email.id);
  t.assert.equal(email.status, "queued");
});
