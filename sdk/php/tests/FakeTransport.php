<?php

namespace MailX\Tests;

use MailX\Transport;

class FakeTransport implements Transport
{
    /** @var array<array{0:int,1:array<string,string>,2:string}> */
    private array $responses;
    public array $requests = [];

    public function __construct(array $responses)
    {
        $this->responses = $responses;
    }

    public function send(string $method, string $url, array $headers, ?string $body): array
    {
        $this->requests[] = [$method, $url, $headers, $body];
        $response = array_shift($this->responses);
        if ($response === null) {
            throw new \RuntimeException('no more fake responses queued');
        }
        return $response;
    }
}
