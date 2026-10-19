package responses

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func testStream() string {
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"more\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5,\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"
}

func TestPrepareResponsesInput(t *testing.T) {
	body := []byte(`{
		"model":"big-pickle",
		"instructions":"be brief",
		"stream":true,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"/tmp"}
		],
		"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{}}}],
		"tool_choice":"auto"
	}`)
	plan, err := Prepare(body, json.RawMessage(`[{"type":"function","function":{"name":"reserved"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Stream || plan.Model != "big-pickle" {
		t.Fatalf("lost plan metadata: %+v", plan)
	}
	var chat struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 5 || chat.Messages[0].Role != "system" || !bytes.Contains(chat.Messages[0].Content, []byte("must not be called")) || chat.Messages[1].Role != "system" || chat.Messages[2].Role != "user" || chat.Messages[3].Role != "assistant" || chat.Messages[4].Role != "tool" {
		t.Fatalf("bad message translation: %s", plan.ChatBody)
	}
	if !bytes.Contains(chat.Messages[2].Content, []byte(`"type":"text"`)) || !bytes.Contains(chat.Messages[2].Content, []byte(`"type":"image_url"`)) {
		t.Fatalf("content parts lost: %s", chat.Messages[2].Content)
	}
	wire := wireName("shell", "")
	if len(chat.Messages[3].ToolCalls) != 1 || chat.Messages[3].ToolCalls[0].Function.Name != wire || chat.Messages[3].ToolCalls[0].Function.Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("function call translation failed: %s", plan.ChatBody)
	}
	if chat.Messages[4].ToolCallID != "call_1" || string(chat.Messages[4].Content) != `"/tmp"` {
		t.Fatalf("tool output translation failed: %s", plan.ChatBody)
	}
	if len(chat.Tools) != 2 || chat.Tools[0].Function.Name != wire || chat.Tools[1].Function.Name != "reserved" {
		t.Fatalf("tool declarations changed: %s", plan.ChatBody)
	}
	if string(chat.ToolChoice) != `"auto"` {
		t.Fatalf("tool choice is not Chat-compatible: %s", plan.ChatBody)
	}
}

func TestPrepareAddsCompatibilityToolsToToolFreeRequests(t *testing.T) {
	padding := json.RawMessage(`[{"type":"function","function":{"name":"reserved"}}]`)
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":true}`), padding)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "reserved" || string(chat.ToolChoice) != `"none"` {
		t.Fatalf("bad tool-free compatibility request: %s", plan.ChatBody)
	}
}

