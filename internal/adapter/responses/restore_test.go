package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode-go-cliproxyapi/internal/adapter/shared"
)

const restoreRequest = `{"input":"hi","tools":[{"type":"custom","name":"exec"},` +
	`{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},` +
	`{"type":"web_search"},{"type":"image_generation"}]}`

func restoreTools(t *testing.T) *shared.ResponseTools {
	t.Helper()
	rt := &shared.ResponseTools{StripHostedWebSearch: true}
	out, eErr := BuildRequest("grok-4.7", "openai-response", []byte(restoreRequest), nil, rt)
	if eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	var req struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 3 || req.Tools[0]["name"] != "exec" || req.Tools[1]["name"] != "files__read" || req.Tools[2]["type"] != "web_search" {
		t.Fatalf("native non-GPT tools = %s", out)
	}
	return rt
}

func TestNonGPTHistoryQualifiesNamespacedCalls(t *testing.T) {
	body := `{"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read"}]}],"input":[` +
		`{"type":"function_call","call_id":"c1","name":"read","namespace":"files","arguments":""},` +
		`{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"ls"},` +
		`{"type":"custom_tool_call_output","call_id":"c2","output":"ok"}]}`
	out, eErr := BuildRequest("muse-spark-1.3-contributor", "openai-response", []byte(body), nil)
	if eErr != nil {
		t.Fatal(eErr)
	}
	items := inputItems(t, decodeReq(t, out))
	call := itemMap(t, items, 0)
	if call["name"] != "files__read" || call["namespace"] != nil || call["arguments"] != "{}" {
		t.Fatalf("namespaced call = %v", call)
	}
	custom := itemMap(t, items, 1)
	if custom["type"] != "function_call" || custom["arguments"] != `{"input":"ls"}` || custom["input"] != nil {
		t.Fatalf("custom call = %v", custom)
	}
	if itemMap(t, items, 2)["type"] != "function_call_output" {
		t.Fatalf("custom output = %v", items[2])
	}
}

func TestPassthroughNonStreamRestoresCalls(t *testing.T) {
	rt := restoreTools(t)
	upstream := `{"id":"r","status":"completed","output":[` +
		`{"type":"function_call","call_id":"c1","name":"exec","arguments":"{\"input\":\"ls -la\"}"},` +
		`{"type":"function_call","call_id":"c2","name":"files__read","arguments":"{}"}]}`
	out, eErr := ConvertNonStreamResponse("openai-response", 200, []byte(upstream), rt)
	if eErr != nil {
		t.Fatal(eErr)
	}
	var resp struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if c := resp.Output[0]; c["type"] != "custom_tool_call" || c["input"] != "ls -la" || c["arguments"] != nil || c["name"] != "exec" {
		t.Fatalf("custom call = %v", c)
	}
	if f := resp.Output[1]; f["type"] != "function_call" || f["name"] != "read" || f["namespace"] != "files" {
		t.Fatalf("namespaced call = %v", f)
	}

	// Without declared custom or namespaced tools the body stays byte-identical.
	if out, _ := ConvertNonStreamResponse("openai-response", 200, []byte(upstream), shared.NewResponseTools()); string(out) != upstream {
		t.Fatalf("plain passthrough changed: %s", out)
	}
}

func TestPassthroughStreamRestoresCustomCall(t *testing.T) {
	rt := restoreTools(t)
	raw := frame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"exec","arguments":""}}`) +
		frame("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"input\":\"ls\"}"}`) +
		frame("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"input\":\"ls\"}"}`) +
		frame("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"c1","name":"exec","arguments":"{\"input\":\"ls\"}"}}`) +
		frame("response.output_text.delta", `{"delta":"hi"}`) +
		frame("response.completed", `{"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc_1","call_id":"c1","name":"exec","arguments":"{\"input\":\"ls\"}"}]}}`)
	events, done, eErr := feedChunks(t, NewStreamConverter("openai-response", rt), chunked(raw))
	if eErr != nil || !done {
		t.Fatalf("done=%v err=%v", done, eErr)
	}
	var names []string
	for _, e := range events {
		ev, _ := parseSSE(t, e)
		names = append(names, ev)
	}
	want := "response.output_item.added,response.custom_tool_call_input.done,response.output_item.done,response.output_text.delta,response.completed"
	if strings.Join(names, ",") != want {
		t.Fatalf("events = %v", names)
	}
	added := payloadOf(t, events[0])["item"].(map[string]any)
	if added["type"] != "custom_tool_call" || added["input"] != "" {
		t.Fatalf("added = %v", added)
	}
	if in := payloadOf(t, events[1]); in["input"] != "ls" || in["item_id"] != "fc_1" {
		t.Fatalf("input done = %v", in)
	}
	if item := payloadOf(t, events[2])["item"].(map[string]any); item["type"] != "custom_tool_call" || item["input"] != "ls" {
		t.Fatalf("item done = %v", item)
	}
	output := payloadOf(t, events[4])["response"].(map[string]any)["output"].([]any)
	if item := output[0].(map[string]any); item["type"] != "custom_tool_call" || item["input"] != "ls" {
		t.Fatalf("completed output = %v", output)
	}
	if want := frame("response.output_text.delta", `{"delta":"hi"}`); string(events[3]) != want {
		t.Fatalf("unrelated frame not verbatim: %q", events[3])
	}
}
