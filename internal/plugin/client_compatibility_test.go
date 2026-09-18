package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

	"opencode-go-cliproxyapi/internal/errclass"
)

func TestClientCompatibilityExecution(t *testing.T) {
	for _, tc := range []struct {
		name, format, body, config, errorText string
		native                                bool
	}{
		{name: "claude inline system", format: "claude", body: `{"system":[{"type":"text","text":"initial"}],"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"later","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"WebSearch","input_schema":{"type":"object"}}]}`},
		{name: "codex optional search", format: "openai-response", body: `{"input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"read"}],"tool_choice":"auto"}`},
		{name: "codex additional search", format: "openai-response", body: `{"input":[{"type":"message","role":"user","content":"hi"},{"type":"additional_tools","tools":[{"type":"web_search_preview"},{"type":"function","name":"read"}]}]}`},
		{name: "search only auto", format: "openai-response", body: `{"input":"hi","tools":[{"type":"web_search"}],"tool_choice":"auto","parallel_tool_calls":true}`},
		{name: "native search passes through", format: "openai-response", native: true, body: `{"input":"hi","tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`},
		{name: "strict config", format: "openai-response", config: "strip-hosted-web-search: false\n", body: `{"input":"hi","tools":[{"type":"web_search"}]}`, errorText: "unsupported tool type"},
		{name: "forced search", format: "openai-response", body: `{"input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"read"}],"tool_choice":{"type":"web_search"}}`, errorText: "requires hosted web_search"},
		{name: "required search only", format: "openai-response", body: `{"input":"hi","tools":[{"type":"web_search"}],"tool_choice":"required"}`, errorText: "none remain"},
		{name: "search history", format: "openai-response", body: `{"input":[{"type":"web_search_call","status":"completed"}]}`, errorText: "history cannot be translated"},
		{name: "unknown hosted tool", format: "openai-response", body: `{"input":"hi","tools":[{"type":"unknown_hosted_tool"}]}`, errorText: "unsupported tool type"},
		{name: "system tool changes", format: "claude", body: `{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"WebSearch"}}]}]}`, errorText: "tool_removal"},
		{name: "malformed input", format: "openai-response", body: `{"input":`, errorText: "malformed"},
	} {
		for _, stream := range []bool{false, true} {
			mode := "non-stream"
			if stream {
				mode = "stream"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				f := &fakeCaller{}
				m := NewManager(NewHostBridge(f.call))
				t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
				model, path, response := "deepseek-v4.1-flash", "/v1/chat/completions", ccResponseBody
				frames := []string{"data: " + `{"id":"r1","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}` + "\n\n", "data: [DONE]\n\n"}
				if tc.native {
					model, path, response = "gpt-5.6-luna", "/v1/responses", responsesPassthrough
					frames = []string{"event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}` + "\n\n"}
				}
				next := upstreamRouter(t, map[string]string{path: response})
				method, httpMethod := "executor.execute", pluginabi.MethodHostHTTPDo
				if stream {
					next = streamResponder(streamScript{upstreamID: "compat-up", frames: frames})
					method, httpMethod = "executor.execute_stream", pluginabi.MethodHostHTTPDoStream
				}
				f.responder = wrapWithCatalog(`{"data":[{"id":"`+model+`"}]}`, next)
				registered, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML+"\n"+tc.config))
				if err != nil || !decodeEnv(t, registered).OK {
					t.Fatalf("register: %s %v", registered, err)
				}
				beforeDo := len(f.callsOf(pluginabi.MethodHostHTTPDo))
				body := tc.body
				if stream && tc.errorText == "" {
					body = strings.Replace(body, "{", `{"stream":true,`, 1)
				}
				request := execReqBody("opencode-go/"+model, tc.format, []byte(body), false)
				if stream {
					request = execStreamReqBody("opencode-go/"+model, tc.format, []byte(body), "compat-down")
				}
				result, err := m.HandleCall(method, request)
				if err != nil {
					t.Fatal(err)
				}
				env := decodeEnv(t, result)
				if tc.errorText != "" {
					if env.OK || env.Error == nil || env.Error.HTTPStatus != http.StatusBadRequest || env.Error.Retryable || !strings.Contains(env.Error.Message, tc.errorText) {
						t.Fatalf("expected non-retryable 400 containing %q: %s", tc.errorText, result)
					}
					if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != beforeDo || len(f.callsOf(pluginabi.MethodHostHTTPDoStream)) != 0 {
						t.Fatal("invalid request reached upstream")
					}
					return
				}
				if !env.OK {
					t.Fatalf("execute: %s", result)
				}
				m.bridge.WaitForInFlight(5 * time.Second)
				if stream {
					assertCleanStreamClose(t, f)
				}
				wire := lastWire(t, f, httpMethod)
				if wire["url"] != "https://opencode.ai/zen/go"+path {
					t.Fatalf("route = %v", wire["url"])
				}
				upstreamBody := wireBody(t, wire, "body")
				var upstream map[string]any
				if err := json.Unmarshal(upstreamBody, &upstream); err != nil {
					t.Fatal(err)
				}
				if tc.native {
					if !strings.Contains(string(upstreamBody), `"type":"web_search"`) || upstream["tool_choice"].(map[string]any)["type"] != "web_search" {
						t.Fatalf("native search changed: %s", upstreamBody)
					}
				} else if tc.format == "claude" {
					turns := upstream["messages"].([]any)
					if len(turns) != 3 || turns[0].(map[string]any)["content"] != "initial" || turns[1].(map[string]any)["role"] != "user" || turns[2].(map[string]any)["role"] != "system" || turns[2].(map[string]any)["content"] != "later" || !strings.Contains(string(upstreamBody), `"name":"WebSearch"`) {
						t.Fatalf("Claude translation = %s", upstreamBody)
					}
				} else {
					if strings.Contains(string(upstreamBody), `"type":"web_search`) {
						t.Fatalf("hosted search not removed: %s", upstreamBody)
					}
					if tc.name == "search only auto" {
						for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
							if _, ok := upstream[key]; ok {
								t.Fatalf("orphan %s: %s", key, upstreamBody)
							}
						}
					} else if !strings.Contains(string(upstreamBody), `"name":"read"`) {
						t.Fatalf("function tool lost: %s", upstreamBody)
					}
				}
			})
		}
	}
}

func TestRequestErrorEnvelopeDoesNotReclassifyUpstreamErrors(t *testing.T) {
	upstream := errclass.FromStatus(http.StatusTooManyRequests, "rate limited")
	env := decodeEnv(t, requestErrorEnvelope(upstream))
	if env.Error.HTTPStatus != http.StatusTooManyRequests || !env.Error.Retryable {
		t.Fatalf("upstream error changed: %+v", env.Error)
	}
	translation := errclass.Translation("malformed upstream response")
	env = decodeEnv(t, classEnvelope(translation))
	if env.Error.HTTPStatus == http.StatusBadRequest {
		t.Fatal("response failure became a request error")
	}
	env = decodeEnv(t, requestErrorEnvelope(translation))
	if env.Error.HTTPStatus != http.StatusBadRequest || translation.StatusCode != 0 {
		t.Fatal("request classification missing or mutated input")
	}
}
