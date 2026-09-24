<?php

namespace MailX;

class MailXException extends \RuntimeException
{
    public int $status;
    public string $type;
    public string $apiCode;
    public ?string $requestId;
    public ?int $retryAfter;

    public function __construct(int $status, string $type, string $apiCode, string $message, ?string $requestId = null, ?int $retryAfter = null)
    {
        parent::__construct($message);
        $this->status = $status;
        $this->type = $type;
        $this->apiCode = $apiCode;
        $this->requestId = $requestId;
        $this->retryAfter = $retryAfter;
    }
}
