// Package responses translates the stateless Responses protocol to Chat
// Completions.
package responses

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type object = map[string]any

const compatibilityToolInstruction = "Only call the function tools supplied by the caller. Caller tools have names beginning with route_. The tools named bash, edit, glob, grep, read, skill, task, todowrite, webfetch, websearch, and write are upstream compatibility declarations and must not be called."

// Plan is immutable after Prepare and can be shared by competing attempts.
type Plan struct {
	ChatBody []byte
	Stream   bool
	Model    string
	id       string
	created  int64
	original map[string]json.RawMessage
	tools    *toolRegistry
	allowed  map[string]bool
	compat   map[string]bool
	// inputEffort is the last durable configuration_update reasoning effort
	// found in the input history. It overrides the top-level reasoning value.
	inputEffort json.RawMessage
	// compact marks a Codex remote compaction turn. The upstream response is
	// converted into one self-contained compaction item.
	compact bool
}

func Prepare(body []byte, padding json.RawMessage) (*Plan, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("A JSON Responses request is required")
	}
	p := &Plan{original: fields, tools: newToolRegistry()}
	if err := json.Unmarshal(fields["model"], &p.Model); err != nil || p.Model == "" {
		return nil, errors.New("model is required")
	}
	if raw := fields["stream"]; present(raw) {
		var stream bool
		if json.Unmarshal(raw, &stream) == nil {
			p.Stream = stream
		} else {
			// Some clients stringify booleans. Interpret the common forms
			// instead of rejecting an otherwise usable request.
			var value string
			if json.Unmarshal(raw, &value) == nil {
				p.Stream = value == "true" || value == "1"
			}
		}
	}
	// previous_response_id, conversation, store and background are accepted for
	// client compatibility. This endpoint always operates statelessly and uses
	// the full input history supplied by the caller.
	if err := p.addAdditionalTools(fields["input"]); err != nil {
		return nil, err
	}
	if raw := fields["tools"]; present(raw) {
		if err := p.tools.addList(raw, ""); err != nil {
			return nil, err
		}
	}
	// Build history first: tool_search_output and additional_tools can add
	// declarations that later calls refer to. They must be visible to
	// tool_choice and the upstream tool list.
	messages, err := p.inputMessages(fields["input"], fields["instructions"])
	if err != nil {
		return nil, err
	}
	if p.compact {
		// A compaction turn must return a summary, not continue the task or
		// call tools. Put the directive first and a concrete request last so
		// ordinary chat models produce one self-contained summary.
		messages = append([]object{{"role": "system", "content": compactionInstruction}}, messages...)
		messages = append(messages, object{"role": "user", "content": "Summarize the conversation above for context compaction. Return only the summary."})
	}
	// Responses truncation:"auto" is lowered to a bounded local replay. Any
	// layout this adapter cannot safely trim is left untouched; this path never
	// rejects an otherwise valid request.
	messages = truncateMessages(messages, fields["truncation"])
	choice, allowed, err := p.tools.choice(fields["tool_choice"])
	if err != nil {
		return nil, err
	}
	if p.compact {
		choice = "none"
		allowed = make(map[string]bool)
	}
	p.allowed = allowed
	var reserved []json.RawMessage
	p.compat = make(map[string]bool)
	if len(padding) != 0 {
		if err := json.Unmarshal(padding, &reserved); err != nil {
			return nil, err
		}
		for _, raw := range reserved {
			var tool struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(raw, &tool) == nil && tool.Function.Name != "" {
				p.compat[tool.Function.Name] = true
			}
		}
	}
	callerTools := p.tools.definitionsFor(p.allowed)
	if len(callerTools) == 0 && len(reserved) != 0 && !p.compact {
		choice = "auto"
	}
	if len(callerTools) != 0 && len(reserved) != 0 {
		messages = append([]object{{"role": "system", "content": compatibilityToolInstruction}}, messages...)
	}
	chat := map[string]json.RawMessage{
		"model": fields["model"], "messages": marshal(messages),
		"stream": json.RawMessage(`true`), "stream_options": json.RawMessage(`{"include_usage":true}`),
		"tool_choice": marshal(choice),
	}
	tools := callerTools
	// The upstream gate validates the official core declarations even when a
	// caller tool has the same original name. Keep those declarations and
	// disambiguate caller tools through their route_ aliases in the retry
	// notice instead of dropping gate-required tools. Compaction turns omit
	// tools entirely so the model returns plain summary text.
	if !p.compact {
		tools = append(tools, reserved...)
	}
	if len(tools) > 0 {
		chat["tools"] = marshal(tools)
	}
	if err := applyChatOptions(chat, fields); err != nil {
		return nil, err
	}
	if present(p.inputEffort) {
		chat["reasoning_effort"] = p.inputEffort
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	p.id, p.created = "resp_"+hex.EncodeToString(random[:]), time.Now().Unix()
	p.ChatBody, err = json.Marshal(chat)
	return p, err
}

