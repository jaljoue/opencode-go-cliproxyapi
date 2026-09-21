//go:build live

package tests_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestLiveIssue5_DeveloperRoleInChatCompletions reproduces Issue #5 where
// requests containing messages with role "developer" sent to DeepSeek models
// fail if the role is forwarded verbatim instead of normalized to "system".
func TestLiveIssue5_DeveloperRoleInChatCompletions(t *testing.T) {
	client := &http.Client{Timeout: 30 * time.Second}
	payload := map[string]any{
		"model": modelID("deepseek-v4.1-flash"),
		"messages": []map[string]string{
			{"role": "developer", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Say hello"},
		},
		"max_tokens": 16,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, cpaHost+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cpaKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200, got %d: %s", resp.StatusCode, string(body))
	}
}