func TestPrepareResponsesToolImageOutput(t *testing.T) {
	body := []byte(`{
		"model":"big-pickle",
		"input":[
			{"type":"message","role":"user","content":"look"},
			{"type":"function_call","call_id":"call_img","name":"view_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_img","output":[
				{"type":"input_text","text":"viewed"},
				{"type":"input_image","image_url":{"url":"data:image/jpeg;base64,YQ==","detail":"high"}}
			]},
			{"type":"message","role":"user","content":"what is it"}
		]
	}`)
	plan, err := Prepare(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 5 {
		t.Fatalf("bad message count: %s", plan.ChatBody)
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "call_img" || string(chat.Messages[2].Content) != `"viewed"` {
		t.Fatalf("tool text was not preserved: %s", plan.ChatBody)
	}
	if chat.Messages[3].Role != "user" || !bytes.Contains(chat.Messages[3].Content, []byte(`"type":"image_url"`)) || !bytes.Contains(chat.Messages[3].Content, []byte(`"detail":"high"`)) {
		t.Fatalf("tool image was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[4].Role != "user" || string(chat.Messages[4].Content) != `"what is it"` {
		t.Fatalf("following user message moved: %s", plan.ChatBody)
	}
}

func TestPrepareResponsesParallelToolImageOutputs(t *testing.T) {
	body := []byte(`{
		"model":"big-pickle",
		"input":[
			{"type":"message","role":"user","content":"look"},
			{"type":"function_call","call_id":"call_a","name":"view_image","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"view_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":[
				{"type":"input_image","image_url":"data:image/png;base64,YQ=="}
			]},
			{"type":"function_call_output","call_id":"call_b","output":"second"}
		]
	}`)
	plan, err := Prepare(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
			ToolCallID string            `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 5 {
		t.Fatalf("bad message count: %s", plan.ChatBody)
	}
	if chat.Messages[1].Role != "assistant" || len(chat.Messages[1].ToolCalls) != 2 {
		t.Fatalf("parallel tool calls were split: %s", plan.ChatBody)
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "call_a" || string(chat.Messages[2].Content) != `"Attachment output attached."` {
		t.Fatalf("image-only tool output is invalid: %s", plan.ChatBody)
	}
	if chat.Messages[3].Role != "tool" || chat.Messages[3].ToolCallID != "call_b" || string(chat.Messages[3].Content) != `"second"` {
		t.Fatalf("tool outputs were interrupted: %s", plan.ChatBody)
	}
	if chat.Messages[4].Role != "user" || !bytes.Contains(chat.Messages[4].Content, []byte(`"type":"image_url"`)) {
		t.Fatalf("tool image was not replayed after the tool group: %s", plan.ChatBody)
	}
}

func TestAllowedToolsChoiceUsesChatMode(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},{"type":"function","name":"other","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"shell"}]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if string(chat.ToolChoice) != `"auto"` {
		t.Fatalf("allowed_tools was not lowered: %s", plan.ChatBody)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != wireName("shell", "") {
		t.Fatalf("allowed_tools subset was not pushed upstream: %s", plan.ChatBody)
	}
	if names := plan.CallerTools(); len(names) != 1 || names[0] != wireName("shell", "") {
		t.Fatalf("compat retry advertised filtered tools: %v", names)
	}
}

func TestNamedToolChoiceRestrictsUpstreamTools(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","tools":[{"type":"function","name":"first","parameters":{"type":"object"}},{"type":"function","name":"second","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"second"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
		ToolChoice struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	wire := wireName("second", "")
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != wire {
		t.Fatalf("named tool choice was not restricted upstream: %s", plan.ChatBody)
	}
	if chat.ToolChoice.Type != "function" || chat.ToolChoice.Function.Name != wire {
		t.Fatalf("named tool choice changed: %s", plan.ChatBody)
	}
}

func TestToolChoiceNoneOmitsUpstreamTools(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"tool_choice":"none"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 0 || string(chat.ToolChoice) != `"none"` {
		t.Fatalf("tool_choice none still declared tools: %s", plan.ChatBody)
	}
}

func TestPrepareNormalizesFunctionCallArguments(t *testing.T) {
	body := []byte(`{
		"model":"big-pickle",
		"input":[
			{"type":"function_call","call_id":"call_obj","name":"echo","arguments":{"text":"hi"}},
			{"type":"function_call_output","call_id":"call_obj","output":"ok"},
			{"type":"function_call","call_id":"call_empty","name":"echo","arguments":""},
			{"type":"function_call_output","call_id":"call_empty","output":"ok"},
			{"type":"function_call","call_id":"call_missing","name":"echo"},
			{"type":"function_call_output","call_id":"call_missing","output":"ok"}
		]
	}`)
	plan, err := Prepare(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"text":"hi"}`, `{}`, `{}`}
	for index, expected := range want {
		message := chat.Messages[index*2]
		if message.Role != "assistant" || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Arguments != expected {
			t.Fatalf("arguments were not normalized at %d: %s", index, plan.ChatBody)
		}
	}
}

func TestPrepareDegradesOrphanAndDuplicateToolOutputs(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[{"type":"function_call_output","call_id":"call_missing","output":"ok"}]}`), nil)
	if err != nil {
		t.Fatalf("orphan tool output rejected the request: %v", err)
	}
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" || !strings.Contains(chat.Messages[0].Content, "call_missing") || !strings.Contains(chat.Messages[0].Content, "ok") {
		t.Fatalf("orphan tool output was not replayed as user text: %s", plan.ChatBody)
	}
	plan, err = Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"first"},
		{"type":"function_call_output","call_id":"call_1","output":"second"}
	]}`), nil)
	if err != nil {
		t.Fatalf("duplicate tool output rejected the request: %v", err)
	}
	chat.Messages = nil
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 || chat.Messages[1].Role != "tool" || chat.Messages[1].Content != "first" || chat.Messages[2].Role != "user" || !strings.Contains(chat.Messages[2].Content, "second") {
		t.Fatalf("duplicate tool output was not preserved: %s", plan.ChatBody)
	}
}

func TestPrepareAcceptsCustomToolOutput(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch"},
		{"type":"custom_tool_call_output","call_id":"call_patch","output":"done"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[1].Role != "tool" || chat.Messages[1].ToolCallID != "call_patch" || chat.Messages[1].Content != "done" {
		t.Fatalf("custom tool output was not preserved: %s", plan.ChatBody)
	}
}

func TestPrepareIgnoresHostedToolHistory(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"user","content":"before"},
		{"type":"web_search_call","id":"ws_1","status":"completed"},
		{"type":"file_search_call","id":"fs_1","status":"completed"},
		{"type":"computer_call_output","call_id":"cc_1","output":{"type":"input_text","text":"screen"}},
		{"type":"mcp_approval_response","approval_request_id":"mcp_1","approve":true},
		{"type":"message","role":"user","content":"after"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Content != "before" || chat.Messages[1].Content != "after" {
		t.Fatalf("hosted history changed replay: %s", plan.ChatBody)
	}

	plan, err = Prepare([]byte(`{"model":"big-pickle","input":[{"type":"bogus_call"}]}`), nil)
	if err != nil {
		t.Fatalf("future input item rejected the request: %v", err)
	}
	var future struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &future); err != nil {
		t.Fatal(err)
	}
	if len(future.Messages) != 0 {
		t.Fatalf("future input item changed replay: %s", plan.ChatBody)
	}
}

