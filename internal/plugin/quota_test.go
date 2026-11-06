package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/config"
)

func TestQuotaProviderRegistration(t *testing.T) {
	m := NewManager(nil)
	var registration registrationResult
	raw := mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(testValidYAML))
	decodeResult(t, raw, &registration)
	if !registration.Capabilities.QuotaProvider {
		t.Fatal("registration did not advertise quota_provider")
	}
	if strings.Contains(string(raw), "management_api") {
		t.Fatalf("registration still advertises management_api: %s", raw)
	}

	var identifier map[string]string
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaIdentifier, []byte(`{}`)), &identifier)
	if identifier["identifier"] != ProviderID {
		t.Fatalf("quota identifier = %v", identifier)
	}
	var describe pluginapi.QuotaDescribeResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaDescribe, []byte(`{}`)), &describe)
	if len(describe.SupportedProviders) != 1 || describe.SupportedProviders[0] != ProviderID || describe.DisplayName != "OpenCode Go" || describe.SupportsReset {
		t.Fatalf("quota describe = %+v", describe)
	}
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodQuotaReset, []byte(`{}`))); env.OK || env.Error == nil || env.Error.Code != "unsupported" {
		t.Fatalf("quota reset = %+v", env)
	}
	for _, method := range []string{pluginabi.MethodManagementRegister, pluginabi.MethodManagementHandle} {
		if env := decodeEnv(t, mustHandle(t, m, method, []byte(`{}`))); env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
			t.Fatalf("%s = %+v, want unknown_method", method, env)
		}
	}
}

const (
	quotaStatusURL    = "https://console.test/console/api/go/status"
	quotaMembersURL   = "https://console.test/console/api/v1/budgets/members"
	quotaUsageBody    = `{"usage":{"rolling":{"status":"ok","percent":9,"resetsAt":"2026-09-05T12:20:04.000Z"},"weekly":{"status":"ok","percent":12,"resetsAt":"2026-09-10T00:00:00Z"},"monthly":{"status":"ok","percent":6,"resetsAt":"2026-10-01T00:00:00Z"}}}`
	goPlusStatusBody  = `{"subscriberUserId":"acc_sub","product":"go-plus","renewalProduct":"go-plus","access":{"meters":{"week":{"limitMicroCents":"12000000000","usedMicroCents":"0"}}}}`
	budgetMembersBody = `[{"user_id":"acc_team","email":"teammate@example.com"},{"user_id":"acc_sub","email":"subscriber@example.com","limit_micro_cents":null}]`
)

func quotaAuthID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "opencode-go-key-" + hex.EncodeToString(digest[:])
}

// quotaFixture serves the usage and Console endpoints for one key plus the
// CPA auth record callbacks for its credential. The Console lives on its own
// host so a request built from base-url cannot reach it.
type quotaFixture struct {
	key      string
	entry    pluginapi.HostAuthFileEntry
	record   string         // stored auth JSON returned by host.auth.get
	statuses map[string]int // non-200 status per URL
	usage    string         // overrides quotaUsageBody when set
}

func newQuotaFixture(key string) quotaFixture {
	id := quotaAuthID(key)
	return quotaFixture{
		key:    key,
		entry:  pluginapi.HostAuthFileEntry{ID: id, AuthIndex: "7", Name: id + ".json"},
		record: `{"type":"opencode-go","id":"` + id + `","api_key":"` + key + `","priority":3,"note":"keep"}`,
	}
}

// request is the quota.fetch payload CPA sends for this fixture's credential.
func (q quotaFixture) request() []byte {
	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{
		AuthIndex: q.entry.AuthIndex, AuthID: q.entry.ID, Provider: ProviderID,
		Attributes: map[string]string{"api_key": q.key},
	})
	return raw
}

