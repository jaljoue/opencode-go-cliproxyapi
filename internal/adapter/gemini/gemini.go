// Package gemini serves models that OpenCode Go only exposes through the
// native Gemini API (models/{id}:generateContent). Protocol translation is
// delegated to CLIProxyAPI's built-in Gemini translators, the same ones the
// host uses for Google's API, so tool calls, thought signatures, and
// reasoning map exactly as they do for CPA's native Gemini provider.
package gemini

import (
	"context"
	"encoding/json"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"opencode-go-cliproxyapi/internal/adapter/shared"
	"opencode-go-cliproxyapi/internal/errclass"
)

// EndpointPath is the route label used in error messages.
const EndpointPath = "/v1/models/{model}:generateContent"

// Action returns the generateContent path suffix for the requested mode.
func Action(stream bool) string {
	if stream {
		return ":streamGenerateContent?alt=sse"
	}
	return ":generateContent"
}

func sourceFormat(format string) (sdktranslator.Format, *errclass.Error) {
	switch format {
	case "openai", "claude", "openai-response":
		return sdktranslator.FromString(format), nil
	}
	return "", shared.UnsupportedFormat(format, EndpointPath)
}

// BuildRequest translates a client request into a Gemini generateContent body.
func BuildRequest(upstreamModel, format string, body []byte, stream bool) ([]byte, *errclass.Error) {
	from, eErr := sourceFormat(format)
	if eErr != nil {
		return nil, eErr
	}
	if !json.Valid(body) {
		return nil, errclass.Translation("malformed " + format + " request JSON")
	}
	out := sdktranslator.TranslateRequest(from, sdktranslator.FormatGemini, upstreamModel, body, stream)
	return clampThinkingLevel(out), nil
}

// clampThinkingLevel maps client effort names onto the low/medium/high
// levels Gemini accepts; the host normally does this with model metadata
// the plugin does not have.
func clampThinkingLevel(body []byte) []byte {
	const path = "generationConfig.thinkingConfig.thinkingLevel"
	level := strings.ToLower(gjson.GetBytes(body, path).String())
	var clamped string
	switch level {
	case "none", "minimal":
		clamped = "low"
	case "xhigh", "max":
		clamped = "high"
	default:
		return body
	}
	if out, err := sjson.SetBytes(body, path, clamped); err == nil {
		return out
	}
	return body
}

// ConvertNonStreamResponse translates a generateContent result back to the
// client format; original and translated are the client and upstream request
// bodies the translators consult for tool and signature context.
func ConvertNonStreamResponse(format, model string, original, translated []byte, status int, body []byte) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, body)
	}
	from, eErr := sourceFormat(format)
	if eErr != nil {
		return nil, eErr
	}
	if !json.Valid(body) {
		return nil, errclass.Translation("malformed Gemini response JSON")
	}
	var param any
	return sdktranslator.TranslateNonStream(context.Background(), sdktranslator.FormatGemini, from, model, original, translated, body, &param), nil
}

// StreamConverter translates a streamGenerateContent SSE stream. Gemini has
// no terminal event, so the final client events are produced by Flush once
// the upstream stream closes.
type StreamConverter struct {
	format     sdktranslator.Format
	formatErr  *errclass.Error
	model      string
	original   []byte
	translated []byte
	framer     *shared.SSEFramer
	param      any
	flushed    bool
}

// NewStreamConverter prepares stream translation for one request.
func NewStreamConverter(format, model string, original, translated []byte) *StreamConverter {
	from, eErr := sourceFormat(format)
	return &StreamConverter{
		format: from, formatErr: eErr, model: model,
		original: original, translated: translated,
		framer: shared.NewSSEFramer(false),
	}
}

// Feed consumes one network chunk and returns translated client events.
func (sc *StreamConverter) Feed(chunk []byte) ([][]byte, bool, *errclass.Error) {
	if sc.formatErr != nil {
		return nil, false, sc.formatErr
	}
	sc.framer.Push(chunk)
	var events [][]byte
	for {
		_, data, _, ok := sc.framer.Next()
		if !ok {
			return events, false, nil
		}
		if data == "" {
			continue
		}
		if gjson.Get(data, "error").Exists() {
			status := int(gjson.Get(data, "error.code").Int())
			if status < 400 {
				status = 502
			}
			return events, false, shared.UpstreamStatusError(status, []byte(data))
		}
		events = append(events, sc.translate([]byte(data))...)
	}
}

// Flush emits the terminal client events after the upstream stream closes.
func (sc *StreamConverter) Flush() [][]byte {
	if sc.flushed || sc.formatErr != nil {
		return nil
	}
	sc.flushed = true
	return sc.translate([]byte("[DONE]"))
}

func (sc *StreamConverter) translate(payload []byte) [][]byte {
	out := sdktranslator.TranslateStream(context.Background(), sdktranslator.FormatGemini, sc.format, sc.model, sc.original, sc.translated, payload, &sc.param)
	events := make([][]byte, 0, len(out))
	for _, event := range out {
		if len(event) == 0 {
			continue
		}
		// Chat Completions chunks are bare JSON; the host frames them. SSE
		// events must end their frame so consecutive events stay separate.
		if sc.format != sdktranslator.FormatOpenAI && !strings.HasSuffix(string(event), "\n\n") {
			event = append(event, '\n', '\n')
		}
		events = append(events, event)
	}
	return events
}