func TestPrepareMapsAgentMessages(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[
			{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\nhello"}
		]},
		{"type":"agent_message","author":"/root/worker","recipient":"/root","content":[
			{"type":"encrypted_content","encrypted_content":"secret"}
		]},
		{"type":"message","role":"user","content":"after"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "user" || chat.Messages[0].Content != "Message Type: NEW_TASK\nPayload:\nhello" || chat.Messages[1].Content != "after" {
		t.Fatalf("agent messages were not mapped as user input: %s", plan.ChatBody)
	}
}

func TestPrepareReplaysReasoningContent(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"think one"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"},
		{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"think two"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 5 {
		t.Fatalf("bad message count: %s", plan.ChatBody)
	}
	if chat.Messages[1].Role != "assistant" || chat.Messages[1].ReasoningContent != "think one" {
		t.Fatalf("commentary reasoning was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[2].Role != "assistant" || len(chat.Messages[2].ToolCalls) != 1 || chat.Messages[2].ToolCalls[0].ID != "call_1" || chat.Messages[2].ReasoningContent != "think one" {
		t.Fatalf("tool call reasoning was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[3].Role != "tool" || chat.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("tool output changed: %s", plan.ChatBody)
	}
	if chat.Messages[4].Role != "assistant" || chat.Messages[4].ReasoningContent != "think two" {
		t.Fatalf("assistant reasoning was not replayed: %s", plan.ChatBody)
	}
}

func TestPrepareDegradesUnsupportedImageFileID(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_123"}]}]}`), nil)
	if err != nil {
		t.Fatalf("file_id image rejected the request: %v", err)
	}
	if !bytes.Contains(plan.ChatBody, []byte(`[image file: file_123]`)) {
		t.Fatalf("file_id image was not replayed as text: %s", plan.ChatBody)
	}
	plan, err = Prepare([]byte(`{"model":"big-pickle","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,YQ==","detail":"tiny"}]}]}`), nil)
	if err != nil {
		t.Fatalf("unknown image detail rejected the request: %v", err)
	}
	if bytes.Contains(plan.ChatBody, []byte(`"detail":"tiny"`)) || !bytes.Contains(plan.ChatBody, []byte(`"type":"image_url"`)) {
		t.Fatalf("unknown image detail was not dropped: %s", plan.ChatBody)
	}
}

func TestPrepareMapsOriginalImageDetailToHigh(t *testing.T) {
	body := []byte(`{
		"model":"big-pickle",
		"input":[
			{"type":"function_call","call_id":"call_img","name":"view_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_img","output":[
				{"type":"input_image","image_url":"data:image/jpeg;base64,YQ==","detail":"original"}
			]}
		]
	}`)
	plan, err := Prepare(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plan.ChatBody, []byte(`"detail":"high"`)) || bytes.Contains(plan.ChatBody, []byte(`"detail":"original"`)) {
		t.Fatalf("original image detail was not mapped to high: %s", plan.ChatBody)
	}
}

func TestPrepareReplaysNonUserAttachmentsAsUserContent(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"look"},
			{"type":"input_image","image_url":"data:image/png;base64,YQ==","detail":"high"}
		]}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "assistant" || !bytes.Contains(chat.Messages[0].Content, []byte("look")) {
		t.Fatalf("assistant text was not preserved: %s", plan.ChatBody)
	}
	if chat.Messages[1].Role != "user" || !bytes.Contains(chat.Messages[1].Content, []byte(`"type":"image_url"`)) || !bytes.Contains(chat.Messages[1].Content, []byte(`"detail":"high"`)) {
		t.Fatalf("assistant image was not replayed as user content: %s", plan.ChatBody)
	}

	plan, err = Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"system","content":[
			{"type":"input_text","text":"document"},
			{"type":"input_file","filename":"a.txt","file_data":"data:text/plain;base64,YQ=="}
		]}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "system" || !bytes.Contains(chat.Messages[0].Content, []byte("document")) {
		t.Fatalf("system text was not preserved: %s", plan.ChatBody)
	}
	if chat.Messages[1].Role != "user" || !bytes.Contains(chat.Messages[1].Content, []byte(`"type":"file"`)) || !bytes.Contains(chat.Messages[1].Content, []byte("a.txt")) {
		t.Fatalf("system file was not replayed as user content: %s", plan.ChatBody)
	}
}

