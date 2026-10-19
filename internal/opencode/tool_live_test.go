package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

// A diagnostic for the upstream tool protocol, independent of the current
// tool-free compatibility adapter. No local commands or files are executed.
func TestOpenCodeLiveToolRoundTrip(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE_TEST") != "1" {
		t.Skip("set OPENCODE_LIVE_TEST=1 to test a live custom tool round trip")
	}
	model := os.Getenv("OPENCODE_LIVE_MODEL")
	if model == "" {
		model = "deepseek-v4.1-flash-free"
	}
	base, _ := url.Parse("https://opencode.ai/inference/openai/v1")
	p := &Proxy{cfg: config.OpenCode{BaseURL: base}}
	p.transport = func(source net.IP) *http.Transport {
		transport := p.newTransport(source)
		transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
		return transport
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(chatTools, &tools); err != nil {
		t.Fatal(err)
	}
	custom := json.RawMessage(`{"type":"function","function":{"name":"route_echo","description":"Return the supplied text.","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}}}`)
	messages := []map[string]any{{"role": "user", "content": "Call route_echo with text hello. After receiving its result, reply with the result exactly."}}
	body := map[string]any{"model": model, "messages": messages, "tools": []json.RawMessage{custom}, "tool_choice": map[string]any{"type": "function", "function": map[string]string{"name": "route_echo"}}, "stream": true, "max_completion_tokens": 512}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	identity, err := newRequestIdentity()
	if err != nil {
		t.Fatal(err)
	}
	attempt := func(collect bool) attemptResult {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		result := p.attempt(ctx, net.IPv6loopback, chatRequest{body: encoded, streaming: true, collect: collect}, identity)
		t.Cleanup(result.cleanup)
		return result
	}
	// First observe the ordinary caller-supplied tool path without padding.
	plain := attempt(false)
	if plain.response != nil {
		t.Logf("custom tool alone: HTTP %d", plain.response.StatusCode)
	}
	plain.cleanup()
	// Force the only implemented tool; the reserved declarations cannot be chosen.
	body["tools"] = append(tools, custom)
	result := attempt(false)
	if result.err != nil {
		t.Fatalf("custom tool with compatibility declarations: %v %s", result.err, result.body)
	}
	data, err := io.ReadAll(io.LimitReader(result.stream, maxEventBytes))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "data: [DONE]") {
		t.Fatal("custom tool response did not complete")
	}
	var callID, name, arguments, reasoning string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Reasoning string `json:"reasoning_content"`
					Calls     []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string
							Arguments string
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			reasoning += choice.Delta.Reasoning
			for _, call := range choice.Delta.Calls {
				callID += call.ID
				name += call.Function.Name
				arguments += call.Function.Arguments
			}
		}
	}
	var args struct{ Text string }
	if callID == "" || name != "route_echo" || json.Unmarshal([]byte(arguments), &args) != nil || args.Text != "hello" {
		t.Fatalf("invalid tool call: id=%q name=%q arguments=%q", callID, name, arguments)
	}
	t.Logf("native Go model=%s HTTP %d, tool=%s arguments=%s", model, result.response.StatusCode, name, arguments)
	toolOutput := "TOOL_OK_" + args.Text
	assistant := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}}}
	if reasoning != "" {
		assistant["reasoning_content"] = reasoning
	}
	body["messages"] = append(messages, assistant, map[string]any{"role": "tool", "tool_call_id": callID, "content": toolOutput})
	body["tool_choice"] = "none"
	result = attempt(true)
	if result.err != nil {
		t.Fatalf("tool result round trip failed: %v %s", result.err, result.body)
	}
	var response struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if json.Unmarshal(result.body, &response) != nil || len(response.Choices) != 1 || response.Choices[0].Message.Content != toolOutput {
		t.Fatalf("tool result was not consumed: %s", result.body)
	}
	t.Logf("tool result round trip: HTTP %d answer=%q", result.response.StatusCode, response.Choices[0].Message.Content)
}
