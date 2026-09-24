<?php

namespace MailX;

interface Transport
{
    /**
     * @param array<string,string> $headers
     * @return array{0:int,1:array<string,string>,2:string} [status, headers, body]
     */
    public function send(string $method, string $url, array $headers, ?string $body): array;
}

class CurlTransport implements Transport
{
    public function send(string $method, string $url, array $headers, ?string $body): array
    {
        $ch = curl_init($url);
        $headerLines = [];
        foreach ($headers as $key => $value) {
            $headerLines[] = "$key: $value";
        }

        curl_setopt_array($ch, [
            CURLOPT_CUSTOMREQUEST => $method,
            CURLOPT_HTTPHEADER => $headerLines,
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_HEADER => true,
            CURLOPT_TIMEOUT => 30,
        ]);
        if ($body !== null) {
            curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
        }

        $response = curl_exec($ch);
        if ($response === false) {
            $error = curl_error($ch);
            curl_close($ch);
            throw new \RuntimeException("mailx: request failed: $error");
        }

        $status = curl_getinfo($ch, CURLINFO_HTTP_CODE);
        $headerSize = curl_getinfo($ch, CURLINFO_HEADER_SIZE);
        curl_close($ch);

        $rawHeaders = substr($response, 0, $headerSize);
        $responseBody = substr($response, $headerSize);

        $respHeaders = [];
        foreach (explode("\r\n", $rawHeaders) as $line) {
            if (str_contains($line, ':')) {
                [$k, $v] = explode(':', $line, 2);
                $respHeaders[strtolower(trim($k))] = trim($v);
            }
        }

        return [$status, $respHeaders, $responseBody];
    }
}
