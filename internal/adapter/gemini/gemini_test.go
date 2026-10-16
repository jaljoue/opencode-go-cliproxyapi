package gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"opencode-go-cliproxyapi/internal/errclass"
)

const model = "gemini-4-argon-contributor"

func TestBuildRequestFromEachFormat(t *testing.T) {
	for format, body := range map[string]string{
		"openai":          `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh","tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]}`,
		"claude":          `{"model":"x","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"minimal"}}`,
		"openai-response": `{"model":"x","input":"hi","reasoning":{"effort":"medium"}}`,
	} {
		out, eErr := BuildRequest(model, format, []byte(body), false)
		if eErr != nil {
			t.Fatalf("%s: %v", format, eErr)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.0.text").String(); got != "hi" {
			t.Fatalf("%s: contents = %s", format, out)
		}
		level := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").String()
		if want := map[string]string{"openai": "high", "claude": "low", "openai-response": "medium"}[format]; level != want {
			t.Fatalf("%s: thinkingLevel = %q, want %q (%s)", format, level, want, out)
		}
	}
}

func TestBuildRequestRejectsUnknownFormatAndBadJSON(t *testing.T) {
	if _, eErr := BuildRequest(model, "gemini", []byte(`{}`), false); eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("unknown format err = %v", eErr)
	}
	if _, eErr := BuildRequest(model, "openai", []byte(`{bad`), false); eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("bad json err = %v", eErr)
	}
}

func TestConvertNonStream(t *testing.T) {
	original := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	translated, _ := BuildRequest(model, "openai", original, false)
	body := `{"responseId":"r1","modelVersion":"` + model + `","candidates":[{"finishReason":"STOP","index":0,"content":{"role":"model","parts":[{"text":"OK","thoughtSignature":"sig"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":5}}`
	out, eErr := ConvertNonStreamResponse("openai", model, original, translated, 200, []byte(body))
	if eErr != nil {
		t.Fatal(eErr)
	}
	if gjson.GetBytes(out, "choices.0.message.content").String() != "OK" || gjson.GetBytes(out, "usage.total_tokens").Int() != 5 {
		t.Fatalf("chat completion = %s", out)
	}

	_, eErr = ConvertNonStreamResponse("openai", model, original, translated, 400, []byte(`{"error":{"code":400,"message":"Thinking level MINIMAL is not supported"}}`))
	if eErr == nil || !strings.Contains(eErr.Message, "MINIMAL") {
		t.Fatalf("error = %v", eErr)
	}
}

func TestStreamToEachFormat(t *testing.T) {
	stream := "data: {\"responseId\":\"r1\",\"modelVersion\":\"" + model + "\",\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"O\"}]}}]}\n\n" +
		"data: {\"responseId\":\"r1\",\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"K\"}]}}]}\n\n" +
		"data: {\"responseId\":\"r1\",\"candidates\":[{\"finishReason\":\"STOP\",\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"\",\"thoughtSignature\":\"sig\"}]}}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":1,\"totalTokenCount\":5}}\n\n"
	requests := map[string]string{
		"openai":          `{"model":"x","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"claude":          `{"model":"x","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		"openai-response": `{"model":"x","stream":true,"input":"hi"}`,
	}
	for format, req := range requests {
		translated, _ := BuildRequest(model, format, []byte(req), true)
		sc := NewStreamConverter(format, model, []byte(req), translated)
		var events [][]byte
		// Split mid-frame so partial lines are buffered across Feed calls.
		for _, chunk := range []string{stream[:37], stream[37:]} {
			out, done, eErr := sc.Feed([]byte(chunk))
			if eErr != nil || done {
				t.Fatalf("%s: done=%v err=%v", format, done, eErr)
			}
			events = append(events, out...)
		}
		events = append(events, sc.Flush()...)
		if sc.Flush() != nil {
			t.Fatalf("%s: second Flush emitted events", format)
		}
		all := string(joinEvents(events))
		var text string
		switch format {
		case "openai":
			for _, e := range events {
				if strings.HasPrefix(string(e), "data:") || strings.HasPrefix(string(e), "event:") {
					t.Fatalf("chat chunk is framed: %q", e)
				}
				text += gjson.GetBytes(e, "choices.0.delta.content").String()
			}
			if !strings.Contains(all, `"finish_reason":"stop"`) {
				t.Fatalf("missing finish: %s", all)
			}
		case "claude":
			text = collect(t, events, "content_block_delta", "delta.text")
			if !strings.Contains(all, "event: message_stop") {
				t.Fatalf("missing message_stop: %s", all)
			}
		case "openai-response":
			text = collect(t, events, "response.output_text.delta", "delta")
			if !strings.Contains(all, "event: response.completed") {
				t.Fatalf("missing response.completed: %s", all)
			}
		}
		if text != "OK" {
			t.Fatalf("%s: text = %q from %s", format, text, all)
		}
	}
}

func TestStreamErrorPayload(t *testing.T) {
	sc := NewStreamConverter("openai", model, []byte(`{}`), []byte(`{}`))
	_, _, eErr := sc.Feed([]byte("data: {\"error\":{\"code\":429,\"message\":\"quota exhausted\"}}\n\n"))
	if eErr == nil || eErr.StatusCode != 429 || !strings.Contains(eErr.Message, "quota exhausted") {
		t.Fatalf("stream error = %+v", eErr)
	}
}

func joinEvents(events [][]byte) []byte {
	var b []byte
	for _, e := range events {
		b = append(b, e...)
	}
	return b
}

// collect concatenates a field from every SSE frame of one type, checking
// each emitted chunk ends its frames.
func collect(t *testing.T, events [][]byte, eventType, path string) string {
	t.Helper()
	var out string
	for _, e := range events {
		if !strings.HasSuffix(string(e), "\n\n") {
			t.Fatalf("unterminated SSE frame: %q", e)
		}
		for _, frame := range strings.Split(string(e), "\n\n") {
			frame = strings.TrimSpace(frame)
			if !strings.HasPrefix(frame, "event: "+eventType+"\n") {
				continue
			}
			data := frame[strings.Index(frame, "data: ")+len("data: "):]
			out += gjson.Get(data, path).String()
		}
	}
	return out
}
