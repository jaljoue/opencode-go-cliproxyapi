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
	names map[string]responseToolIdentity
}

// Normalize merges top-level tools and additional_tools declarations, removes
// declaration items from the conversation, and qualifies tool calls and choices.
// First declaration wins for repeated identities, including across both sources.
// Unsupported tools still fail explicitly rather than being silently discarded.
func (t *ResponseTools) Normalize(r *ResponsesRequest, target string) ([]RespItem, *errclass.Error) {
	items, eErr := r.DecodeInputItems()
	if eErr != nil {
		return nil, eErr
	}
	t.names = make(map[string]responseToolIdentity)
	var tools []RespTool
	var add func(RespTool, string) *errclass.Error
	add = func(tool RespTool, namespace string) *errclass.Error {
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
	r.Tools = tools
	return conversation, nil
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
