package opencode

import (
	_ "embed"
	"encoding/json"
	"errors"
	"sync"
)

// toolsJSON is the exact tool array emitted by the OpenCode 1.19.4 CLI. The
// inference free tier validates every recognised tool against these
// descriptions and parameter schemas, so stubs or generic schemas are rejected.
//
//go:embed tools.json
var toolsJSON []byte

type toolHeader struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// coreToolNames are the tools the agent-shaped free-tier gate requires.
var coreToolNames = []string{"bash", "edit", "glob", "grep", "read"}

var toolDefinitions = sync.OnceValue(func() map[string]json.RawMessage {
	var entries []json.RawMessage
	if err := json.Unmarshal(toolsJSON, &entries); err != nil {
		panic(err)
	}
	definitions := make(map[string]json.RawMessage, len(entries))
	for _, raw := range entries {
		var entry toolHeader
		if err := json.Unmarshal(raw, &entry); err != nil {
			panic(err)
		}
		definitions[entry.Function.Name] = raw
	}
	return definitions
})

var chatCoreTools = sync.OnceValue(func() json.RawMessage {
	data, err := json.Marshal(ensureAgentTools(nil))
	if err != nil {
		panic(err)
	}
	return data
})

func requestHasTools(body []byte) bool {
	var fields struct {
		Tools []json.RawMessage `json:"tools"`
	}
	return json.Unmarshal(body, &fields) == nil && len(fields.Tools) != 0
}

type chatRequest struct {
	body      []byte
	streaming bool
	collect   bool
}

func (workspace promptWorkspace) prepareChat(body []byte, streaming, compatibility bool) (chatRequest, error) {
	body = stripClientMetadata(body)
	request := chatRequest{body: body, streaming: streaming}
	if !compatibility {
		return request, nil
	}
	var err error
	body, err = workspace.injectAgentPrompt(body)
	if err != nil {
		return request, errors.New("messages must be an array")
	}
	request.body = body
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return request, errors.New("A JSON chat request is required")
	}
	// Legacy function-calling payloads keep their original contract.
	if len(fields["functions"]) != 0 || len(fields["function_call"]) != 0 {
		return request, nil
	}
	var callerTools []json.RawMessage
	if raw := fields["tools"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &callerTools); err != nil {
			return request, errors.New("tools must be an array")
		}
	}
	if len(callerTools) == 0 {
		if raw := fields["tool_choice"]; len(raw) != 0 && string(raw) != "null" {
			var choice string
			if json.Unmarshal(raw, &choice) != nil || (choice != "none" && choice != "auto") {
				return request, errors.New("tool_choice requires caller-supplied tools")
			}
		}
		fields["tools"] = toolsJSON
		fields["tool_choice"] = json.RawMessage(`"auto"`)
	} else {
		fields["tools"], err = json.Marshal(ensureAgentTools(callerTools))
		if err != nil {
			return request, err
		}
	}
	// The free tier only serves agent-shaped streaming requests.
	fields["stream"] = json.RawMessage(`true`)
	// Request usage in the final SSE event so ordinary JSON callers keep it.
	var options map[string]json.RawMessage
	if raw := fields["stream_options"]; len(raw) != 0 {
		if json.Unmarshal(raw, &options) != nil {
			return request, errors.New("stream_options must be an object")
		}
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	options["include_usage"] = json.RawMessage(`true`)
	fields["stream_options"], _ = json.Marshal(options)
	request.body, err = json.Marshal(fields)
	request.streaming, request.collect = true, !streaming
	return request, err
}

// OpenCode rejects client-supplied metadata even though the public Chat and
// Responses APIs accept it. Metadata is diagnostic only, so drop the top-level
// fields while leaving requests without them byte-for-byte unchanged.
func stripClientMetadata(body []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return body
	}
	if _, ok := fields["metadata"]; !ok {
		if _, ok := fields["client_metadata"]; !ok {
			return body
		}
	}
	delete(fields, "metadata")
	delete(fields, "client_metadata")
	stripped, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return stripped
}

// ensureAgentTools rewrites recognised caller tools to their genuine 1.19.4
// definitions and appends any missing core tools. Caller tools are preserved.
func ensureAgentTools(tools []json.RawMessage) []json.RawMessage {
	definitions := toolDefinitions()
	present := make(map[string]bool, len(tools))
	for index, raw := range tools {
		var entry toolHeader
		if json.Unmarshal(raw, &entry) != nil || entry.Function.Name == "" {
			continue
		}
		name := entry.Function.Name
		present[name] = true
		if definition, known := definitions[name]; known {
			tools[index] = definition
		}
	}
	for _, name := range coreToolNames {
		if !present[name] {
			tools = append(tools, definitions[name])
		}
	}
	return tools
}