func (q quotaFixture) respond(t *testing.T) func(string, []byte) ([]byte, error) {
	usage := quotaUsageBody
	if q.usage != "" {
		usage = q.usage
	}
	bodies := map[string]string{
		"https://quota.test/v1/usage": usage,
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

// quotaManager configures a different key than the fixture's, so a fetch
// can only succeed with the credential CPA passes in the request.
func quotaManager(responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	f := &fakeCaller{responder: responder}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = config.Config{BaseURL: "https://quota.test/v1/", ConsoleURL: "https://console.test/console", RequestTimeout: config.DefaultRequestTimeout, APIKeys: []config.APIKey{{Value: "configured-key-unused"}}}
	return m, f
}

func fetchQuotaOK(t *testing.T, m *Manager, request []byte) pluginapi.QuotaFetchResponse {
	t.Helper()
	var got pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, request), &got)
	return got
}

func fetchQuotaErr(t *testing.T, m *Manager, request []byte) *pluginabi.Error {
	t.Helper()
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, request))
	if env.OK || env.Error == nil {
		t.Fatalf("quota fetch succeeded, want error: %+v", env)
	}
	return env.Error
}

func TestQuotaFetch(t *testing.T) {
	const key = "quota-refresh-secret"
	fixture := newQuotaFixture(key)
	m, f := quotaManager(fixture.respond(t))
	got := fetchQuotaOK(t, m, fixture.request())

	if got.Subscription == nil || got.Subscription.Plan != "go-plus" || got.Subscription.TierName != "Go Plus" {
		t.Fatalf("subscription = %+v", got.Subscription)
	}
	if len(got.Groups) != 1 || got.Groups[0].DisplayName != "OpenCode Go" {
		t.Fatalf("groups = %+v", got.Groups)
	}
	want := []pluginapi.QuotaBucket{
		{Window: "5h", RemainingFraction: 0.91, ResetTime: "2026-09-05T12:20:04Z", Description: "5-hour limit"},
		{Window: "weekly", RemainingFraction: 0.88, ResetTime: "2026-09-10T00:00:00Z", Description: "Weekly limit"},
		{Window: "monthly", RemainingFraction: 0.94, ResetTime: "2026-10-01T00:00:00Z", Description: "Monthly limit"},
	}
	buckets := got.Groups[0].Buckets
	if len(buckets) != len(want) {
		t.Fatalf("buckets = %+v", buckets)
	}
	for i, bucket := range buckets {
		w := want[i]
		if bucket.Window != w.Window || math.Abs(bucket.RemainingFraction-w.RemainingFraction) > 1e-9 || bucket.ResetTime != w.ResetTime || bucket.Description != w.Description {
			t.Fatalf("bucket %d = %+v, want %+v", i, bucket, w)
		}
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

func TestQuotaFetchReadsKeyFromMetadata(t *testing.T) {
	const key = "quota-metadata-secret"
	fixture := newQuotaFixture(key)
	m, _ := quotaManager(fixture.respond(t))
	raw, _ := json.Marshal(pluginapi.QuotaFetchRequest{AuthIndex: "7", AuthID: fixture.entry.ID, Provider: ProviderID, Metadata: map[string]any{"api_key": key}})
	if got := fetchQuotaOK(t, m, raw); len(got.Groups) != 1 {
		t.Fatalf("quota = %+v", got)
	}
}

func TestQuotaFetchRequiresSelectedCredential(t *testing.T) {
	for name, req := range map[string]pluginapi.QuotaFetchRequest{
		"no key":         {AuthIndex: "7", Provider: ProviderID},
		"other provider": {AuthIndex: "7", Provider: "claude", Attributes: map[string]string{"api_key": "quota-other-secret"}},
	} {
		t.Run(name, func(t *testing.T) {
			m, f := quotaManager(func(string, []byte) ([]byte, error) { return hostOK(map[string]any{}), nil })
			raw, _ := json.Marshal(req)
			fetchQuotaErr(t, m, raw)
			if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
				t.Fatalf("contacted upstream without the selected credential: %v", f.recorded())
			}
		})
	}
	m, _ := quotaManager(nil)
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, []byte(`not json`))); env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("malformed request = %+v", env)
	}
}

