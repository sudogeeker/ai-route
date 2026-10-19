package responses

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func compatTestPlan(t *testing.T) *Plan {
	t.Helper()
	padding := json.RawMessage(`[{"type":"function","function":{"name":"shell","description":"shell","parameters":{"type":"object"}}}]`)
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}]}`)
	plan, err := Prepare(body, padding)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestProbeCompatReplaysTextStream(t *testing.T) {
	plan := compatTestPlan(t)
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	calls, replay, err := plan.ProbeCompat(strings.NewReader(stream))
	if err != nil || len(calls) != 0 {
		t.Fatalf("unexpected probe result: calls=%v err=%v", calls, err)
	}
	data, err := io.ReadAll(replay)
	if err != nil || string(data) != stream {
		t.Fatalf("replay changed the stream: %q %v", data, err)
	}
}

func TestProbeCompatReplaysLongTextAtLookahead(t *testing.T) {
	plan := compatTestPlan(t)
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", compatTextLookahead) + "\"},\"finish_reason\":null}]}\n\n"
	calls, replay, err := plan.ProbeCompat(strings.NewReader(stream))
	if err != nil || len(calls) != 0 || replay == nil {
		t.Fatalf("long text was not released at the lookahead boundary: calls=%v replay=%v err=%v", calls, replay, err)
	}
	data, err := io.ReadAll(replay)
	if err != nil || string(data) != stream {
		t.Fatalf("replay changed the stream: %q %v", data, err)
	}
}

func TestProbeCompatDetectsCompatibilityTool(t *testing.T) {
	plan := compatTestPlan(t)
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	calls, replay, err := plan.ProbeCompat(strings.NewReader(stream))
	if err != nil || replay != nil || len(calls) != 1 {
		t.Fatalf("unexpected probe result: calls=%v replay=%v err=%v", calls, replay, err)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "shell" || calls[0].Arguments != `{"command":"ls"}` {
		t.Fatalf("bad compatibility call: %+v", calls[0])
	}
}

func TestProbeCompatDetectsCompatibilityToolAfterText(t *testing.T) {
	plan := compatTestPlan(t)
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I will check.\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	calls, replay, err := plan.ProbeCompat(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "shell" || replay != nil {
		t.Fatalf("compatibility call after text was missed: calls=%v replay=%v", calls, replay)
	}
}

func TestProbeCompatPreservesCallerTool(t *testing.T) {
	plan := compatTestPlan(t)
	wire := wireName("exec_command", "")
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"" + wire + "\",\"arguments\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	calls, replay, err := plan.ProbeCompat(strings.NewReader(stream))
	if err != nil || len(calls) != 0 || replay == nil {
		t.Fatalf("unexpected probe result: calls=%v replay=%v err=%v", calls, replay, err)
	}
	data, _ := io.ReadAll(replay)
	if string(data) != stream {
		t.Fatalf("replay changed caller tool stream: %q", data)
	}
}

func TestAppendCompatRetryAddsErrorResult(t *testing.T) {
	plan := compatTestPlan(t)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	calls := []CompatCall{{ID: "call_1", Name: "shell", Arguments: `{"command":"ls"}`}}
	next, err := AppendCompatRetry(body, calls, plan.CallerTools())
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID       string  `json:"tool_call_id"`
			ReasoningContent *string `json:"reasoning_content"`
			Content          string  `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(next, &fields) != nil || len(fields.Messages) != 3 {
		t.Fatalf("bad retry body: %s", next)
	}
	if fields.Messages[1].Role != "assistant" || len(fields.Messages[1].ToolCalls) != 1 || fields.Messages[1].ToolCalls[0].Function.Name != "shell" {
		t.Fatalf("assistant call not preserved: %s", next)
	}
	if fields.Messages[1].ReasoningContent == nil || *fields.Messages[1].ReasoningContent != missingReasoningPlaceholder {
		t.Fatalf("compatibility retry omitted reasoning_content: %s", next)
	}
	wire := wireName("exec_command", "")
	if fields.Messages[2].Role != "tool" || fields.Messages[2].ToolCallID != "call_1" || !strings.Contains(fields.Messages[2].Content, wire) || !strings.Contains(fields.Messages[2].Content, "unavailable") {
		t.Fatalf("tool error not added: %s", next)
	}
}

func TestCompatRetryNamesAliasedCallerTool(t *testing.T) {
	padding := json.RawMessage(`[{"type":"function","function":{"name":"shell","description":"shell","parameters":{"type":"object"}}}]`)
	plan, err := Prepare([]byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`), padding)
	if err != nil {
		t.Fatal(err)
	}
	next, err := AppendCompatRetry(plan.ChatBody, []CompatCall{{ID: "call_1", Name: "shell", Arguments: "{}"}}, plan.CallerTools())
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(next, &fields) != nil || len(fields.Messages) < 2 {
		t.Fatalf("bad retry body: %s", next)
	}
	wire := wireName("shell", "")
	if !strings.Contains(fields.Messages[len(fields.Messages)-1].Content, wire) {
		t.Fatalf("retry notice did not name the aliased caller tool: %s", next)
	}
}
