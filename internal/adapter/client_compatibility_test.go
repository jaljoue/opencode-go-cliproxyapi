package adapters

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode-go-cliproxyapi/internal/adapter/chatcompletions"
	"opencode-go-cliproxyapi/internal/adapter/messages"
	"opencode-go-cliproxyapi/internal/adapter/responses"
	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

func TestClaudeInlineSystemMessages(t *testing.T) {
	// Keep the reported request's structure without retaining logged user data.
	body := []byte(`{"system":[{"type":"text","text":"initial","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"question"}]},
		{"role":"system","content":[{"type":"text","text":"later","cache_control":{"type":"ephemeral"}},{"type":"text","text":"instruction"}]},
		{"role":"assistant","content":"answer"},{"role":"system","content":"latest"}],
		"tools":[{"name":"WebSearch","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],
		"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"max_tokens":64000,"stream":true}`)
	for _, target := range []string{"chat", "responses", "messages"} {
		t.Run(target, func(t *testing.T) {
			var out []byte
			var eErr *errclass.Error
			switch target {
			case "chat":
				out, eErr = chatcompletions.BuildRequest("deepseek-v4.1-flash", "claude", body, nil)
			case "responses":
				out, eErr = responses.BuildRequest("gpt-5.6-luna", "claude", body, nil)
			case "messages":
				out, eErr = messages.BuildRequest("qwen3.7-max", "claude", body, nil)
			}
			if eErr != nil {
				t.Fatal(eErr)
			}
			var wire map[string]any
			if err := json.Unmarshal(out, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["stream"] != true || len(wire["tools"].([]any)) != 1 || !strings.Contains(string(out), "WebSearch") {
				t.Fatalf("lost stream flag or client tool: %s", out)
			}
			field := "messages"
			wantRoles := []string{"user", "system", "assistant", "system"}
			if target == "chat" {
				wantRoles = append([]string{"system"}, wantRoles...)
			} else if target == "responses" {
				field = "input"
				if wire["instructions"] != "initial" {
					t.Fatalf("initial instructions = %v", wire["instructions"])
				}
			}
			turns := wire[field].([]any)
			if len(turns) != len(wantRoles) {
				t.Fatalf("turns = %s", out)
			}
			for i, role := range wantRoles {
				if turns[i].(map[string]any)["role"] != role {
					t.Fatalf("turn %d changed order: %s", i, out)
				}
			}
			if target == "chat" {
				if turns[0].(map[string]any)["content"] != "initial" || turns[2].(map[string]any)["content"] != "later\n\ninstruction" || turns[4].(map[string]any)["content"] != "latest" {
					t.Fatalf("system text changed: %s", out)
				}
			}
			if target != "messages" && strings.Contains(string(out), "cache_control") {
				t.Fatalf("cache metadata leaked: %s", out)
			}
		})
	}
}

func TestClaudeInlineSystemRejectsNonText(t *testing.T) {
	for _, kind := range []string{"tool_addition", "tool_removal", "thinking", "tool_use"} {
		body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"` + kind + `","text":"must not disappear"}]}]}`)
		_, chatErr := chatcompletions.BuildRequest("m", "claude", body, nil)
		_, respErr := responses.BuildRequest("m", "claude", body, nil)
		for _, eErr := range []*errclass.Error{chatErr, respErr} {
			if eErr == nil || eErr.Class != errclass.ClassUnsupported || !strings.Contains(eErr.Message, kind) {
				t.Fatalf("%s error = %v", kind, eErr)
			}
		}
	}
}

func TestHostedSearchFilteringAcrossTargets(t *testing.T) {
	for _, target := range []string{"chat", "messages"} {
		t.Run(target, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"message","role":"user","content":"code"},{"type":"additional_tools","tools":[{"type":"web_search_preview"},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}],"tools":[{"type":"web_search"},{"type":"function","name":"web_search","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"read","namespace":"files"},"stream":true}`)
			tools := &shared.ResponseTools{StripHostedWebSearch: true}
			var out []byte
			var eErr *errclass.Error
			if target == "chat" {
				out, eErr = chatcompletions.BuildRequest("m", "openai-response", body, nil, tools)
			} else {
				out, eErr = messages.BuildRequest("m", "openai-response", body, nil, tools)
			}
			if eErr != nil {
				t.Fatal(eErr)
			}
			var wire map[string]any
			if err := json.Unmarshal(out, &wire); err != nil {
				t.Fatal(err)
			}
			if len(wire["tools"].([]any)) != 2 || !strings.Contains(string(out), `"name":"web_search"`) || !strings.Contains(string(out), `"name":"files__read"`) || strings.Contains(string(out), `"type":"web_search`) || strings.Contains(string(out), "additional_tools") {
				t.Fatalf("filter lost client tools or left hosted search: %s", out)
			}
		})
	}
}