func TestQuotaInferenceKeyKeepsUsage(t *testing.T) {
	const key = "quota-inference-secret"
	fixture := newQuotaFixture(key)
	fixture.statuses = map[string]int{quotaStatusURL: http.StatusForbidden, quotaMembersURL: http.StatusForbidden}
	m, f := quotaManager(fixture.respond(t))
	got := fetchQuotaOK(t, m, fixture.request())
	if got.Subscription != nil || len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 3 {
		t.Fatalf("quota = %+v", got)
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
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newQuotaFixture(key)
			edit(&fixture)
			m, f := quotaManager(fixture.respond(t))
			fetchQuotaOK(t, m, fixture.request())
			if len(f.callsOf(pluginabi.MethodHostAuthGet)) != 0 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
				t.Fatalf("unexpected auth record write: %v", f.recorded())
			}
		})
	}
	t.Run("no record", func(t *testing.T) {
		fixture := newQuotaFixture(key)
		request := fixture.request()
		fixture.entry = pluginapi.HostAuthFileEntry{ID: "opencode-go-key-other", AuthIndex: "8", Name: "opencode-go-key-other.json"}
		m, f := quotaManager(fixture.respond(t))
		fetchQuotaOK(t, m, request)
		if len(f.callsOf(pluginabi.MethodHostAuthGet)) != 0 || len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
			t.Fatalf("unexpected auth record write: %v", f.recorded())
		}
	})
}

func TestQuotaEmailPersistenceMatchesAuthIndex(t *testing.T) {
	const key = "quota-readable-name-secret"
	fixture := newQuotaFixture(key)
	request := fixture.request()
	// A renamed credential file keeps its auth index but not the digest name.
	fixture.entry.ID, fixture.entry.Name = "Personal", "Personal.json"
	m, f := quotaManager(fixture.respond(t))
	fetchQuotaOK(t, m, request)
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 1 || !strings.Contains(string(saves[0].payload), `"Personal.json"`) {
		t.Fatalf("auth saves = %v", saves)
	}
}

func TestQuotaEmailPersistenceFailureKeepsRefresh(t *testing.T) {
	const key = "quota-persist-failure-secret"
	fixture := newQuotaFixture(key)
	base := fixture.respond(t)
	m, f := quotaManager(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthGet {
			return hostErr("missing", "auth file not found for "+key), nil
		}
		return base(method, payload)
	})
	got := fetchQuotaOK(t, m, fixture.request())
	if got.Subscription == nil || got.Subscription.Plan != "go-plus" || len(got.Groups) != 1 {
		t.Fatalf("quota = %+v", got)
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

func TestNormalizeQuota(t *testing.T) {
	pct := func(v float64) *float64 { return &v }
	t.Run("missing windows and reset times are omitted", func(t *testing.T) {
		got, err := normalizeQuota(map[string]quotaUpstreamWindow{
			"weekly":  {Percent: pct(100), ResetsAt: "not a time"},
			"monthly": {Percent: nil, ResetsAt: "2026-10-01T00:00:00Z"},
			"daily":   {Percent: pct(50)},
		})
		if err != nil {
			t.Fatal(err)
		}
		buckets := got.Groups[0].Buckets
		if len(buckets) != 1 || buckets[0].Window != "weekly" || buckets[0].RemainingFraction != 0 || buckets[0].ResetTime != "" {
			t.Fatalf("buckets = %+v", buckets)
		}
	})
	for name, usage := range map[string]map[string]quotaUpstreamWindow{
		"no readings":      {},
		"negative percent": {"rolling": {Percent: pct(-1)}},
		"percent over 100": {"weekly": {Percent: pct(100.5)}},
		"not a number":     {"monthly": {Percent: pct(math.NaN())}},
	} {
		if _, err := normalizeQuota(usage); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPlanLabel(t *testing.T) {
	for plan, want := range map[string]string{"go": "Go", "go-plus": "Go Plus", "go_team": "Go Team", "": ""} {
		if got := planLabel(plan); got != want {
			t.Errorf("planLabel(%q) = %q, want %q", plan, got, want)
		}
	}
}

func TestQuotaErrorsAreRedacted(t *testing.T) {
	const key = "quota-error-secret"
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
			m, _ := quotaManager(responder)
			got := fetchQuotaErr(t, m, newQuotaFixture(key).request())
			if got.Code != "quota_failure" || strings.Contains(got.Message, key) || strings.Contains(got.Message, "upstream body") {
				t.Fatalf("error = %+v", got)
			}
		})
	}
}
