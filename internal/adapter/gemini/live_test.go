//go:build live

package gemini

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Live round trips against OpenCode Go's native Gemini endpoint. Requires
// OPENCODE_GO_API_KEY and a workspace that allows models training on
// request data. Each test runs a tool call and its follow-up turn, which
// is where Gemini thought signatures break if replay is wrong.

const liveBase = "https://opencode.ai/inference/go/v1/models/" + model

func liveKey(t *testing.T) string {
	key := os.Getenv("OPENCODE_GO_API_KEY")
	if key == "" {
		t.Skip("OPENCODE_GO_API_KEY not set")
	}
	return key
}

func post(t *testing.T, key, session string, stream bool, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, liveBase+Action(stream), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-session", session)
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// roundTrip translates, sends, and translates back one client request.
func roundTrip(t *testing.T, key, session, format string, stream bool, req string) []byte {
	t.Helper()
	upstream, eErr := BuildRequest(model, format, []byte(req), stream)
	if eErr != nil {
		t.Fatal(eErr)
	}
	status, body := post(t, key, session, stream, upstream)
	if !stream {
		out, eErr := ConvertNonStreamResponse(format, model, []byte(req), upstream, status, body)
		if eErr != nil {
			t.Fatalf("%s: %v", format, eErr)
		}
		return out
	}
	if status >= 400 {
		t.Fatalf("%s stream: %d %s", format, status, body)
	}
	sc := NewStreamConverter(format, model, []byte(req), upstream)
	events, _, eErr := sc.Feed(body)
	if eErr != nil {
		t.Fatal(eErr)
	}
	events = append(events, sc.Flush()...)
	return bytes.Join(events, []byte("\n"))
}

const toolPrompt = "Call get_weather for Paris. After you get the result, answer in one short sentence."

func TestLiveOpenAIToolRoundTrip(t *testing.T) {
	key := liveKey(t)
	session := "live-openai-" + time.Now().Format("150405")
	first := `{"model":"x","reasoning_effort":"low","messages":[{"role":"user","content":"` + toolPrompt + `"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
	out := roundTrip(t, key, session, "openai", false, first)
	call := gjson.GetBytes(out, "choices.0.message.tool_calls.0")
	if call.Get("function.name").String() != "get_weather" {
		t.Fatalf("no tool call: %s", out)
	}
	// Clients usually drop provider extras; replay only the standard fields.
	assistant := `{"role":"assistant","content":null,"tool_calls":[{"id":` + call.Get("id").Raw + `,"type":"function","function":{"name":"get_weather","arguments":` + call.Get("function.arguments").Raw + `}}]}`
	second, _ := sjson.SetRawBytes([]byte(first), "messages.-1", []byte(assistant))
	second, _ = sjson.SetRawBytes(second, "messages.-1", []byte(`{"role":"tool","tool_call_id":`+call.Get("id").Raw+`,"content":"18C and sunny"}`))
	second, _ = sjson.SetBytes(second, "stream", true)
	stream := roundTrip(t, key, session, "openai", true, string(second))
	if !strings.Contains(strings.ToLower(string(stream)), "sunny") && !strings.Contains(string(stream), "18") {
		t.Fatalf("follow-up ignored tool result: %s", stream)
	}
}

func TestLiveResponsesToolRoundTrip(t *testing.T) {
	key := liveKey(t)
	session := "live-responses-" + time.Now().Format("150405")
	first := `{"model":"x","reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":"` + toolPrompt + `"}],` +
		`"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"stream":true}`
	stream := roundTrip(t, key, session, "openai-response", true, first)
	var completed gjson.Result
	for _, frame := range strings.Split(string(stream), "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(frame), "event: response.completed") {
			completed = gjson.Get(frame[strings.Index(frame, "data: ")+6:], "response.output")
		}
	}
	var call gjson.Result
	var history []string
	for _, item := range completed.Array() {
		history = append(history, item.Raw) // replay reasoning carriers as Codex does
		if item.Get("type").String() == "function_call" {
			call = item
		}
	}
	if call.Get("name").String() != "get_weather" {
		t.Fatalf("no function_call: %s", stream)
	}
	second := []byte(first)
	for _, item := range history {
		second, _ = sjson.SetRawBytes(second, "input.-1", []byte(item))
	}
	second, _ = sjson.SetRawBytes(second, "input.-1", []byte(`{"type":"function_call_output","call_id":`+call.Get("call_id").Raw+`,"output":"18C and sunny"}`))
	second, _ = sjson.SetBytes(second, "stream", false)
	out := roundTrip(t, key, session, "openai-response", false, string(second))
	if text := gjson.GetBytes(out, `output.#(type=="message").content.0.text`).String(); text == "" {
		t.Fatalf("no follow-up text: %s", out)
	}
}

func TestLiveClaudeToolRoundTrip(t *testing.T) {
	key := liveKey(t)
	session := "live-claude-" + time.Now().Format("150405")
	first := `{"model":"x","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"` + toolPrompt + `"}],` +
		`"tools":[{"name":"get_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
	out := roundTrip(t, key, session, "claude", false, first)
	use := gjson.GetBytes(out, `content.#(type=="tool_use")`)
	if use.Get("name").String() != "get_weather" {
		t.Fatalf("no tool_use: %s", out)
	}
	second, _ := sjson.SetRawBytes([]byte(first), "messages.-1", []byte(`{"role":"assistant","content":`+gjson.GetBytes(out, "content").Raw+`}`))
	second, _ = sjson.SetRawBytes(second, "messages.-1", []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":`+use.Get("id").Raw+`,"content":"18C and sunny"}]}`))
	second, _ = sjson.SetBytes(second, "stream", true)
	stream := roundTrip(t, key, session, "claude", true, string(second))
	if !strings.Contains(string(stream), "event: message_stop") || !strings.Contains(string(stream), "text_delta") {
		t.Fatalf("follow-up stream incomplete: %s", stream)
	}
}
