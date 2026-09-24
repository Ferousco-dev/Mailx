<?php

namespace MailX;

class Client
{
    private string $apiKey;
    private string $baseUrl;
    private int $maxRetries;
    private Transport $transport;

    public function __construct(string $apiKey, string $baseUrl = 'https://api.mailx.dev', int $maxRetries = 3, ?Transport $transport = null)
    {
        $this->apiKey = $apiKey;
        $this->baseUrl = rtrim($baseUrl, '/');
        $this->maxRetries = $maxRetries;
        $this->transport = $transport ?? new CurlTransport();
    }

    /**
     * @param array<string,mixed>|null $body
     * @return array<string,mixed>
     */
    private function request(string $method, string $path, ?array $body = null): array
    {
        $payload = $body !== null ? json_encode($body) : null;
        $headers = ['Authorization' => 'Bearer ' . $this->apiKey];
        if ($payload !== null) {
            $headers['Content-Type'] = 'application/json';
        }

        $attempt = 0;
        while (true) {
            if ($attempt > 0) {
                usleep($this->backoffMicros($attempt));
            }

            [$status, $respHeaders, $respBody] = $this->transport->send($method, $this->baseUrl . $path, $headers, $payload);

            if ($status >= 200 && $status < 300) {
                return $respBody === '' ? [] : json_decode($respBody, true);
            }

            $decoded = json_decode($respBody, true) ?? [];
            $retryAfter = isset($respHeaders['retry-after']) ? (int) $respHeaders['retry-after'] : null;
            $exception = new MailXException(
                $status,
                $decoded['type'] ?? '',
                $decoded['code'] ?? '',
                $decoded['message'] ?? "request failed with status $status",
                $decoded['request_id'] ?? null,
                $retryAfter
            );

            $retryable = $status === 429 || $status >= 500;
            if ($retryable && $attempt < $this->maxRetries) {
                $attempt++;
                continue;
            }
            throw $exception;
        }
    }

    private function backoffMicros(int $attempt): int
    {
        $baseMs = min(1000 * (2 ** $attempt), 10000);
        $jitterMs = random_int(0, 250);
        return ($baseMs + $jitterMs) * 1000;
    }

    /** @param array<string,mixed> $email */
    public function sendEmail(array $email): array
    {
        return $this->request('POST', '/v1/emails', $email);
    }

    /** @param array<string,mixed> $batch */
    public function sendBatch(array $batch): array
    {
        return $this->request('POST', '/v1/emails/batch', $batch);
    }

    public function getEmail(string $id): array
    {
        return $this->request('GET', '/v1/emails/' . $id);
    }

    public function listEmails(string $query = ''): array
    {
        return $this->request('GET', '/v1/emails' . $query);
    }

    public function listEvents(string $emailId): array
    {
        return $this->request('GET', '/v1/emails/' . $emailId . '/events');
    }

    public function createDomain(array $body): array { return $this->request('POST', '/v1/domains', $body); }
    public function getDomain(string $id): array { return $this->request('GET', '/v1/domains/' . $id); }
    public function listDomains(): array { return $this->request('GET', '/v1/domains'); }
    public function deleteDomain(string $id): array { return $this->request('DELETE', '/v1/domains/' . $id); }
    public function verifyDkim(string $domainId): array { return $this->request('POST', '/v1/domains/' . $domainId . '/dkim/verify'); }
    public function getSpf(string $domainId): array { return $this->request('GET', '/v1/domains/' . $domainId . '/spf'); }
    public function getDmarc(string $domainId): array { return $this->request('GET', '/v1/domains/' . $domainId . '/dmarc'); }
    public function setBimi(string $domainId, array $body): array { return $this->request('PUT', '/v1/domains/' . $domainId . '/bimi', $body); }

    public function createTemplate(array $body): array { return $this->request('POST', '/v1/templates', $body); }
    public function getTemplate(string $id): array { return $this->request('GET', '/v1/templates/' . $id); }
    public function listTemplates(): array { return $this->request('GET', '/v1/templates'); }
    public function updateTemplate(string $id, array $body): array { return $this->request('PATCH', '/v1/templates/' . $id, $body); }
    public function deleteTemplate(string $id): array { return $this->request('DELETE', '/v1/templates/' . $id); }

    public function createContact(array $body): array { return $this->request('POST', '/v1/contacts', $body); }
    public function getContact(string $id): array { return $this->request('GET', '/v1/contacts/' . $id); }
    public function listContacts(): array { return $this->request('GET', '/v1/contacts'); }
    public function updateContact(string $id, array $body): array { return $this->request('PATCH', '/v1/contacts/' . $id, $body); }
    public function deleteContact(string $id): array { return $this->request('DELETE', '/v1/contacts/' . $id); }

    public function createAudience(array $body): array { return $this->request('POST', '/v1/audiences', $body); }
    public function getAudience(string $id): array { return $this->request('GET', '/v1/audiences/' . $id); }
    public function listAudiences(): array { return $this->request('GET', '/v1/audiences'); }
    public function deleteAudience(string $id): array { return $this->request('DELETE', '/v1/audiences/' . $id); }

    public function createBroadcast(array $body): array { return $this->request('POST', '/v1/broadcasts', $body); }
    public function getBroadcast(string $id): array { return $this->request('GET', '/v1/broadcasts/' . $id); }
    public function listBroadcasts(): array { return $this->request('GET', '/v1/broadcasts'); }
    public function sendBroadcast(string $id): array { return $this->request('POST', '/v1/broadcasts/' . $id . '/send'); }

    public function getAnalytics(string $query = ''): array { return $this->request('GET', '/v1/analytics' . $query); }

    public function listSuppressions(): array { return $this->request('GET', '/v1/suppressions'); }
    public function createSuppression(array $body): array { return $this->request('POST', '/v1/suppressions', $body); }
    public function deleteSuppression(string $id): array { return $this->request('DELETE', '/v1/suppressions/' . $id); }

    public function createWebhook(array $body): array { return $this->request('POST', '/v1/webhooks', $body); }
    public function getWebhook(string $id): array { return $this->request('GET', '/v1/webhooks/' . $id); }
    public function listWebhooks(): array { return $this->request('GET', '/v1/webhooks'); }
    public function updateWebhook(string $id, array $body): array { return $this->request('PATCH', '/v1/webhooks/' . $id, $body); }
    public function deleteWebhook(string $id): array { return $this->request('DELETE', '/v1/webhooks/' . $id); }
}
