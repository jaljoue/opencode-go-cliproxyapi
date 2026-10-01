// Package responses implements the OpenAI Responses protocol adapter
// (arch §8): upstream endpoint /v1/responses serving GPT 5.6 Luna and
// any catalog model declared compatible (FR-004). Request translation is
// FR-005, response and stream translation FR-006, acceptance coverage
// AC §D. Responses support is REQUIRED first-release scope, not
// experimental (07-open-questions.md §5).
//
// Per arch §8 this package is isolated from the scheduler and catalog
// manager and uses only stdlib JSON primitives.
package responses

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/catalog"
	"opencode-go-cliproxyapi/internal/errclass"
	"opencode-go-cliproxyapi/internal/thinking"
)

// EndpointPath is the upstream endpoint path for the Responses route (FR-004).
var EndpointPath = catalog.RouteResponses.EndpointPath()

// BuildRequest translates an inbound request body from sourceFormat
// ("openai" Chat Completions, "claude" Anthropic Messages,
// "openai-response" native Responses passthrough) into a Responses body
// for upstreamModel (FR-005, AC §D). upstreamModel replaces whatever
// client-facing model ID the request carried (FR-003). ts is the target
// model's declared reasoning capability and drives budget→effort mapping
// via the shared thinking package; nil falls back to the default ladder.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation; messages are descriptive and redacted.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, ts *pluginapi.ThinkingSupport, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "openai-response":
		if !strings.HasPrefix(strings.ToLower(upstreamModel), "gpt") {
			return fromResponsesNormalized(upstreamModel, sourceBody, tools...)
		}
		return shared.RewriteModelID(upstreamModel, sourceBody, "openai-response")
	case "openai":
		return fromChatCompletions(upstreamModel, sourceBody, ts)
	case "claude":
		return fromClaudeMessages(upstreamModel, sourceBody, ts)
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// fromResponsesNormalized normalizes a native Responses passthrough body
// for non-GPT upstream models, which reject several OpenAI-only shapes:
// additional_tools items merge into tools, namespace tools flatten, custom
// tools and their history become function calls taking one "input" string,
// historical reasoning and compaction items are dropped (their encrypted
// blobs are bound to another account), and web_search declarations lose
// options the OpenCode Go server only accepts on web_search_preview.
// Other items and top-level fields pass through byte-for-byte.
func fromResponsesNormalized(upstreamModel string, sourceBody []byte, tools ...*shared.ResponseTools) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	var raw map[string]json.RawMessage
	if json.Unmarshal(sourceBody, &src) != nil || json.Unmarshal(sourceBody, &raw) != nil || raw == nil {
		return nil, errclass.Translation("invalid Responses request body: malformed JSON")
	}

	if inputRaw := raw["input"]; shared.HasContent(inputRaw) {
		var items []json.RawMessage
		if err := json.Unmarshal(inputRaw, &items); err != nil {
			var s string
			if json.Unmarshal(inputRaw, &s) != nil {
				return nil, errclass.Translation("input must be a string or an array of items")
			}
		} else {
			preserved := make([]json.RawMessage, 0, len(items))
			for _, item := range items {
				var m map[string]json.RawMessage
				if json.Unmarshal(item, &m) != nil || m == nil {
					preserved = append(preserved, item)
					continue
				}
				var kind string
				_ = json.Unmarshal(m["type"], &kind)
				switch kind {
				case "additional_tools":
					var declared []shared.RespTool
					if json.Unmarshal(m["tools"], &declared) != nil || declared == nil {
						return nil, errclass.Translation("additional_tools requires a tools array")
					}
					src.Tools = append(src.Tools, declared...)
					continue
				case "reasoning", "compaction":
					continue
				case "custom_tool_call_output":
					m["type"] = json.RawMessage(`"function_call_output"`)
				case "custom_tool_call", "function_call":
					var name, namespace, args string
					_ = json.Unmarshal(m["name"], &name)
					_ = json.Unmarshal(m["namespace"], &namespace)
					if kind == "custom_tool_call" {
						var input string
						_ = json.Unmarshal(m["input"], &input)
						wrapped, _ := json.Marshal(map[string]string{"input": input})
						args = string(wrapped)
						delete(m, "input")
						m["type"] = json.RawMessage(`"function_call"`)
					} else {
						_ = json.Unmarshal(m["arguments"], &args)
					}
					m["arguments"], _ = json.Marshal(shared.DefaultArgs(args))
					m["name"], _ = json.Marshal(shared.QualifiedResponseToolName(name, namespace))
					delete(m, "namespace")
				default:
					preserved = append(preserved, item)
					continue
				}
				b, err := json.Marshal(m)
				if err != nil {
					return nil, errclass.Translation("failed to encode normalized Responses request: " + err.Error())
				}
				preserved = append(preserved, b)
			}
			raw["input"], _ = json.Marshal(preserved)
		}
	}

	toolReq := &shared.ResponsesRequest{Tools: src.Tools, ToolChoice: src.ToolChoice}
	if _, eErr := shared.ResponseToolContext(tools).Normalize(toolReq, EndpointPath); eErr != nil {
		return nil, eErr
	}
	raw["model"], _ = json.Marshal(upstreamModel)
	if len(toolReq.Tools) > 0 {
		declared := make([]any, len(toolReq.Tools))
		for i, t := range toolReq.Tools {
			if t.Type == "web_search" {
				declared[i] = map[string]any{"type": "web_search"}
			} else {
				declared[i] = t
			}
		}
		raw["tools"], _ = json.Marshal(declared)
	} else {
		delete(raw, "tools")
	}
	if len(toolReq.ToolChoice) > 0 {
		raw["tool_choice"] = toolReq.ToolChoice
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, errclass.Translation("failed to encode normalized Responses request: " + err.Error())
	}
	return out, nil
}

