package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

// quotaDisplayName labels the provider and its quota group in Management Center.
const quotaDisplayName = "OpenCode Go"

// quotaWindows maps OpenCode's usage windows to normalized bucket windows,
// in display order. "5h" and "weekly" are window names Management Center
// already recognizes.
var quotaWindows = []struct{ upstream, window, description string }{
	{"rolling", "5h", "5-hour limit"},
	{"weekly", "weekly", "Weekly limit"},
	{"monthly", "monthly", "Monthly limit"},
}

type quotaUpstreamWindow struct {
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

type goStatus struct {
	SubscriberUserID string `json:"subscriberUserId"`
	Product          string `json:"product"`
}

type budgetMember struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

func quotaDescribe() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{ProviderID}, DisplayName: quotaDisplayName}
}

// FetchQuota reads usage for the credential CPA selected. It never falls back
// to a configured key, so a quota reading always belongs to that credential.
func (m *Manager) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.Provider != "" && req.Provider != ProviderID {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("unsupported quota provider")
	}
	key := quotaKey(req)
	if key == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota credential has no API key")
	}
	if m.bridge == nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota bridge unavailable")
	}
	m.mu.RLock()
	baseURL, consoleURL, timeout := m.cfg.BaseURL, m.cfg.ConsoleURL, m.cfg.RequestTimeout
	m.mu.RUnlock()
	if timeout <= 0 {
		timeout = config.DefaultRequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var usage struct {
		Usage map[string]quotaUpstreamWindow `json:"usage"`
	}
	if err := getJSON(ctx, m.bridge, strings.TrimRight(baseURL, "/")+"/usage", key, &usage); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	resp, err := normalizeQuota(usage.Usage)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	plan, email := fetchAccount(ctx, m.bridge, consoleURL, key)
	if plan != "" {
		resp.Subscription = &pluginapi.QuotaSubscription{Plan: plan, TierName: planLabel(plan)}
	}
	if email != "" {
		if err := m.persistEmail(ctx, req.AuthIndex, req.AuthID, email); err != nil {
			_ = m.bridge.Log("warn", "quota email was not saved to the auth record", map[string]any{"error": err.Error()})
		}
	}
	return resp, nil
}

// quotaKey reads the API key from the attributes set by ParseAuth, falling
// back to the stored auth record fields the host passes as metadata.
func quotaKey(req pluginapi.QuotaFetchRequest) string {
	if key := strings.TrimSpace(req.Attributes["api_key"]); key != "" {
		return key
	}
	key, _ := req.Metadata["api_key"].(string)
	return strings.TrimSpace(key)
}

// normalizeQuota converts OpenCode's used percentages into remaining
// fractions. Missing windows are omitted; an invalid percentage fails the
// reading rather than reporting a made-up value.
func normalizeQuota(usage map[string]quotaUpstreamWindow) (pluginapi.QuotaFetchResponse, error) {
	buckets := make([]pluginapi.QuotaBucket, 0, len(quotaWindows))
	for _, w := range quotaWindows {
		window, ok := usage[w.upstream]
		if !ok || window.Percent == nil {
			continue
		}
		percent := *window.Percent
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
			return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota percentage invalid")
		}
		bucket := pluginapi.QuotaBucket{Window: w.window, RemainingFraction: 1 - percent/100, Description: w.description}
		if reset, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(window.ResetsAt)); err == nil {
			bucket.ResetTime = reset.UTC().Format(time.RFC3339)
		}
		buckets = append(buckets, bucket)
	}
	if len(buckets) == 0 {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota response has no readings")
	}
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: quotaDisplayName, Buckets: buckets}}}, nil
}

// planLabel turns an OpenCode product id such as "go-plus" into "Go Plus".
func planLabel(plan string) string {
	words := strings.FieldsFunc(plan, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// fetchAccount reads the Go plan and the subscriber's email from the OpenCode
// Console. Both endpoints reject inference-only keys, so failures leave the
// fields empty instead of failing the usage refresh.
func fetchAccount(ctx context.Context, bridge *HostBridge, consoleURL, key string) (plan, email string) {
	var status goStatus
	if getJSON(ctx, bridge, consoleURL+"/api/go/status", key, &status) != nil {
		status = goStatus{}
	}
	var members []budgetMember
	if getJSON(ctx, bridge, consoleURL+"/api/v1/budgets/members", key, &members) == nil {
		email = subscriberEmail(members, status.SubscriberUserID)
	}
	return strings.TrimSpace(status.Product), email
}

// subscriberEmail finds the Go subscriber among the workspace members. When
// the subscriber is unknown, a single-member workspace is unambiguous.
func subscriberEmail(members []budgetMember, subscriberID string) string {
	if subscriberID != "" {
		for _, member := range members {
			if member.UserID == subscriberID {
				return strings.TrimSpace(member.Email)
			}
		}
	}
	if len(members) == 1 {
		return strings.TrimSpace(members[0].Email)
	}
	return ""
}

func getJSON(ctx context.Context, bridge *HostBridge, endpoint, key string, out any) error {
	resp, err := bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: endpoint, Headers: http.Header{"Authorization": []string{"Bearer " + key}, "Accept": []string{"application/json"}}})
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("quota upstream request failed")
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("quota response invalid")
	}
	return nil
}

// persistEmail stores the subscriber email in the credential's auth file so
// CPA lists it as the account, as it does for Claude and Codex. The file is
// rewritten only when the listed email differs, and its other fields are kept.
func (m *Manager) persistEmail(ctx context.Context, authIndex, id, email string) error {
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		matched := authIndex != "" && entry.AuthIndex == authIndex
		if !matched && id != "" {
			matched = entry.ID == id || entry.Name == id+".json"
		}
		if !matched {
			continue
		}
		if entry.Disabled || strings.TrimSpace(entry.Email) == email {
			return nil
		}
		record, err := m.bridge.AuthGet(ctx, entry.AuthIndex)
		if err != nil {
			return err
		}
		var fields map[string]any
		if json.Unmarshal(record.JSON, &fields) != nil || fields == nil {
			return fmt.Errorf("auth record is not a JSON object")
		}
		fields["email"] = email
		updated, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("auth record encoding failed")
		}
		return m.bridge.AuthSave(ctx, pluginapi.HostAuthSaveRequest{Name: entry.Name, JSON: updated})
	}
	return nil
}
