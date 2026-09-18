package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"opencode-go-cliproxyapi/internal/errclass"
)

type responseToolIdentity struct {
	name      string
	namespace string
}

// ResponseTools retains request-local tool identities while translating a
// Responses conversation through a protocol with a flat function namespace.
// The same instance must accompany request and response conversion.
type ResponseTools struct {
	// StripHostedWebSearch omits optional hosted search declarations when the
	// upstream protocol cannot execute them. Client function tools are retained.
	StripHostedWebSearch bool
	names                map[string]responseToolIdentity
}

// Normalize merges top-level tools and additional_tools declarations, removes
// declaration items from the conversation, and qualifies tool calls and choices.
// First declaration wins for repeated identities, including across both sources.
// Optional hosted search is omitted only when StripHostedWebSearch is enabled;
// all other unsupported tools fail explicitly.
func (t *ResponseTools) Normalize(r *ResponsesRequest, target string) ([]RespItem, *errclass.Error) {
	items, eErr := r.DecodeInputItems()
	if eErr != nil {
		return nil, eErr
	}
	t.names = make(map[string]responseToolIdentity)
	var tools []RespTool
	strippedSearch := false
	var add func(RespTool, string) *errclass.Error
	add = func(tool RespTool, namespace string) *errclass.Error {
		if t.StripHostedWebSearch && namespace == "" && isHostedWebSearch(tool.Type) {
			strippedSearch = true
			return nil
		}
		if tool.Type == "namespace" && namespace == "" {
			if strings.TrimSpace(tool.Name) == "" || tool.Tools == nil {
				return errclass.Translation("namespace tool requires a name and tools array")
			}
			for _, child := range tool.Tools {
				if eErr := add(child, tool.Name); eErr != nil {
					return eErr
				}
			}
			return nil
		}
		if eErr := FunctionTool(tool.Type, target); eErr != nil {
			return eErr
		}
		if strings.TrimSpace(tool.Name) == "" {
			return errclass.Translation("function tool requires a name")
		}
		identity := responseToolIdentity{name: tool.Name, namespace: namespace}
		name := qualifiedResponseToolName(tool.Name, namespace)
		if prev, ok := t.names[name]; ok {
			if prev != identity {
				return errclass.Translation("Responses tool names collide after namespace conversion")
			}
			return nil
		}
		t.names[name] = identity
		tool.Name = name
		tools = append(tools, tool)
		return nil
	}
	for _, tool := range r.Tools {
		if eErr := add(tool, ""); eErr != nil {
			return nil, eErr
		}
	}
	conversation := make([]RespItem, 0, len(items))
	for _, item := range items {
		if item.Type != "additional_tools" {
			conversation = append(conversation, item)
			continue
		}
		if item.Tools == nil {
			return nil, errclass.Translation("additional_tools requires a tools array")
		}
		for _, tool := range item.Tools {
			if eErr := add(tool, ""); eErr != nil {
				return nil, eErr
			}
		}
	}
	for i := range conversation {
		item := &conversation[i]
		if t.StripHostedWebSearch && item.Type == "web_search_call" {
			return nil, &errclass.Error{Class: errclass.ClassUnsupported,
				Message: "hosted web_search history cannot be translated; start a new conversation or use a compatible Responses model"}
		}
		if item.Type == "function_call" {
			item.Name = qualifiedResponseToolName(item.Name, item.Namespace)
			item.Namespace = ""
		}
	}
	// Leave string choices and unrelated choice validation to DecodeToolChoice.
	var choice map[string]json.RawMessage
	if json.Unmarshal(r.ToolChoice, &choice) == nil && choice != nil {
		var kind, name, namespace string
		_ = json.Unmarshal(choice["type"], &kind)
		if t.StripHostedWebSearch && isHostedWebSearch(kind) {
			return nil, &errclass.Error{Class: errclass.ClassUnsupported,
				Message: "tool_choice requires hosted web_search, which this upstream route cannot execute"}
		}
		if kind == "function" {
			if raw, ok := choice["namespace"]; ok {
				if json.Unmarshal(raw, &namespace) != nil || json.Unmarshal(choice["name"], &name) != nil || name == "" {
					return nil, errclass.Translation("malformed namespaced tool_choice")
				}
				choice["name"], _ = json.Marshal(qualifiedResponseToolName(name, namespace))
				delete(choice, "namespace")
				r.ToolChoice, _ = json.Marshal(choice)
			}
		}
	}
	if strippedSearch {
		kind, name, eErr := DecodeToolChoice(r.ToolChoice)
		if eErr != nil {
			return nil, eErr
		}
		if (kind == ToolChoiceAny || kind == ToolChoiceNamed) && len(tools) == 0 {
			return nil, &errclass.Error{Class: errclass.ClassUnsupported,
				Message: "tool_choice requires a tool, but none remain after removing hosted web_search"}
		}
		if kind == ToolChoiceNamed {
			if _, exists := t.names[name]; !exists {
				return nil, &errclass.Error{Class: errclass.ClassUnsupported,
					Message: "tool_choice names a tool that is unavailable after removing hosted web_search"}
			}
		}
		// Avoid forwarding a tool choice without any tool declarations. Requests
		// requiring tools have already failed above; auto/none/absent are equivalent.
		if len(tools) == 0 {
			r.ToolChoice = nil
			r.ParallelToolCalls = nil
		}
	}
	r.Tools = tools
	return conversation, nil
}

func isHostedWebSearch(toolType string) bool {
	return toolType == "web_search" || toolType == "web_search_preview"
}

func qualifiedResponseToolName(name, namespace string) string {
	if namespace == "" {
		return name
	}
	qualified := namespace + "__" + name
	// Chat Completions limits function names to 64 bytes. Retain an identity
	// suffix when a namespace pushes the name over that limit.
	if len(qualified) > 64 {
		digest := sha256.Sum256([]byte(qualified))
		qualified = qualified[:50] + "__" + hex.EncodeToString(digest[:6])
	}
	return qualified
}

func (t *ResponseTools) identity(name string) responseToolIdentity {
	if t != nil {
		if identity, ok := t.names[name]; ok {
			return identity
		}
	}
	return responseToolIdentity{name: name}
}

// ResponseToolContext selects the optional context. Without one, request
// translation still normalizes declarations but doesn't retain reverse names.
func ResponseToolContext(contexts []*ResponseTools) *ResponseTools {
	if len(contexts) > 0 && contexts[0] != nil {
		return contexts[0]
	}
	return &ResponseTools{}
}