// ---- target wire shape -------------------------------------------------

type responsesEnvelope struct {
	Model             string            `json:"model"`
	Input             []any             `json:"input,omitempty"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []shared.RespTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         map[string]any    `json:"reasoning,omitempty"`
	MaxOutputTokens   *int64            `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
}

func msgItem(role string, parts []map[string]any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": parts}
}

// textPart renders message text; assistant history replays with
// output_text, every other role uses input_text.
func textPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url}
}

// respToolChoice renders a shared.DecodeToolChoice kind into the Responses
// wire value; absent yields nil so omitempty drops the field. It is
// infallible: shared.DecodeToolChoice emits exactly the closed set below,
// so the switch covers every possible kind (the exhaustiveness test in
// request_test.go enforces this against shared.AllToolChoiceKinds).
func respToolChoice(kind, name string) json.RawMessage {
	switch kind {
	case shared.ToolChoiceAuto:
		return json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		return json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{"type": "function", "name": name})
		return b
	}
	return nil
}

// ---- Chat Completions source -------------------------------------------

// contentParts normalizes an OpenAI-style content field (JSON string or
// typed parts) into Responses input parts for the given role (FR-005
// multimodal preservation). The shared kernel owns validation and the
// empty-input policy: empty-text parts decode to zero parts so empty
// messages drop identically on every route (F5 parity).
func contentParts(raw json.RawMessage, role string) ([]map[string]any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.ImageURL != "" {
			out = append(out, imagePart(p.ImageURL))
			continue
		}
		out = append(out, textPart(role, p.Text))
	}
	return out, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Responses request (FR-005, AC §D): system/developer messages become
// instructions, user/assistant turns become message items (assistant
// replaying output_text), tool_calls history becomes function_call items,
// tool results become function_call_output items, max_tokens/
// max_completion_tokens become max_output_tokens, and reasoning_effort is
// capability-checked against ts and rejected descriptively when the model
// cannot represent it (matching the reverse Responses→CC leg).
// tool_choice is decoded by the shared classifier and rendered into the
// Responses wire values ("auto"/"none"/"required", flat
// {type:function,name} for a forced tool); parallel_tool_calls passes
// through as-is.
//
// Explicit omission policy (FR-005 "where compatible"): `stop` has no
// Responses equivalent and is dropped, as are sampling/extras without an
// equivalent (logprobs, frequency_penalty, presence_penalty, n, seed,
// response_format, logit_bias) via struct selection. Tool calls and
// reasoning controls are never dropped silently.
func fromChatCompletions(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	var src shared.ChatCompletionsRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	req := &responsesEnvelope{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	req.ToolChoice = respToolChoice(kind, tcName)
	if src.MaxTokens != nil && *src.MaxTokens > 0 {
		req.MaxOutputTokens = src.MaxTokens
	} else if src.MaxCompletionTokens != nil && *src.MaxCompletionTokens > 0 {
		req.MaxOutputTokens = src.MaxCompletionTokens
	}
	if src.ReasoningEffort != "" {
		norm := strings.ToLower(strings.TrimSpace(src.ReasoningEffort))
		if norm != "auto" { // "auto" sentinel omits reasoning block (FR-005)
			req.Reasoning = map[string]any{"effort": norm}
		}
	}

	var instr strings.Builder
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			// Instructions have no image representation: reject like the
			// sibling routes instead of dropping silently (FR-005).
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, shared.SystemImageRejected()
				}
			}
			if text := shared.JoinTexts(parts); text != "" {
				if instr.Len() > 0 {
					instr.WriteString("\n\n")
				}
				instr.WriteString(text)
			}
		case "user":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
		case "assistant":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": shared.DefaultArgs(tc.Function.Arguments),
				})
			}
		case "tool":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, errclass.Translation(fmt.Sprintf(
						"unsupported tool content part type %q; function_call_output carries text only", typ))
				}
			}
			req.Input = append(req.Input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  shared.JoinTexts(parts),
			})
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	req.Instructions = instr.String()

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  shared.ObjectSchema(t.Function.Parameters),
		})
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// ---- Claude Messages source --------------------------------------------

