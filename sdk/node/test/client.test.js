import { test } from "node:test";
import assert from "node:assert/strict";
import { MailXClient, MailXError } from "../dist/index.js";

function fakeFetch(handler) {
  return async (url, init) => handler(url, init);
}

test("sendEmail posts to /emails with auth header", async () => {
  let captured;
  const client = new MailXClient({
    apiKey: "mx_test",
    baseUrl: "https://x/v1",
    fetchImpl: fakeFetch((url, init) => {
      captured = { url, init };
      return new Response(JSON.stringify({ id: "m1", from: "a@x.com", to: ["b@x.com"], subject: "s", status: "queued", created_at: "now" }), {
        status: 202,
        headers: { "content-type": "application/json" },
      });
    }),
  });
  const email = await client.sendEmail({ from: "a@x.com", to: ["b@x.com"], subject: "s", text: "t" });
  assert.equal(email.id, "m1");
  assert.equal(captured.url, "https://x/v1/emails");
  assert.equal(captured.init.headers.Authorization, "Bearer mx_test");
});

test("retries on 429 honoring Retry-After then succeeds", async () => {
  let calls = 0;
  const client = new MailXClient({
    apiKey: "mx_test",
    baseUrl: "https://x/v1",
    fetchImpl: fakeFetch(() => {
      calls++;
      if (calls === 1) {
        return new Response(JSON.stringify({ error: { type: "rate_limited", code: "x", message: "m" } }), {
          status: 429,
          headers: { "Retry-After": "0" },
        });
      }
      return new Response(JSON.stringify({ id: "m2" }), { status: 202 });
    }),
  });
  const res = await client.getEmail("m2");
  assert.equal(res.id, "m2");
  assert.equal(calls, 2);
});

test("throws MailXError on non-retryable 4xx", async () => {
  const client = new MailXClient({
    apiKey: "mx_test",
    baseUrl: "https://x/v1",
    fetchImpl: fakeFetch(
      () =>
        new Response(JSON.stringify({ error: { type: "validation_error", code: "missing_from", message: "from is required" } }), {
          status: 422,
        })
    ),
  });
  await assert.rejects(
    () => client.sendEmail({ from: "", to: [] }),
    (err) => err instanceof MailXError && err.code === "missing_from" && err.status === 422
  );
});
