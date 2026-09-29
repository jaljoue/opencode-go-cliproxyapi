package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
	"opencode-go-cliproxyapi/resources"
)

func TestManagementRegistration(t *testing.T) {
	m := NewManager(nil)
	var got struct {
		Routes    []struct{ Method, Path string }            `json:"routes"`
		Resources []struct{ Path, Menu, Description string } `json:"resources"`
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`)), &got)
	if len(got.Routes) != 1 || got.Routes[0].Method != http.MethodPost || got.Routes[0].Path != "/plugins/"+pluginName+"/quota-usage" {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if len(got.Resources) != 1 || got.Resources[0].Path != "/quota" || got.Resources[0].Menu != "OpenCode Go Quota" {
		t.Fatalf("resources = %+v", got.Resources)
	}
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML)), &registration)
	if !registration.Capabilities.ManagementAPI {
		t.Fatal("registration did not advertise management_api")
	}
}

func TestQuotaListDoesNotCallHost(t *testing.T) {
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: "quota-key-a"}, {Value: "quota-key-b"}}}
	var got quotaList
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cards) != 2 || got.Cards[0].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-a") || got.Cards[1].Label != "OpenCode Go credential "+mustHashPrefix("quota-key-b") {
		t.Fatalf("cards = %+v", got.Cards)
	}
	if strings.Contains(string(resp.Body), "quota-key-") || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("list leaked key or called upstream: %s calls=%v", resp.Body, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
}

const (
	quotaStatusURL    = "https://console.test/console/api/go/status"
	quotaMembersURL   = "https://console.test/console/api/v1/budgets/members"
	quotaUsageBody    = `{"usage":{"rolling":{"status":"ok","percent":9,"resetsAt":"2026-09-05T12:20:04Z"},"weekly":{"status":"ok","percent":12,"resetsAt":"2026-09-10T00:00:00Z"},"monthly":{"status":"ok","percent":6,"resetsAt":"2026-10-01T00:00:00Z"}}}`
	goPlusStatusBody  = `{"subscriberUserId":"acc_sub","product":"go-plus","renewalProduct":"go-plus","access":{"meters":{"week":{"limitMicroCents":"12000000000","usedMicroCents":"0"}}}}`
	budgetMembersBody = `[{"user_id":"acc_team","email":"teammate@example.com"},{"user_id":"acc_sub","email":"subscriber@example.com","limit_micro_cents":null}]`
)

// quotaFixture serves the usage and Console endpoints for one key plus the
// CPA auth record callbacks for its credential. The Console lives on its own
// host so a request built from base-url cannot reach it.
type quotaFixture struct {
	key      string
	entry    pluginapi.HostAuthFileEntry
	record   string         // stored auth JSON returned by host.auth.get
	statuses map[string]int // non-200 status per URL
}

func newQuotaFixture(key string) quotaFixture {
	id, _ := quotaIdentity(key)
	return quotaFixture{
		key:    key,
		entry:  pluginapi.HostAuthFileEntry{ID: id, AuthIndex: "7", Name: id + ".json"},
		record: `{"type":"opencode-go","id":"` + id + `","api_key":"` + key + `","priority":3,"note":"keep"}`,
	}
}

func (q quotaFixture) respond(t *testing.T) func(string, []byte) ([]byte, error) {
	bodies := map[string]string{
		"https://quota.test/v1/usage": quotaUsageBody,
		quotaStatusURL:                goPlusStatusBody,
		quotaMembersURL:               budgetMembersBody,
	}
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{q.entry}}), nil
		case pluginabi.MethodHostAuthGet:
			return hostOK(pluginapi.HostAuthGetResponse{AuthIndex: q.entry.AuthIndex, Name: q.entry.Name, JSON: json.RawMessage(q.record)}), nil
		case pluginabi.MethodHostHTTPDo:
		default:
			return hostOK(map[string]any{}), nil
		}
		var wire struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || wire.Method != http.MethodGet || wire.Headers.Get("Authorization") != "Bearer "+q.key || wire.Headers.Get("Accept") != "application/json" {
			t.Errorf("bad quota request: %s", payload)
			return hostErr("test", "bad request"), nil
		}
		body, ok := bodies[wire.URL]
		if !ok {
			t.Errorf("unexpected quota url %s", wire.URL)
			return hostErr("test", "unrouted"), nil
		}
		if status := q.statuses[wire.URL]; status != 0 {
			return hostOK(pluginapi.HTTPResponse{StatusCode: status, Body: []byte(`{"_tag":"Forbidden"}`)}), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(body)}), nil
	}
}

func quotaManager(key string, responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	f := &fakeCaller{responder: responder}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1/", ConsoleURL: "https://console.test/console", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
	return m, f
}

func refreshQuota(t *testing.T, m *Manager, key string) (pluginapi.ManagementResponse, quotaCard) {
	t.Helper()
	id, _ := quotaIdentity(key)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var card quotaCard
	if err := json.Unmarshal(resp.Body, &card); err != nil {
		t.Fatalf("quota body %s: %v", resp.Body, err)
	}
	return resp, card
}

func TestQuotaRefresh(t *testing.T) {
	const key = "quota-refresh-secret"
	fixture := newQuotaFixture(key)
	m, f := quotaManager(key, fixture.respond(t))
	_, got := refreshQuota(t, m, key)
	if got.Usage == nil || got.Usage.Rolling.Percent != 9 || got.Usage.Weekly.ResetsAt != "2026-09-10T00:00:00Z" || got.Usage.Monthly.Percent != 6 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.PlanType != "go-plus" || got.Email != "subscriber@example.com" {
		t.Fatalf("account = plan %q email %q", got.PlanType, got.Email)
	}
	gets := f.callsOf(pluginabi.MethodHostAuthGet)
	if len(gets) != 1 || !strings.Contains(string(gets[0].payload), `"auth_index":"7"`) {
		t.Fatalf("auth get calls = %v", gets)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 {
		t.Fatalf("auth saves = %d, want 1", len(saves))
	}
	var wire pluginapi.HostAuthSaveRequest
	var saved map[string]any
	if err := json.Unmarshal(saves[0].payload, &wire); err != nil || json.Unmarshal(wire.JSON, &saved) != nil {
		t.Fatalf("auth save payload = %s", saves[0].payload)
	}
	if wire.Name != fixture.entry.Name || saved["email"] != "subscriber@example.com" || saved["api_key"] != key || saved["priority"] != float64(3) || saved["note"] != "keep" || saved["type"] != ProviderID {
		t.Fatalf("saved record %q = %v", wire.Name, saved)
	}
}

func TestQuotaInferenceKeyKeepsUsage(t *testing.T) {
	const key = "quota-inference-secret"
	fixture := newQuotaFixture(key)
	fixture.statuses = map[string]int{quotaStatusURL: http.StatusForbidden, quotaMembersURL: http.StatusForbidden}
	m, f := quotaManager(key, fixture.respond(t))
	resp, got := refreshQuota(t, m, key)
	if resp.StatusCode != 0 || got.Usage == nil || got.Usage.Weekly.Percent != 12 {
		t.Fatalf("refresh = %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "plan_type") || strings.Contains(string(resp.Body), "email") {
		t.Fatalf("inference-only key reported account fields: %s", resp.Body)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthList)) != 0 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatalf("auth record touched without an email: %v", f.recorded())
	}
}

func TestQuotaEmailPersistenceSkipsUnneededWrites(t *testing.T) {
	const key = "quota-persist-secret"
	for name, edit := range map[string]func(*quotaFixture){
		"already listed": func(q *quotaFixture) { q.entry.Email = "subscriber@example.com" },
		"disabled":       func(q *quotaFixture) { q.entry.Disabled = true },
		"no record": func(q *quotaFixture) {
			q.entry.ID, q.entry.Name = "opencode-go-key-other", "opencode-go-key-other.json"
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newQuotaFixture(key)
			edit(&fixture)
			m, f := quotaManager(key, fixture.respond(t))
			if _, got := refreshQuota(t, m, key); got.Email != "subscriber@example.com" {
				t.Fatalf("email = %q", got.Email)
			}
			if len(f.callsOf(pluginabi.MethodHostAuthGet)) != 0 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatalf("unexpected auth record write: %v", f.recorded())
			}
		})
	}
}

func TestQuotaEmailPersistenceFailureKeepsRefresh(t *testing.T) {
	const key = "quota-persist-failure-secret"
	fixture := newQuotaFixture(key)
	base := fixture.respond(t)
	m, f := quotaManager(key, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthGet {
			return hostErr("missing", "auth file not found for "+key), nil
		}
		return base(method, payload)
	})
	resp, got := refreshQuota(t, m, key)
	if resp.StatusCode != 0 || got.Email != "subscriber@example.com" || got.PlanType != "go-plus" {
		t.Fatalf("refresh = %d %s", resp.StatusCode, resp.Body)
	}
	logs := f.callsOf(pluginabi.MethodHostLog)
	if len(logs) != 1 || strings.Contains(string(logs[0].payload), key) || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatalf("logs = %v saves = %d", logs, len(f.callsOf(pluginabi.MethodHostAuthSave)))
	}
}

func TestSubscriberEmail(t *testing.T) {
	members := []budgetMember{{UserID: "acc_a", Email: "a@example.com"}, {UserID: "acc_b", Email: " b@example.com "}}
	for _, tc := range []struct {
		name, subscriber, want string
		members                []budgetMember
	}{
		{"subscriber match", "acc_b", "b@example.com", members},
		{"unknown subscriber in shared workspace", "acc_c", "", members},
		{"no subscriber in shared workspace", "", "", members},
		{"single member", "", "a@example.com", members[:1]},
		{"no members", "acc_a", "", nil},
	} {
		if got := subscriberEmail(tc.members, tc.subscriber); got != tc.want {
			t.Errorf("%s: email = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestQuotaUnknownKeyAndResource(t *testing.T) {
	const key = "quota-known-secret"
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{APIKeys: []config.APIKey{{Value: key}}}
	unknown := "opencode-go-key-unknown"
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + unknown + `"}`)})
	if err != nil || resp.StatusCode != http.StatusNotFound || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), unknown) || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("unknown response = %+v err=%v calls=%v", resp, err, f.callsOf(pluginabi.MethodHostHTTPDo))
	}
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/quota"})
	if err != nil || resource.StatusCode != 0 || resource.Headers.Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(string(resource.Body), "OpenCode Go Quota") || strings.Contains(string(resource.Body), key) {
		t.Fatalf("resource response = %+v err=%v", resource, err)
	}
}

