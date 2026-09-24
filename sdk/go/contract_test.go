package mailx

import (
	"context"
	"os"
	"testing"
)

func TestContractSendEmail(t *testing.T) {
	baseURL := os.Getenv("MAILX_SDK_TEST_BASE_URL")
	apiKey := os.Getenv("MAILX_SDK_TEST_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("MAILX_SDK_TEST_BASE_URL/MAILX_SDK_TEST_API_KEY not set, skipping live contract test")
	}
	c := NewClient(apiKey, WithBaseURL(baseURL))
	_, err := c.SendEmail(context.Background(), SendEmailRequest{
		From:    "sdk-test@example.com",
		To:      []string{"sdk-test-dest@example.com"},
		Subject: "Go SDK contract test",
		Text:    "hello",
	})
	if err != nil {
		t.Fatalf("live send failed: %v", err)
	}
}
