package opencode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type decodedTool struct {
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"parameters"`
	} `json:"function"`
}

func decodeTools(t *testing.T, raw json.RawMessage) map[string]decodedTool {
	t.Helper()
	var tools []decodedTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	byName := make(map[string]decodedTool, len(tools))
	for _, tool := range tools {
		byName[tool.Function.Name] = tool
	}
	return byName
}

func decodedMessages(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	return messages
}

func assertAgentPrompt(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if !hasAgentPrompt(raw) {
		t.Fatalf("missing injected system prompt: %s", raw)
	}
}

func hasAgentPrompt(raw json.RawMessage) bool {
	var messages []json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return false
	}
	if len(messages) == 0 {
		return false
	}
	var prompt struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(messages[0], &prompt); err != nil {
		return false
	}
	var text string
	if json.Unmarshal(prompt.Content, &text) != nil {
		return false
	}
	return prompt.Role == "system" && bytes.Contains([]byte(text), []byte("You are opencode, an interactive CLI tool")) && bytes.Contains([]byte(text), []byte("<env>"))
}

func TestPrepareChatCompatibility(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}},{"type":"input_audio","input_audio":{"data":"YQ==","format":"wav"}}]}],"vendor":{"large":9007199254740993},"stream_options":{"vendor_option":true}}`)
		prepared, err := prepareChat(body, streaming, true)
		if err != nil || !prepared.streaming || prepared.collect == streaming {
			t.Fatalf("bad compatibility plan: %+v %v", prepared, err)
		}
		var before, after map[string]json.RawMessage
		json.Unmarshal(body, &before)
		json.Unmarshal(prepared.body, &after)
		if !bytes.Equal(before["vendor"], after["vendor"]) {
			t.Fatalf("changed vendor: %s", after["vendor"])
		}
		assertAgentPrompt(t, after["messages"])
		beforeMessages := decodedMessages(t, before["messages"])
		afterMessages := decodedMessages(t, after["messages"])
		if len(afterMessages) != len(beforeMessages)+1 {
			t.Fatalf("caller messages were not preserved: %s", after["messages"])
		}
		for index := range beforeMessages {
			if !bytes.Equal(beforeMessages[index], afterMessages[index+1]) {
				t.Fatalf("caller message %d changed: %s", index, afterMessages[index+1])
			}
		}
		tools := decodeTools(t, after["tools"])
		if len(tools) != 11 || string(after["tool_choice"]) != `"auto"` || string(after["stream"]) != "true" || string(after["max_tokens"]) != "32000" {
			t.Fatalf("missing agent-shaped compatibility declarations: %s", prepared.body)
		}
		bash, ok := tools["bash"]
		if !ok || bash.Function.Description == "" || bash.Function.Parameters.Properties["command"] == nil {
			t.Fatalf("bash does not carry the genuine 1.19.4 schema: %s", prepared.body)
		}
		if !streaming {
			var options map[string]json.RawMessage
			json.Unmarshal(after["stream_options"], &options)
			if string(options["vendor_option"]) != "true" || string(options["include_usage"]) != "true" {
				t.Fatalf("stream options lost: %s", after["stream_options"])
			}
		}
	}
}

