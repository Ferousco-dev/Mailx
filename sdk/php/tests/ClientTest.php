<?php

namespace MailX\Tests;

use MailX\Client;
use MailX\MailXException;
use PHPUnit\Framework\TestCase;

class ClientTest extends TestCase
{
    public function testSendEmailSuccess(): void
    {
        $transport = new FakeTransport([
            [200, [], json_encode(['id' => 'em_1', 'status' => 'queued'])],
        ]);
        $client = new Client('test-key', 'https://api.mailx.dev', 3, $transport);

        $result = $client->sendEmail(['from' => 'a@b.com', 'to' => ['c@d.com']]);

        $this->assertSame('em_1', $result['id']);
        $this->assertSame('Bearer test-key', $transport->requests[0][2]['Authorization']);
    }

    public function testRetriesOn429ThenSucceeds(): void
    {
        $transport = new FakeTransport([
            [429, ['retry-after' => '0'], json_encode(['type' => 'rate_limited', 'code' => 'too_many_requests', 'message' => 'slow down'])],
            [200, [], json_encode(['id' => 'em_2'])],
        ]);
        $client = new Client('test-key', 'https://api.mailx.dev', 3, $transport);

        $result = $client->sendEmail(['from' => 'a@b.com', 'to' => ['c@d.com']]);

        $this->assertSame('em_2', $result['id']);
        $this->assertCount(2, $transport->requests);
    }

    public function testNonRetryableErrorThrows(): void
    {
        $transport = new FakeTransport([
            [400, [], json_encode(['type' => 'invalid_request', 'code' => 'missing_field', 'message' => 'from is required'])],
        ]);
        $client = new Client('test-key', 'https://api.mailx.dev', 3, $transport);

        $this->expectException(MailXException::class);
        $client->sendEmail([]);
    }

    public function testGetEmail(): void
    {
        $transport = new FakeTransport([
            [200, [], json_encode(['id' => 'em_3', 'status' => 'delivered'])],
        ]);
        $client = new Client('test-key', 'https://api.mailx.dev', 3, $transport);

        $result = $client->getEmail('em_3');

        $this->assertSame('delivered', $result['status']);
        $this->assertStringContainsString('/v1/emails/em_3', $transport->requests[0][1]);
    }
}