func TestQuotaPageReadsRememberedManagementKey(t *testing.T) {
	if !strings.Contains(resources.QuotaPage, "parsed.state && parsed.state.managementKey") {
		t.Fatal("quota page does not read persisted state.managementKey")
	}
}

func TestQuotaPageDecodesCurrentCPAAuthStorage(t *testing.T) {
	for _, marker := range []string{"enc::v1::", "cli-proxy-api-webui::secure-storage", "TextDecoder"} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page does not decode current CPA auth storage: missing %q", marker)
		}
	}
}

func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
	id, _ := quotaIdentity(key)
	for name, responder := range map[string]func(string, []byte) ([]byte, error){
		"bridge": func(string, []byte) ([]byte, error) { return nil, context.Canceled },
		"status": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized, Body: []byte(key + " upstream body")}), nil
		},
		"json": func(string, []byte) ([]byte, error) {
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(key + " not json")}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCaller{responder: responder}
			m := NewManager(NewHostBridge(f.call))
			m.cfg = config.Config{BaseURL: "https://quota.test/v1", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: key}}}
			resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/quota-usage", Body: []byte(`{"key_id":"` + id + `"}`)})
			if err != nil || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(resp.Body), key) || strings.Contains(string(resp.Body), "upstream body") {
				t.Fatalf("response = %+v err=%v", resp, err)
			}
		})
	}
}

