package opencode

import (
	"bytes"
	"encoding/json"
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
		for _, key := range []string{"messages", "vendor"} {
			if !bytes.Equal(before[key], after[key]) {
				t.Fatalf("changed %s: %s", key, after[key])
			}
		}
		tools := decodeTools(t, after["tools"])
		if len(tools) != 12 || string(after["tool_choice"]) != `"none"` || string(after["stream"]) != "true" {
			t.Fatalf("missing agent-shaped compatibility declarations: %s", prepared.body)
		}
		shell, ok := tools["shell"]
		if !ok || shell.Function.Description == "" || shell.Function.Description == "Reserved tool. Do not call." || shell.Function.Parameters.Properties["command"] == nil {
			t.Fatalf("shell does not carry the genuine 2.x schema: %s", prepared.body)
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
	if read := tools["read"]; read.Function.Description == "stub" || read.Function.Parameters.Properties["path"] == nil {
		t.Fatalf("known caller tool was not upgraded: %s", prepared.body)
	}
	if shell := tools["shell"]; shell.Function.Parameters.Properties["command"] == nil {
		t.Fatalf("missing genuine core tool: %s", prepared.body)
	}
}

func TestPrepareChatPassThrough(t *testing.T) {
	for _, test := range []struct {
		body   string
		compat bool
	}{
		{`{ "model": "big-pickle", "messages": [] }`, false},
		{`{ "functions": [{"name":"legacy"}], "function_call":"auto" }`, true},
	} {
		prepared, err := prepareChat([]byte(test.body), false, test.compat)
		if err != nil || string(prepared.body) != test.body || prepared.streaming || prepared.collect {
			t.Fatalf("caller contract changed: %+v %v", prepared, err)
		}
	}
	for _, body := range []string{`null`, `[]`, `{"tools":{}}`, `{"tool_choice":"required"}`, `{"tool_choice":{"type":"function"}}`, `{"stream_options":[]}`} {
		if _, err := prepareChat([]byte(body), false, true); err == nil {
			t.Errorf("invalid compatibility request accepted: %s", body)
		}
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
