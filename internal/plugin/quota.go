package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/resources"
)

type quotaWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resets_at"`
}

type quotaUsage struct {
	Rolling quotaWindow `json:"rolling"`
	Weekly  quotaWindow `json:"weekly"`
	Monthly quotaWindow `json:"monthly"`
}

type quotaCard struct {
	KeyID    string      `json:"key_id"`
	Label    string      `json:"label"`
	PlanType string      `json:"plan_type,omitempty"`
	Email    string      `json:"email,omitempty"`
	Usage    *quotaUsage `json:"usage,omitempty"`
}

type quotaRequest struct {
	KeyID string `json:"key_id"`
}

type usageResponse struct {
	Usage struct {
		Rolling quotaUpstreamWindow `json:"rolling"`
		Weekly  quotaUpstreamWindow `json:"weekly"`
		Monthly quotaUpstreamWindow `json:"monthly"`
	} `json:"usage"`
}

type quotaUpstreamWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

type goStatus struct {
	SubscriberUserID string `json:"subscriberUserId"`
	Product          string `json:"product"`
}

type budgetMember struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

func quotaIdentity(key string) (id, label string) {
	digest := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(digest[:])
	return "opencode-go-key-" + hash, "OpenCode Go credential " + hash[:12]
}

func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if req.Method == http.MethodGet && req.Path == "/v0/resource/plugins/"+pluginName+"/quota" {
		return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: []byte(resources.QuotaPage)}, nil
	}
	if req.Method != http.MethodPost || req.Path != "/v0/management/plugins/"+pluginName+"/quota-usage" {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)}, nil
	}
	var body quotaRequest
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":"invalid request"}`)}, nil
		}
	}
	m.mu.RLock()
	baseURL, consoleURL, timeout := m.cfg.BaseURL, m.cfg.ConsoleURL, m.cfg.RequestTimeout
	keys := append([]config.APIKey(nil), m.cfg.APIKeys...)
	m.mu.RUnlock()
	if body.KeyID == "" {
		cards := make([]quotaCard, 0, len(keys))
		for _, key := range keys {
			id, label := quotaIdentity(key.Value)
			cards = append(cards, quotaCard{KeyID: id, Label: label})
		}
		return quotaJSON(quotaList{Cards: cards})
	}
	for _, key := range keys {
		id, label := quotaIdentity(key.Value)
		if id != body.KeyID {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		usage, err := fetchQuota(ctx, m.bridge, baseURL, key.Value)
		if err != nil {
			return pluginapi.ManagementResponse{StatusCode: http.StatusBadGateway, Body: []byte(`{"error":"quota refresh failed"}`)}, nil
		}
		card := quotaCard{KeyID: id, Label: label, Usage: &usage}
		card.PlanType, card.Email = fetchAccount(ctx, m.bridge, consoleURL, key.Value)
		if card.Email != "" {
			if err := m.persistEmail(ctx, id, card.Email); err != nil {
				_ = m.bridge.Log("warn", "quota email was not saved to the auth record", map[string]any{"error": err.Error()})
			}
		}
		return quotaJSON(card)
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"unknown quota key"}`)}, nil
}

type quotaList struct {
	Cards []quotaCard `json:"cards"`
}

func quotaJSON(v any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{}, fmt.Errorf("quota response encoding failed")
	}
	return pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
}

func fetchQuota(ctx context.Context, bridge *HostBridge, baseURL, key string) (quotaUsage, error) {
	if bridge == nil {
		return quotaUsage{}, fmt.Errorf("quota bridge unavailable")
	}
	var decoded usageResponse
	if err := getJSON(ctx, bridge, strings.TrimRight(baseURL, "/")+"/usage", key, &decoded); err != nil {
		return quotaUsage{}, err
	}
	return quotaUsage{
		Rolling: quotaWindow{Status: decoded.Usage.Rolling.Status, Percent: decoded.Usage.Rolling.Percent, ResetsAt: decoded.Usage.Rolling.ResetsAt},
		Weekly:  quotaWindow{Status: decoded.Usage.Weekly.Status, Percent: decoded.Usage.Weekly.Percent, ResetsAt: decoded.Usage.Weekly.ResetsAt},
		Monthly: quotaWindow{Status: decoded.Usage.Monthly.Status, Percent: decoded.Usage.Monthly.Percent, ResetsAt: decoded.Usage.Monthly.ResetsAt},
	}, nil
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
func (m *Manager) persistEmail(ctx context.Context, id, email string) error {
	entries, err := m.bridge.AuthList(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.ID != id && entry.Name != id+".json" {
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
