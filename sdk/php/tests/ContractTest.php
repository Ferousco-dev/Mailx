<?php

namespace MailX\Tests;

use MailX\Client;
use PHPUnit\Framework\TestCase;

class ContractTest extends TestCase
{
    public function testLiveSendEmail(): void
    {
        $baseUrl = getenv('MAILX_SDK_TEST_BASE_URL');
        $apiKey = getenv('MAILX_SDK_TEST_API_KEY');
        if (!$baseUrl || !$apiKey) {
            $this->markTestSkipped('MAILX_SDK_TEST_BASE_URL/MAILX_SDK_TEST_API_KEY not set');
        }

        $client = new Client($apiKey, $baseUrl);
        $result = $client->sendEmail([
            'from' => 'sdk-test@example.com',
            'to' => ['sdk-test-dest@example.com'],
            'subject' => 'PHP SDK contract test',
            'text' => 'hello',
        ]);

        $this->assertArrayHasKey('id', $result);
    }
}