func TestQuotaPageIsStaticAndSecretFree(t *testing.T) {
	if resources.QuotaPage == "" || strings.Contains(resources.QuotaPage, testKey) || strings.Contains(resources.QuotaPage, "setInterval") || strings.Contains(resources.QuotaPage, "reset button") {
		t.Fatal("quota page contains a secret, polling, or reset action")
	}
	if !strings.Contains(resources.QuotaPage, "textContent") || !strings.Contains(resources.QuotaPage, "Remember password") {
		t.Fatal("quota page missing safe rendering or login guidance")
	}
}

func TestQuotaPageUsesManualSessionCache(t *testing.T) {
	page := resources.QuotaPage
	for _, marker := range []string{
		`const storageKey = "opencode-go-cliproxyapi:quota"`,
		"sessionStorage.getItem(storageKey)",
		"sessionStorage.setItem(storageKey, JSON.stringify(cache))",
		"JSON.parse(stored)",
		"typeof parsed === \"object\"",
		"Array.isArray(parsed)",
		"catch (_) {}",
		"Object.entries(cache)",
		"values.set(keyID, entry.usage)",
		"accounts.set(keyID, account(entry))",
		"cache[card.key_id] = {usage: result.usage, plan_type: info.plan_type, email: info.email, fetched_at: timestamp}",
		"accounts.delete(keyID)",
		"delete cache[keyID]",
		"new Set(cards.map(card => card.key_id))",
		"toLocaleString",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("quota page missing session cache marker %q", marker)
		}
	}
	for _, marker := range []string{
		"setTimeout",
		"visibilitychange",
		"document.hidden",
		"window.onfocus",
		"window.addEventListener(\"focus\"",
		"TTL",
		"expiry",
		"expiration",
		"bulk refresh",
	} {
		if strings.Contains(page, marker) {
			t.Fatalf("quota page contains prohibited automatic or expiry behavior %q", marker)
		}
	}
}