func TestPrepareKeepsOpaqueNonUserAttachmentsAsText(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[{"type":"message","role":"assistant","content":[{"type":"input_image","file_id":"img_1"}]}]}`), nil)
	if err != nil {
		t.Fatalf("opaque non-user attachment rejected the request: %v", err)
	}
	if !bytes.Contains(plan.ChatBody, []byte(`[image file: img_1]`)) {
		t.Fatalf("opaque attachment was lost: %s", plan.ChatBody)
	}
}

func TestWireNameIsReadableAndBounded(t *testing.T) {
	name := wireName("exec_command", "")
	if !strings.HasPrefix(name, "route_exec_command_") || len(name) > 64 {
		t.Fatalf("wire name is not readable or bounded: %q", name)
	}
	namespaced := wireName("read file", "workspace.v1")
	if !strings.HasPrefix(namespaced, "route_workspace_v1_read_file_") || strings.ContainsAny(namespaced, " ./") {
		t.Fatalf("namespace wire name was not sanitized: %q", namespaced)
	}
	if wireName("read", "one") == wireName("read", "two") {
		t.Fatal("namespaces produced the same wire name")
	}
	if long := wireName(strings.Repeat("x", 100), ""); len(long) > 64 {
		t.Fatalf("long wire name exceeds the Chat limit: %d", len(long))
	}
}

func TestPrepareValidatesCustomRegex(t *testing.T) {
	if _, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","tools":[{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"regex","definition":"["}}]}`), nil); err == nil || !strings.Contains(err.Error(), "regex is invalid") {
		t.Fatalf("invalid regex was accepted: %v", err)
	}
	if _, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","tools":[{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"regex","definition":"[a-z]+"}}]}`), nil); err != nil {
		t.Fatalf("valid regex was rejected: %v", err)
	}
}

func TestCompleteResponses(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","store":false}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := plan.Complete(strings.NewReader(testStream()))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
			Total  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "response" || response.Status != "completed" || response.Model != "big-pickle" || !strings.HasPrefix(response.ID, "resp_") {
		t.Fatalf("bad response envelope: %s", data)
	}
	if len(response.Output) != 2 ||
		response.Output[0].Type != "reasoning" ||
		response.Output[0].Status != "completed" ||
		len(response.Output[0].Summary) != 0 ||
		len(response.Output[0].Content) != 1 ||
		response.Output[0].Content[0].Type != "reasoning_text" ||
		response.Output[0].Content[0].Text != "think more" ||
		response.Output[1].Type != "message" ||
		response.Output[1].Content[0].Text != "hello" {
		t.Fatalf("bad output translation: %s", data)
	}
	if response.Usage.Input != 2 || response.Usage.Output != 3 || response.Usage.Total != 5 {
		t.Fatalf("bad usage: %s", data)
	}
}

func TestWriteResponsesStream(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(testStream()), &out); err != nil {
		t.Fatal(err)
	}
	events, final := parseEvents(t, out.String())
	for _, required := range []string{"response.created", "response.in_progress", "response.reasoning_text.delta", "response.reasoning_text.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.output_item.done", "response.completed"} {
		if !events[required] {
			t.Errorf("missing %s event:\n%s", required, out.String())
		}
	}
	for _, forbidden := range []string{"response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done"} {
		if events[forbidden] {
			t.Errorf("unexpected summary event %s:\n%s", forbidden, out.String())
		}
	}
	if final["status"] != "completed" {
		t.Fatalf("bad final status: %v", final)
	}
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("bad final output: %v", final)
	}
}

func TestReasoningClosesBeforeMessage(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(testStream()), &out); err != nil {
		t.Fatal(err)
	}
	reasoningDone, messageAdded := -1, -1
	index := 0
	scanner := bufio.NewScanner(strings.NewReader(out.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		item, _ := event["item"].(map[string]any)
		switch event["type"] {
		case "response.output_item.done":
			if item["type"] == "reasoning" {
				reasoningDone = index
				if item["status"] != "completed" {
					t.Fatalf("reasoning item not completed: %v", item)
				}
				content, _ := item["content"].([]any)
				if len(content) != 1 {
					t.Fatalf("reasoning content missing: %v", item)
				}
				part, _ := content[0].(map[string]any)
				if part["type"] != "reasoning_text" || part["text"] != "think more" {
					t.Fatalf("bad reasoning content: %v", item)
				}
			}
		case "response.output_item.added":
			if item["type"] == "message" {
				messageAdded = index
			}
		}
		index++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if reasoningDone < 0 || messageAdded < 0 || reasoningDone > messageAdded {
		t.Fatalf("reasoning must close before message starts: reasoning=%d message=%d\n%s", reasoningDone, messageAdded, out.String())
	}
}

func TestResponsesToolCallTranslation(t *testing.T) {
	wire := wireName("read_file", "")
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"read","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"tool_choice":"required","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"checking \"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_read\",\"type\":\"function\",\"function\":{\"name\":\"" + wire + "\",\"arguments\":\"{\\\"path\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a.txt\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	events, final := parseEvents(t, out.String())
	if !events["response.function_call_arguments.delta"] || !events["response.function_call_arguments.done"] {
		t.Fatalf("missing tool events:\n%s", out.String())
	}
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("bad tool output: %v", final)
	}
	call, _ := output[1].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "read_file" || call["call_id"] != "call_read" || call["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("tool call not restored: %v", call)
	}
}