// addAdditionalTools imports Responses Lite tool declarations embedded in the
// input history. They are ordinary tool definitions even though they arrive as
// an input item rather than in the top-level tools array.
func (p *Plan) addAdditionalTools(raw json.RawMessage) error {
	if !present(raw) {
		return nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	for _, item := range items {
		if stringField(item["type"]) != "additional_tools" || !present(item["tools"]) {
			continue
		}
		if err := p.tools.addList(item["tools"], ""); err != nil {
			return err
		}
	}
	return nil
}

// applyChatOptions maps every top-level Responses option that has a Chat
// Completions equivalent. Responses-only controls are accepted and ignored;
// they must not reject an otherwise valid request.
func applyChatOptions(chat map[string]json.RawMessage, fields map[string]json.RawMessage) error {
	for _, key := range []string{
		"temperature", "top_p", "parallel_tool_calls", "user", "prompt_cache_key",
		"seed", "presence_penalty", "frequency_penalty", "stop", "logprobs",
		"top_logprobs", "service_tier", "safety_identifier", "modalities", "audio",
		"response_format", "verbosity",
	} {
		if raw := fields[key]; present(raw) {
			chat[key] = raw
		}
	}
	if raw := fields["max_output_tokens"]; present(raw) {
		var limit int64
		if json.Unmarshal(raw, &limit) == nil && limit > 0 {
			chat["max_tokens"] = raw
		}
	}
	if raw := fields["max_completion_tokens"]; present(raw) {
		chat["max_tokens"] = raw
	}
	if raw := fields["reasoning_effort"]; present(raw) {
		chat["reasoning_effort"] = raw
	}
	if raw := fields["reasoning"]; present(raw) {
		var reasoning map[string]json.RawMessage
		if json.Unmarshal(raw, &reasoning) == nil {
			if present(reasoning["effort"]) {
				chat["reasoning_effort"] = reasoning["effort"]
			}
		} else if trimmed := bytes.TrimSpace(raw); len(trimmed) != 0 && (trimmed[0] == '"' || (trimmed[0] >= '0' && trimmed[0] <= '9')) {
			chat["reasoning_effort"] = raw
		}
	}
	if raw := fields["text"]; present(raw) {
		var text map[string]json.RawMessage
		if json.Unmarshal(raw, &text) == nil {
			if verbosity := text["verbosity"]; present(verbosity) {
				chat["verbosity"] = verbosity
			}
			if format := text["format"]; present(format) {
				if mapped := mapTextFormat(format); mapped != nil {
					chat["response_format"] = mapped
				}
			}
		}
	}
	if raw := fields["stream_options"]; present(raw) {
		options := map[string]json.RawMessage{"include_usage": json.RawMessage(`true`)}
		var incoming map[string]json.RawMessage
		if json.Unmarshal(raw, &incoming) == nil {
			if value := incoming["include_obfuscation"]; present(value) {
				options["include_obfuscation"] = value
			}
		}
		chat["stream_options"] = marshal(options)
	}
	return nil
}

// mapTextFormat lowers Responses text.format into a Chat response_format.
// Unknown format types are forwarded as-is so provider-specific formats keep
// working; malformed formats degrade to plain text instead of failing.
func mapTextFormat(raw json.RawMessage) json.RawMessage {
	var format map[string]json.RawMessage
	if json.Unmarshal(raw, &format) != nil || format == nil {
		return nil
	}
	kind := stringField(format["type"])
	switch kind {
	case "", "text":
		return nil
	case "json_object":
		return json.RawMessage(`{"type":"json_object"}`)
	case "json_schema":
		if nested := format["json_schema"]; present(nested) {
			return marshal(object{"type": "json_schema", "json_schema": nested})
		}
		if !present(format["schema"]) {
			return nil
		}
		delete(format, "type")
		if !present(format["name"]) {
			format["name"] = json.RawMessage(`"response"`)
		}
		return marshal(object{"type": "json_schema", "json_schema": format})
	default:
		return marshal(format)
	}
}

func present(value json.RawMessage) bool {
	return len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// All callers pass already-validated JSON or simple serializable values.
func marshal(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