// reasoningEffortFor is the single off-state policy for the Responses
// target (FR-005), applied to an already-resolved effort value from both
// source legs. The dynamic "auto" sentinel has no wire representation here,
// so the reasoning field is omitted entirely (no dynamic sentinel on the
// Responses wire). "none" forwards verbatim when the model declares it via
// SupportedLevels — the same admission thinking.ValidateEffort applies and
// matching the CC-upstream leg which forwards the validated value as-is;
// when "none" is not declared there is nothing to express, so the field is
// omitted rather than clamped to the weakest representable level. Every
// other validated level forwards as-is.
func reasoningEffortFor(effort string, ts *pluginapi.ThinkingSupport) (string, bool) {
	switch {
	case effort == "auto":
		return "", false
	case effort == "none":
		if slices.Contains(thinking.SupportedLevels(ts), "none") {
			return effort, true
		}
		return "", false
	default:
		return effort, true
	}
}

// fromClaudeMessages translates an Anthropic Messages request into a
// Responses request (FR-005, AC §D): system becomes instructions, text/
// image blocks become input parts, tool_use/tool_result become
// function_call/function_call_output items, an enabled thinking budget
// maps to reasoning.effort through reasoningEffortFor (the "auto" sentinel
// and an undeclared "none" omit the field entirely; a declared "none"
// forwards), max_tokens becomes max_output_tokens (defaulted by the
// shared kernel, FR-005). Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
//
// Explicit omission policy (FR-005 "where compatible"): stop_sequences is
// dropped (no Responses equivalent); thinking/redacted_thinking blocks are
// dropped because signed Anthropic chain-of-thought has no Responses input
// representation (the thinking *control* maps to reasoning.effort); images
// nested inside tool_result content are rejected descriptively, never
// silently lost (FR-005). The tool_result is_error flag has no Responses
// field and is preserved as an "[error] " marker in the output text rather
// than lost silently.
func fromClaudeMessages(upstreamModel string, body []byte, ts *pluginapi.ThinkingSupport) ([]byte, *errclass.Error) {
	src, eErr := shared.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	req := &responsesEnvelope{
		Model:           upstreamModel,
		MaxOutputTokens: &src.MaxTokens,
		Stream:          src.Stream,
		Temperature:     src.Temperature,
		TopP:            src.TopP,
	}
	req.ToolChoice = respToolChoice(src.ToolChoiceKind, src.ToolChoiceName)
	if shared.ThinkingEnabled(src.Thinking) {
		if effort, ok := reasoningEffortFor(thinking.EffortFromBudget(src.Thinking.BudgetTokens, ts), ts); ok {
			req.Reasoning = map[string]any{"effort": effort}
		}
	}
	req.Instructions = src.System

	for _, m := range src.Messages {
		switch m.Role {
		case "system":
			text, eErr := shared.ClaudeSystemMessageText(m, EndpointPath)
			if eErr != nil {
				return nil, eErr
			}
			if text != "" {
				req.Input = append(req.Input, msgItem("system", []map[string]any{textPart("system", text)}))
			}
			continue
		case "user", "assistant":
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
		var parts []map[string]any
		flush := func() {
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
				parts = nil
			}
		}
		if m.Content != "" {
			parts = append(parts, textPart(m.Role, m.Content))
		}
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case "text":
				parts = append(parts, textPart(m.Role, blk.Text))
			case "image":
				parts = append(parts, imagePart(blk.URL))
			case "tool_use":
				flush()
				args := shared.DefaultArgs(string(blk.Input))
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   blk.CallID,
					"name":      blk.Name,
					"arguments": args,
				})
			case "tool_result":
				flush()
				output, eErr := shared.ToolResultText(blk.Result, "function_call_output carries text only")
				if eErr != nil {
					return nil, eErr
				}
				if blk.IsError {
					output = shared.ToolResultErrorPrefix + output
				}
				req.Input = append(req.Input, map[string]any{
					"type":    "function_call_output",
					"call_id": blk.CallID,
					"output":  output,
				})
			case "thinking", "redacted_thinking":
				// omitted per the FR-005 policy in the doc comment
			default:
				flush()
				return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
			}
		}
		flush()
	}

	for _, t := range src.Tools {
		// Claude tools carry no type field (validation cannot reject here).
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  shared.ObjectSchema(t.InputSchema),
		})
	}
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}