func TestWriteStreamAcceptsMissingDoneAfterFinish(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n"
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(stream), &out); err != nil {
		t.Fatalf("missing [DONE] after finish_reason should be accepted: %v", err)
	}
	events := parseEventList(t, out.String())
	if len(events) == 0 || events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("expected response.completed, got %s", out.String())
	}
}

func TestCompleteStillRequiresDone(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":false}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n"
	if _, err := plan.Complete(strings.NewReader(stream)); err == nil || !strings.Contains(err.Error(), "before [DONE]") {
		t.Fatalf("non-streaming completion must still require [DONE], got %v", err)
	}
}

func TestWriteStreamEmitsIncompleteForLength(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"cut\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	events := parseEventList(t, out.String())
	last := events[len(events)-1]
	if last["type"] != "response.incomplete" {
		t.Fatalf("expected response.incomplete, got %s", out.String())
	}
	response, _ := last["response"].(map[string]any)
	if response["status"] != "incomplete" {
		t.Fatalf("unexpected status: %v", response)
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("unexpected incomplete details: %v", response)
	}
}

func TestWriteStreamEmitsFailedOnTruncation(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"hi","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	var out bytes.Buffer
	err = plan.WriteStream(strings.NewReader(stream), &out)
	if err == nil {
		t.Fatal("truncated stream unexpectedly succeeded")
	}
	events := parseEventList(t, out.String())
	last := events[len(events)-1]
	if last["type"] != "response.failed" {
		t.Fatalf("expected response.failed, got %s", out.String())
	}
	response, _ := last["response"].(map[string]any)
	failure, _ := response["error"].(map[string]any)
	if failure["code"] != "server_error" || failure["message"] == "" {
		t.Fatalf("unexpected failure payload: %v", response)
	}
}

func parseEventList(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("bad event JSON: %v %s", err, line)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}
	return events
}

func parseEvents(t *testing.T, body string) (map[string]bool, map[string]any) {
	t.Helper()
	seen := make(map[string]bool)
	var final map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("bad event JSON: %v %s", err, line)
		}
		kind, _ := event["type"].(string)
		if kind == "" || event["sequence_number"] == nil {
			t.Fatalf("event missing type/sequence: %v", event)
		}
		seen[kind] = true
		if kind == "response.completed" {
			final, _ = event["response"].(map[string]any)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return seen, final
}

func TestResponsesCustomToolTranslation(t *testing.T) {
	wire := wireName("apply_patch", "")
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":"patch","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"tool_choice":"required","stream":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_patch\",\"type\":\"function\",\"function\":{\"name\":\"" + wire + "\",\"arguments\":\"{\\\"input\\\":\\\"*** Begin Patch\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(stream), &out); err != nil {
		t.Fatal(err)
	}
	events, final := parseEvents(t, out.String())
	if !events["response.custom_tool_call_input.delta"] || !events["response.custom_tool_call_input.done"] {
		t.Fatalf("missing custom tool events:\n%s", out.String())
	}
	output, _ := final["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("bad custom output: %v", final)
	}
	call, _ := output[0].(map[string]any)
	if call["type"] != "custom_tool_call" || call["name"] != "apply_patch" || call["call_id"] != "call_patch" || call["input"] != "*** Begin Patch" {
		t.Fatalf("custom tool call not restored: %v", call)
	}
}