func TestPrepareChatMergesAgentTools(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[],"tools":[{"type":"function","function":{"name":"mine","description":"keep","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}},{"type":"function","function":{"name":"read","description":"stub","parameters":{"type":"object","properties":{}}}}],"tool_choice":"required"}`)
	prepared, err := prepareChat(body, false, true)
	if err != nil || !prepared.streaming || !prepared.collect {
		t.Fatalf("bad shaped plan: %+v %v", prepared, err)
	}
	var after map[string]json.RawMessage
	json.Unmarshal(prepared.body, &after)
	tools := decodeTools(t, after["tools"])
	if len(tools) != 6 || string(after["tool_choice"]) != `"required"` {
		t.Fatalf("caller tools not merged: %s", prepared.body)
	}
	if mine := tools["mine"]; mine.Function.Description != "keep" {
		t.Fatalf("caller tool changed: %s", prepared.body)
	}
	if read := tools["read"]; read.Function.Description == "stub" || read.Function.Parameters.Properties["filePath"] == nil {
		t.Fatalf("known caller tool was not upgraded: %s", prepared.body)
	}
	if bash := tools["bash"]; bash.Function.Parameters.Properties["command"] == nil {
		t.Fatalf("missing genuine core tool: %s", prepared.body)
	}
}

func TestPrepareChatPassThrough(t *testing.T) {
	body := `{ "model": "big-pickle", "messages": [] }`
	prepared, err := prepareChat([]byte(body), false, false)
	if err != nil || string(prepared.body) != body || prepared.streaming || prepared.collect {
		t.Fatalf("caller contract changed: %+v %v", prepared, err)
	}
	legacy := []byte(`{ "functions": [{"name":"legacy"}], "function_call":"auto" }`)
	prepared, err = prepareChat(legacy, false, true)
	if err != nil || prepared.streaming || prepared.collect {
		t.Fatalf("legacy compatibility changed: %+v %v", prepared, err)
	}
	var legacyFields map[string]json.RawMessage
	if json.Unmarshal(prepared.body, &legacyFields) != nil {
		t.Fatal(err)
	}
	if string(legacyFields["function_call"]) != `"auto"` || !hasAgentPrompt(legacyFields["messages"]) {
		t.Fatalf("legacy request was not completed correctly: %s", prepared.body)
	}
	for _, body := range []string{`null`, `[]`, `{"tools":{}}`, `{"tool_choice":"required"}`, `{"tool_choice":{"type":"function"}}`, `{"stream_options":[]}`} {
		if _, err := prepareChat([]byte(body), false, true); err == nil {
			t.Errorf("invalid compatibility request accepted: %s", body)
		}
	}
}

func TestPrepareChatInjectsOfficialPrompt(t *testing.T) {
	prepared, err := prepareChat([]byte(`{"model":"big-pickle","messages":[{"role":"system","content":"client instructions"},{"role":"user","content":"hi"}]}`), true, true)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(prepared.body, &fields); err != nil {
		t.Fatal(err)
	}
	messages := decodedMessages(t, fields["messages"])
	if len(messages) != 2 || !hasAgentPrompt(fields["messages"]) {
		t.Fatalf("official prompt was not merged: %s", messages)
	}
	var system struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(messages[0], &system) != nil || !strings.Contains(system.Content, "client instructions") || !strings.Contains(system.Content, "\nclient instructions\nSkills provide") || string(messages[1]) != `{"role":"user","content":"hi"}` {
		t.Fatalf("caller messages changed: %s", messages)
	}
}

func TestPrepareChatStripsUnsupportedClientMetadata(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[],"metadata":{"trace":"one"},"client_metadata":{"trace":"two"},"vendor":{"keep":true}}`)
	for _, compatibility := range []bool{false, true} {
		prepared, err := prepareChat(body, false, compatibility)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(prepared.body, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["metadata"]; ok {
			t.Fatalf("metadata was forwarded upstream: %s", prepared.body)
		}
		if _, ok := fields["client_metadata"]; ok {
			t.Fatalf("client_metadata was forwarded upstream: %s", prepared.body)
		}
		if string(fields["vendor"]) != `{"keep":true}` {
			t.Fatalf("unrelated request fields changed: %s", prepared.body)
		}
	}
}

func TestPreparedChatMatchesCaptured1194Contract(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"Reply exactly OK. Do not use tools."}]}`)
	prepared, err := prepareChat(body, true, true)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(prepared.body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["max_tokens"]) != "32000" || string(fields["stream"]) != "true" || string(fields["tool_choice"]) != `"auto"` {
		t.Fatalf("wrong captured request controls: %s", prepared.body)
	}
	if _, ok := fields["max_completion_tokens"]; ok {
		t.Fatal("1.19.4 body must use max_tokens")
	}
	var options map[string]bool
	if json.Unmarshal(fields["stream_options"], &options) != nil || !options["include_usage"] {
		t.Fatalf("missing stream usage: %s", fields["stream_options"])
	}
	var messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) != 2 || messages[0].Role != "system" || messages[1].Role != "user" {
		t.Fatalf("wrong captured message shape: %s", fields["messages"])
	}
	if !strings.HasPrefix(messages[0].Content, agentSystemPrompt+"\nYou are powered by the model named big-pickle. The exact model ID is opencode/big-pickle\n") || !strings.Contains(messages[0].Content, agentSkills) {
		t.Fatal("system prompt is not the 1.19.4 default/environment/skills assembly")
	}
	if messages[1].Content != "Reply exactly OK. Do not use tools." {
		t.Fatalf("user message changed: %q", messages[1].Content)
	}
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(fields["tools"], &tools) != nil || len(tools) != len(toolNames) {
		t.Fatalf("wrong captured tools: %s", fields["tools"])
	}
	for index, name := range toolNames {
		if tools[index].Function.Name != name {
			t.Fatalf("tool %d=%q, want %q", index, tools[index].Function.Name, name)
		}
	}
}

func TestPrepareChatNormalizesTokenLimits(t *testing.T) {
	for _, body := range []string{
		`{"model":"big-pickle","messages":[],"max_completion_tokens":123}`,
		`{"model":"big-pickle","messages":[],"max_output_tokens":123}`,
	} {
		prepared, err := prepareChat([]byte(body), true, true)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(prepared.body, &fields) != nil || string(fields["max_tokens"]) != "123" {
			t.Fatalf("token limit was not normalized: %s", prepared.body)
		}
		if _, ok := fields["max_completion_tokens"]; ok {
			t.Fatalf("max_completion_tokens leaked: %s", prepared.body)
		}
		if _, ok := fields["max_output_tokens"]; ok {
			t.Fatalf("max_output_tokens leaked: %s", prepared.body)
		}
	}
}
