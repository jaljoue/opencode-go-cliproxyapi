package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Exercise the executor boundary so routing, request translation, and reverse
// tool identities are checked together in both streaming and non-streaming modes.
func TestExecuteResponsesAdditionalTools(t *testing.T) {
	const body = `{
		"tools":[{"type":"function","name":"read","description":"top-level"}],
		"input":[
			{"type":"message","role":"user","content":"read the file"},
			{"type":"additional_tools","tools":[
				{"type":"function","name":"read","description":"duplicate"},
				{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","description":"read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]}
			]},
			{"type":"function_call","call_id":"previous","name":"read","namespace":"files","arguments":"{\"path\":\"old\"}"},
			{"type":"function_call_output","call_id":"previous","output":"old contents"}
		],
		"tool_choice":{"type":"function","name":"read","namespace":"files"}
	}`
	cases := []struct {
		model, endpoint, response string
		frames                    []string
	}{
		{
			model: "deepseek-v4.1-flash", endpoint: "/v1/chat/completions",
			response: `{"id":"r1","model":"deepseek-v4.1-flash","choices":[{"message":{"tool_calls":[{"id":"next","type":"function","function":{"name":"files__read","arguments":"{\"path\":\"new\"}"}}]},"finish_reason":"tool_calls"}]}`,
			frames: []string{
				"data: " + `{"id":"r1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"next","type":"function","function":{"name":"files__read","arguments":""}}]}}]}` + "\n\n",
				"data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}` + "\n\n",
				"data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"new\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
				"data: [DONE]\n\n",
			},
		},
		{
			model: "qwen3.7-max", endpoint: "/v1/messages",
			response: `{"id":"r1","model":"qwen3.7-max","content":[{"type":"tool_use","id":"next","name":"files__read","input":{"path":"new"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`,
			frames: []string{
				"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"r1","model":"qwen3.7-max","usage":{"input_tokens":1}}}` + "\n\n",
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"next","name":"files__read","input":{}}}` + "\n\n",
				"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"new\"}"}}` + "\n\n",
				"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n",
				"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}` + "\n\n",
				"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n",
			},
		},
	}
	for _, tc := range cases {
		for _, mode := range []string{"non-stream", "stream", "stream-flush"} {
			t.Run(tc.model+"/"+mode, func(t *testing.T) {
				f := &fakeCaller{}
				m := NewManager(NewHostBridge(f.call))
				t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
				frames := tc.frames
				if mode == "stream-flush" {
					frames = frames[:len(frames)-1]
				}
				next := upstreamRouter(t, map[string]string{tc.endpoint: tc.response})
				method := "executor.execute"
				httpMethod := pluginabi.MethodHostHTTPDo
				if mode != "non-stream" {
					next = streamResponder(streamScript{upstreamID: "up", frames: frames})
					method = "executor.execute_stream"
					httpMethod = pluginabi.MethodHostHTTPDoStream
				}
				f.responder = wrapWithCatalog(`{"data":[{"id":"`+tc.model+`"}]}`, next)
				registered, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML))
				if err != nil || !decodeEnv(t, registered).OK {
					t.Fatalf("register = %s, %v", registered, err)
				}
				requestBody := body
				if mode != "non-stream" {
					requestBody = strings.Replace(body, "{", `{"stream":true,`, 1)
				}
				request := execStreamReqBody("opencode-go/"+tc.model, "openai-response", []byte(requestBody), "down")
				if mode == "non-stream" {
					request = execReqBody("opencode-go/"+tc.model, "openai-response", []byte(requestBody), false)
				}
				resp, err := m.HandleCall(method, request)
				if err != nil || !decodeEnv(t, resp).OK {
					t.Fatalf("execute = %s, %v", resp, err)
				}
				m.bridge.WaitForInFlight(5 * time.Second)
				wire := lastWire(t, f, httpMethod)
				if wire["url"] != "https://opencode.ai/zen/go"+tc.endpoint || wire["method"] != http.MethodPost {
					t.Fatalf("wrong endpoint: %v", wire)
				}
				var upstream map[string]any
				if err := json.Unmarshal(wireBody(t, wire, "body"), &upstream); err != nil {
					t.Fatal(err)
				}
				toolList := upstream["tools"].([]any)
				if len(toolList) != 2 || upstream["model"] != tc.model {
					t.Fatalf("upstream = %v", upstream)
				}
				tool := toolList[1].(map[string]any)
				choice := upstream["tool_choice"].(map[string]any)
				if tc.endpoint == "/v1/chat/completions" {
					tool = tool["function"].(map[string]any)
					choice = choice["function"].(map[string]any)
				}
				if tool["name"] != "files__read" || choice["name"] != "files__read" {
					t.Fatalf("tool = %v, choice = %v", tool, choice)
				}
				upstreamJSON := string(wireBody(t, wire, "body"))
				if strings.Contains(upstreamJSON, "additional_tools") || !strings.Contains(upstreamJSON, "old contents") || !strings.Contains(upstreamJSON, "top-level") || strings.Contains(upstreamJSON, "duplicate") {
					t.Fatalf("declarations or history lost: %s", upstreamJSON)
				}
				if mode == "non-stream" {
					var result pluginapi.ExecutorResponse
					if err := json.Unmarshal(decodeEnv(t, resp).Result, &result); err != nil {
						t.Fatal(err)
					}
					assertNamespacedToolResponse(t, result.Payload)
					return
				}
				assertCleanStreamClose(t, f)
				var sawAdded, sawCompleted bool
				for _, event := range emittedEvents(t, f) {
					for _, line := range strings.Split(event, "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
							t.Fatal(err)
						}
						switch string(payload["type"]) {
						case `"response.output_item.added"`:
							sawAdded = true
							var item map[string]any
							_ = json.Unmarshal(payload["item"], &item)
							if item["name"] != "read" || item["namespace"] != "files" || item["call_id"] != "next" {
								t.Fatalf("added = %v", item)
							}
						case `"response.completed"`:
							sawCompleted = true
							assertNamespacedToolResponse(t, payload["response"])
						}
					}
				}
				if !sawAdded || !sawCompleted {
					t.Fatalf("missing events: added=%t completed=%t", sawAdded, sawCompleted)
				}
			})
		}
	}
}

func assertNamespacedToolResponse(t *testing.T, body []byte) {
	t.Helper()
	var response struct {
		Output []struct {
			Type, Name, Namespace, Arguments string
			CallID                           string `json:"call_id"`
		}
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 {
		t.Fatalf("output = %s", body)
	}
	item := response.Output[0]
	if item.Type != "function_call" || item.Name != "read" || item.Namespace != "files" || item.CallID != "next" || item.Arguments != `{"path":"new"}` {
		t.Fatalf("output = %s", body)
	}
}