func TestPrepareMapsExtendedResponseFields(t *testing.T) {
	plan, err := Prepare([]byte(`{
		"model":"big-pickle",
		"input":"hi",
		"stream":true,
		"previous_response_id":"resp_old",
		"conversation":"conv_1",
		"store":true,
		"background":true,
		"service_tier":"flex",
		"safety_identifier":"safe-user",
		"modalities":["text","audio"],
		"audio":{"voice":"alloy","format":"wav"},
		"verbosity":"low",
		"max_output_tokens":42,
		"reasoning":{"effort":"high","summary":"auto","context":"all"},
		"text":{"verbosity":"medium","format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}},
		"stream_options":{"include_usage":false,"include_obfuscation":false},
		"metadata":{"a":"1"},
		"client_metadata":{"a":"client","b":"2"},
		"logprobs":true,
		"top_logprobs":3,
		"prompt_cache_key":"cache-key",
		"include":["reasoning.encrypted_content"],
		"access_programs":["program-a"]
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		MaxCompletionTokens int      `json:"max_completion_tokens"`
		ReasoningEffort     string   `json:"reasoning_effort"`
		Verbosity           string   `json:"verbosity"`
		ServiceTier         string   `json:"service_tier"`
		SafetyIdentifier    string   `json:"safety_identifier"`
		Modalities          []string `json:"modalities"`
		Audio               struct {
			Voice  string `json:"voice"`
			Format string `json:"format"`
		} `json:"audio"`
		Logprobs       bool   `json:"logprobs"`
		TopLogprobs    int    `json:"top_logprobs"`
		PromptCacheKey string `json:"prompt_cache_key"`
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string         `json:"name"`
				Strict bool           `json:"strict"`
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		StreamOptions struct {
			IncludeUsage       bool `json:"include_usage"`
			IncludeObfuscation bool `json:"include_obfuscation"`
		} `json:"stream_options"`
		Metadata   map[string]string `json:"metadata"`
		Store      any               `json:"store"`
		Background any               `json:"background"`
		Previous   any               `json:"previous_response_id"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.MaxCompletionTokens != 42 || chat.ReasoningEffort != "high" || chat.Verbosity != "medium" {
		t.Fatalf("core options were not mapped: %s", plan.ChatBody)
	}
	if chat.ServiceTier != "flex" || chat.SafetyIdentifier != "safe-user" || len(chat.Modalities) != 2 || chat.Audio.Voice != "alloy" || chat.Audio.Format != "wav" {
		t.Fatalf("provider options were not mapped: %s", plan.ChatBody)
	}
	if !chat.Logprobs || chat.TopLogprobs != 3 || chat.PromptCacheKey != "cache-key" {
		t.Fatalf("sampling options were not mapped: %s", plan.ChatBody)
	}
	if chat.ResponseFormat.Type != "json_schema" || chat.ResponseFormat.JSONSchema.Name != "answer" || !chat.ResponseFormat.JSONSchema.Strict || chat.ResponseFormat.JSONSchema.Schema["type"] != "object" {
		t.Fatalf("text.format was not mapped: %s", plan.ChatBody)
	}
	if !chat.StreamOptions.IncludeUsage || chat.StreamOptions.IncludeObfuscation {
		t.Fatalf("stream options were not mapped: %s", plan.ChatBody)
	}
	if chat.Metadata["a"] != "1" || chat.Metadata["b"] != "2" {
		t.Fatalf("metadata was not merged: %s", plan.ChatBody)
	}
	if chat.Store != nil || chat.Background != nil || chat.Previous != nil {
		t.Fatalf("stateless-only controls were forwarded: %s", plan.ChatBody)
	}
}

func TestPrepareSupportsCodexShellSearchAndImageItems(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"user","content":"start"},
		{"type":"local_shell_call","call_id":"lsh_1","status":"completed","action":{"type":"exec","command":["echo","hi"],"timeout_ms":1000}},
		{"type":"function_call_output","call_id":"lsh_1","output":"hi"},
		{"type":"tool_search_call","call_id":"ts_1","execution":"client","arguments":{"query":"docs"}},
		{"type":"tool_search_output","call_id":"ts_1","status":"completed","execution":"client","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
		{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"golang"}},
		{"type":"image_generation_call","id":"ig_1","status":"completed","revised_prompt":"a cat","result":"YQ=="},
		{"type":"message","role":"assistant","content":"done"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 9 {
		t.Fatalf("bad message count: %s", plan.ChatBody)
	}
	foundLookup := false
	for _, declared := range chat.Tools {
		if declared.Function.Name == wireName("lookup", "") {
			foundLookup = true
		}
	}
	if !foundLookup {
		t.Fatalf("tool_search_output declaration was not registered: %s", plan.ChatBody)
	}
	if chat.Messages[1].Role != "assistant" || len(chat.Messages[1].ToolCalls) != 1 || chat.Messages[1].ToolCalls[0].ID != "lsh_1" || chat.Messages[1].ToolCalls[0].Function.Name != "local_shell" || !strings.Contains(chat.Messages[1].ToolCalls[0].Function.Arguments, `"command"`) {
		t.Fatalf("local shell call was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "lsh_1" || !strings.Contains(string(chat.Messages[2].Content), `"hi"`) {
		t.Fatalf("local shell output was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[3].Role != "assistant" || len(chat.Messages[3].ToolCalls) != 1 || chat.Messages[3].ToolCalls[0].ID != "ts_1" || chat.Messages[3].ToolCalls[0].Function.Name != "tool_search" {
		t.Fatalf("tool search call was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[4].Role != "tool" || chat.Messages[4].ToolCallID != "ts_1" || !strings.Contains(string(chat.Messages[4].Content), "lookup") {
		t.Fatalf("tool search output was not replayed: %s", plan.ChatBody)
	}
	if !bytes.Contains(chat.Messages[5].Content, []byte("Web search: golang")) {
		t.Fatalf("web search was not preserved: %s", plan.ChatBody)
	}
	if !bytes.Contains(chat.Messages[6].Content, []byte("Image generated: a cat")) {
		t.Fatalf("image generation prompt was not preserved: %s", plan.ChatBody)
	}
	if chat.Messages[7].Role != "user" || !bytes.Contains(chat.Messages[7].Content, []byte(`data:image/png;base64,YQ==`)) {
		t.Fatalf("generated image was not replayed: %s", plan.ChatBody)
	}
	if chat.Messages[8].Role != "assistant" || !bytes.Contains(chat.Messages[8].Content, []byte("done")) {
		t.Fatalf("final assistant message changed: %s", plan.ChatBody)
	}
}

func TestPrepareDegradesUnknownInputShapes(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":{"type":"message","content":"single item"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plan.ChatBody, []byte("single item")) {
		t.Fatalf("single input item was not accepted: %s", plan.ChatBody)
	}
	plan, err = Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"critic","content":[{"type":"future_part","text":"kept"},{"type":"input_image","file_id":"img_1"}]},
		{"type":"message","content":"no role"},
		{"type":"message","role":"user","content":[{"type":"another_future","text":"also kept"}]}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("unknown input shapes changed message count: %s", plan.ChatBody)
	}
	for _, message := range chat.Messages {
		if message.Role != "user" {
			t.Fatalf("unknown role was not mapped to user: %s", plan.ChatBody)
		}
	}
	for _, want := range []string{"kept", "[image file: img_1]", "no role", "also kept"} {
		if !bytes.Contains(plan.ChatBody, []byte(want)) {
			t.Fatalf("unknown input content %q was lost: %s", want, plan.ChatBody)
		}
	}
}

func TestPreparePreservesUnknownToolOutputParts(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":[
			{"type":"future_output","text":"kept output"},
			{"type":"input_image","file_id":"img_1"},
			{"type":"encrypted_content","encrypted_content":"secret"}
		]}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[1].Role != "tool" || !strings.Contains(chat.Messages[1].Content, "kept output") || !strings.Contains(chat.Messages[1].Content, "[image file: img_1]") {
		t.Fatalf("unknown tool output parts were not preserved: %s", plan.ChatBody)
	}
}