func TestQuotaPageUsesNativeQuotaStylesAndThemeBridge(t *testing.T) {
	for _, marker := range []string{
		"quota-page",
		"quota-grid",
		"quota-card",
		"quota-track",
		"quota-fill",
		"repeat(auto-fill, minmax(380px, 1fr))",
		"@media (max-width: 768px)",
		`[data-theme="white"]`,
		`[data-theme="dark"]`,
		`window.parent.document`,
		`data-theme`,
		`--bg-secondary`,
		`frameElement.style.backgroundColor`,
		`frameElement.parentElement.style.backgroundColor`,
		".quota-refresh",
		"--bg-secondary: #faf9f5",
		"--bg-primary: #f0eee8",
		"--bg-tertiary: #e9e6df",
		"--text-primary: #2d2a26",
		"--text-secondary: #6d6760",
		"--text-tertiary: #a29c95",
		"--border-color: #e3e1db",
		"--primary-color: #8b8680",
		"--primary-hover: #7f7a74",
		"border-radius: 8px; padding: 8px 10px",
		`MutationObserver`,
		`catch (_) {}`,
	} {
		if !strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page missing styling marker %q", marker)
		}
	}
	if strings.Count(resources.QuotaPage, `document.createElement("button")`) != 1 || strings.Contains(resources.QuotaPage, `textContent = "Refresh card"`) || strings.Contains(resources.QuotaPage, "quota-button") || strings.Contains(resources.QuotaPage, "quota-refresh-small") {
		t.Fatal("quota page does not have exactly one secondary refresh button path")
	}
	for _, marker := range []string{"querySelectorAll('head link[rel=\"stylesheet\"], head style')", "cloneNode(true)", "dataset.cpaStyle"} {
		if strings.Contains(resources.QuotaPage, marker) {
			t.Fatalf("quota page still clones parent styles: %q", marker)
		}
	}
}

func mustHashPrefix(key string) string {
	_, label := quotaIdentity(key)
	return strings.TrimPrefix(label, "OpenCode Go credential ")
}
