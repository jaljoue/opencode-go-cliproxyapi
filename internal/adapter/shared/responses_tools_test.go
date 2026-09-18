package shared

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode-go-cliproxyapi/internal/errclass"
)

func TestResponsesAdditionalToolsMerge(t *testing.T) {
	var r ResponsesRequest
	body := `{
		"tools":[{"type":"function","name":"lookup","description":"original","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}],
		"input":[
			{"type":"message","role":"user","content":"find it"},
			{"type":"additional_tools","tools":[{"type":"function","name":"lookup","description":"duplicate"},{"type":"function","name":"read"}]},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
			{"type":"additional_tools","tools":[{"type":"function","name":"read"},{"type":"function","name":"write"}]},
			{"type":"function_call_output","call_id":"c1","output":"found"}
		]}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	var tools ResponseTools
	items, eErr := tools.Normalize(&r, "/v1/chat/completions")
	if eErr != nil {
		t.Fatal(eErr)
	}
	if len(r.Tools) != 3 || r.Tools[0].Name != "lookup" || r.Tools[1].Name != "read" || r.Tools[2].Name != "write" {
		t.Fatalf("merged tools = %+v", r.Tools)
	}
	if r.Tools[0].Description != "original" || !strings.Contains(string(r.Tools[0].Parameters), `"required":["q"]`) {
		t.Fatalf("first declaration lost metadata: %+v", r.Tools[0])
	}
	if len(items) != 3 || items[0].Type != "message" || items[1].CallID != "c1" || items[2].Output != "found" {
		t.Fatalf("conversation changed: %+v", items)
	}
}

func TestResponsesNamespaceIdentity(t *testing.T) {
	for _, namespace := range []string{"functions", strings.Repeat("long_namespace_", 6)} {
		t.Run(namespace, func(t *testing.T) {
			var r ResponsesRequest
			body := `{"tools":[{"type":"function","name":"read"}],"input":[
				{"type":"function_call","call_id":"c1","name":"read","namespace":` + strconvJSON(namespace) + `,"arguments":"{}"},
				{"type":"additional_tools","tools":[{"type":"namespace","name":` + strconvJSON(namespace) + `,"tools":[{"type":"function","name":"read"}]}]}
			],"tool_choice":{"type":"function","name":"read","namespace":` + strconvJSON(namespace) + `}}`
			if err := json.Unmarshal([]byte(body), &r); err != nil {
				t.Fatal(err)
			}
			var tools ResponseTools
			items, eErr := tools.Normalize(&r, "/v1/chat/completions")
			if eErr != nil {
				t.Fatal(eErr)
			}
			name := r.Tools[1].Name
			if name == "read" || len(name) > 64 || items[0].Name != name || items[0].Namespace != "" {
				t.Fatalf("namespaced history = %+v; tool = %q", items, name)
			}
			kind, choice, eErr := DecodeToolChoice(r.ToolChoice)
			if eErr != nil || kind != ToolChoiceNamed || choice != name {
				t.Fatalf("choice = %s, %s, %v", kind, choice, eErr)
			}
			oa := NewOutputAssembler("r1", &tools)
			oa.AppendFunctionCall("c2", name, `{"path":"a"}`)
			item := oa.Render()[0].(RespItem)
			if item.Name != "read" || item.Namespace != namespace || item.CallID != "c2" || item.Arguments != `{"path":"a"}` {
				t.Fatalf("response lost identity: %+v", item)
			}
		})
	}
}

func strconvJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestHostedSearchChoiceAndHistory(t *testing.T) {
	for _, tc := range []struct{ name, body, errorText string }{
		{"optional only", `{"tools":[{"type":"web_search"}],"tool_choice":"auto","parallel_tool_calls":true}`, ""},
		{"none", `{"tools":[{"type":"web_search"}],"tool_choice":"none"}`, ""},
		{"implicit auto", `{"tools":[{"type":"web_search_preview"}]}`, ""},
		{"required only", `{"tools":[{"type":"web_search"}],"tool_choice":"required"}`, "none remain"},
		{"forced search", `{"tools":[{"type":"web_search"},{"type":"function","name":"read"}],"tool_choice":{"type":"web_search"}}`, "requires hosted web_search"},
		{"forced preview", `{"tools":[{"type":"web_search_preview"}],"tool_choice":{"type":"web_search_preview"}}`, "requires hosted web_search"},
		{"required surviving function", `{"tools":[{"type":"web_search"},{"type":"function","name":"read"}],"tool_choice":"required"}`, ""},
		{"named missing tool", `{"tools":[{"type":"web_search"},{"type":"function","name":"read"}],"tool_choice":{"type":"function","name":"missing"}}`, "names a tool that is unavailable"},
		{"search history", `{"input":[{"type":"web_search_call","id":"search_1","status":"completed"}]}`, "history cannot be translated"},
		{"unknown hosted tool", `{"tools":[{"type":"unrecognized_hosted_tool"}]}`, "unsupported tool type"},
		{"custom tool", `{"tools":[{"type":"custom","name":"patch"}]}`, "unsupported tool type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r ResponsesRequest
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatal(err)
			}
			tools := ResponseTools{StripHostedWebSearch: true}
			_, eErr := tools.Normalize(&r, "/v1/chat/completions")
			if tc.errorText != "" {
				if eErr == nil || !strings.Contains(eErr.Message, tc.errorText) {
					t.Fatalf("error = %v, want %s", eErr, tc.errorText)
				}
				return
			}
			if eErr != nil {
				t.Fatal(eErr)
			}
			if len(r.Tools) == 0 && (r.ToolChoice != nil || r.ParallelToolCalls != nil) {
				t.Fatalf("orphan tool controls: %+v", r)
			}
			if len(r.Tools) != 0 && string(r.ToolChoice) != `"required"` {
				t.Fatalf("required choice lost: %s", r.ToolChoice)
			}
		})
	}
}

func TestResponsesAdditionalToolsValidation(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		class errclass.Class
	}{
		{"missing tools", `{"input":[{"type":"additional_tools"}]}`, errclass.ClassTranslation},
		{"null tools", `{"input":[{"type":"additional_tools","tools":null}]}`, errclass.ClassTranslation},
		{"missing name", `{"input":[{"type":"additional_tools","tools":[{"type":"function"}]}]}`, errclass.ClassTranslation},
		{"unsupported tool", `{"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, errclass.ClassUnsupported},
		{"unsupported namespace child", `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"n","tools":[{"type":"custom","name":"patch"}]}]}]}`, errclass.ClassUnsupported},
		{"missing namespace children", `{"tools":[{"type":"namespace","name":"n"}]}`, errclass.ClassTranslation},
		{"name collision", `{"tools":[{"type":"function","name":"n__read"}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"read"}]}]}]}`, errclass.ClassTranslation},
		{"invalid namespace choice", `{"tool_choice":{"type":"function","name":"read","namespace":123}}`, errclass.ClassTranslation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r ResponsesRequest
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatal(err)
			}
			var tools ResponseTools
			_, eErr := tools.Normalize(&r, "/v1/chat/completions")
			if eErr == nil || eErr.Class != tc.class {
				t.Fatalf("error = %v; want %s", eErr, tc.class)
			}
		})
	}
	var r ResponsesRequest
	_ = json.Unmarshal([]byte(`{"input":[{"type":"additional_tools","tools":[]}]}`), &r)
	var tools ResponseTools
	items, eErr := tools.Normalize(&r, "/v1/chat/completions")
	if eErr != nil || len(items) != 0 || len(r.Tools) != 0 {
		t.Fatalf("empty declaration = %v, %v", items, eErr)
	}
}