func TestPrepareUsesConfigurationUpdateReasoning(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","reasoning":{"effort":"high"},"input":[
		{"type":"configuration_update","reasoning":{"effort":"low"}},
		{"type":"message","role":"user","content":"hi"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.ReasoningEffort != "low" {
		t.Fatalf("configuration_update did not override reasoning effort: %s", plan.ChatBody)
	}
}

func TestPrepareImportsAdditionalToolsFromInput(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"function","name":"extra","parameters":{"type":"object","properties":{"value":{"type":"string"}}}}
		]},
		{"type":"message","role":"user","content":"hi"}
	],"tool_choice":"auto"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != wireName("extra", "") {
		t.Fatalf("additional_tools was not imported: %s", plan.ChatBody)
	}
}

func TestPrepareSkipsMalformedToolDeclarations(t *testing.T) {
	for _, body := range []string{
		`{"model":"big-pickle","input":"hi","tools":[
			"not-a-tool",
			{"type":"future_tool","name":"future"},
			{"type":"function","name":"ok","parameters":"not-a-schema"},
			{"type":"namespace","name":"ns","tools":{"type":"function","name":"nested","parameters":{"type":"object"}}}
		]}`,
		`{"model":"big-pickle","input":"hi","tools":{"type":"function","name":"solo","parameters":{"type":"object"}}}`,
	} {
		plan, err := Prepare([]byte(body), nil)
		if err != nil {
			t.Fatalf("malformed tool declaration rejected the request: %v %s", err, body)
		}
		var chat struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
			t.Fatal(err)
		}
		if len(chat.Tools) == 0 {
			t.Fatalf("valid tools were lost: %s", plan.ChatBody)
		}
	}
}

func TestPrepareCompactionTrigger(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"user","content":"do work"},
		{"type":"compaction_trigger"}
	],"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"tool_choice":"auto"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := string(plan.ChatBody)
	if !strings.Contains(body, "compacting the conversation context") || !strings.Contains(body, "Summarize the conversation above") {
		t.Fatalf("compaction prompt was not injected: %s", body)
	}
	if !strings.Contains(body, `"tool_choice":"none"`) || strings.Contains(body, `"tools":`) {
		t.Fatalf("compaction turn still exposed tools: %s", body)
	}
}

func TestCompleteCompaction(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"user","content":"hello"},
		{"type":"compaction_trigger"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := plan.Complete(strings.NewReader(testStream()))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || response.Output[0].Type != "compaction" {
		t.Fatalf("compaction turn did not produce exactly one compaction item: %s", data)
	}
	if got := decodeCompactionSummary(json.RawMessage(string(marshal(response.Output[0].EncryptedContent)))); got != "hello" {
		t.Fatalf("compaction summary was not encoded: %q %s", got, data)
	}
}

func TestPrepareReplaysCompaction(t *testing.T) {
	summary := encodeCompactionSummary("kept context")
	body := `{"model":"big-pickle","input":[
		{"type":"compaction","encrypted_content":"` + summary + `"},
		{"type":"message","role":"user","content":"after"}
	]}`
	plan, err := Prepare([]byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.ChatBody), `Conversation summary:\nkept context`) {
		t.Fatalf("compaction summary was not replayed: %s", plan.ChatBody)
	}
	plan, err = Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"compaction","encrypted_content":"opaque-foreign-payload"},
		{"type":"message","role":"user","content":"after"}
	]}`), nil)
	if err != nil {
		t.Fatalf("foreign compaction payload rejected the request: %v", err)
	}
	if bytes.Contains(plan.ChatBody, []byte("Conversation summary")) {
		t.Fatalf("foreign compaction payload was misinterpreted: %s", plan.ChatBody)
	}
}

func TestWriteStreamCompaction(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"big-pickle","input":[
		{"type":"message","role":"user","content":"hello"},
		{"type":"compaction_trigger"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := plan.WriteStream(strings.NewReader(testStream()), &out); err != nil {
		t.Fatal(err)
	}
	_, final := parseEvents(t, out.String())
	output, _ := final["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("stream compaction produced %d items: %s", len(output), out.String())
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "compaction" {
		t.Fatalf("stream compaction item has wrong type: %s", out.String())
	}
	encrypted, _ := item["encrypted_content"].(string)
	if decodeCompactionSummary(json.RawMessage(string(marshal(encrypted)))) != "hello" {
		t.Fatalf("stream compaction payload is invalid: %s", out.String())
	}
}

func TestPrepareAppliesTruncationAuto(t *testing.T) {
	old := "OLD_CONTEXT_" + strings.Repeat("x", truncationAutoHistoryBytes+1024)
	input := []any{
		object{"type": "message", "role": "user", "content": old},
		object{"type": "message", "role": "assistant", "content": "middle"},
		object{"type": "message", "role": "user", "content": "LATEST"},
	}
	body := marshal(object{"model": "big-pickle", "truncation": "auto", "input": input})
	plan, err := Prepare(body, nil)
	if err != nil {
		t.Fatalf("truncation auto rejected the request: %v", err)
	}
	if bytes.Contains(plan.ChatBody, []byte("OLD_CONTEXT_")) {
		t.Fatalf("old history survived truncation: %d bytes", len(plan.ChatBody))
	}
	if !bytes.Contains(plan.ChatBody, []byte("LATEST")) || !bytes.Contains(plan.ChatBody, []byte("middle")) {
		t.Fatalf("recent history was lost during truncation: %s", plan.ChatBody)
	}

	plan, err = Prepare(marshal(object{"model": "big-pickle", "truncation": "future_mode", "input": input}), nil)
	if err != nil {
		t.Fatalf("unknown truncation mode rejected the request: %v", err)
	}
	if !bytes.Contains(plan.ChatBody, []byte("OLD_CONTEXT_")) {
		t.Fatalf("unknown truncation mode unexpectedly rewrote history: %d bytes", len(plan.ChatBody))
	}
}

func TestPrepareBackfillsReasoningContentForToolCalls(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("bad message count: %s", plan.ChatBody)
	}
	raw, ok := chat.Messages[1]["reasoning_content"]
	if !ok {
		t.Fatalf("tool call omitted reasoning_content: %s", plan.ChatBody)
	}
	var reasoning string
	if json.Unmarshal(raw, &reasoning) != nil || reasoning != "" {
		t.Fatalf("unexpected reasoning backfill: %s", plan.ChatBody)
	}
}

func TestPrepareReplaysReasoningOnceForGroupedToolCalls(t *testing.T) {
	plan, err := Prepare([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"think"}]},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
		{"type":"function_call","call_id":"call_2","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"one"},
		{"type":"function_call_output","call_id":"call_2","output":"two"}
	]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(plan.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 4 {
		t.Fatalf("bad message count: %s", plan.ChatBody)
	}
	if chat.Messages[1].Role != "assistant" || len(chat.Messages[1].ToolCalls) != 2 || chat.Messages[1].ReasoningContent != "think" {
		t.Fatalf("grouped tool call reasoning was not replayed once: %s", plan.ChatBody)
	}
}
