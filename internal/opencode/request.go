package opencode

import (
	_ "embed"
	"encoding/json"
	"errors"
)

// toolsJSON is the exact tool array emitted by the OpenCode 1.19.4 CLI. The
// inference free tier validates every recognised tool against these
// descriptions and parameter schemas, so stubs or generic schemas are rejected.
//
//go:embed tools.json
var toolsJSON []byte

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	raw         json.RawMessage
}

var toolNames = []string{"bash", "edit", "glob", "grep", "read", "skill", "task", "todowrite", "webfetch", "websearch", "write"}

// coreToolNames are the tools the agent-shaped free-tier gate requires.
var coreToolNames = []string{"bash", "edit", "glob", "grep", "read"}

var toolDefinitions = func() map[string]toolDefinition {
	var entries []json.RawMessage
	if err := json.Unmarshal(toolsJSON, &entries); err != nil {
		panic(err)
	}
	definitions := make(map[string]toolDefinition, len(entries))
	for _, raw := range entries {
		var entry struct {
			Function toolDefinition `json:"function"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			panic(err)
		}
		entry.Function.raw = raw
		definitions[entry.Function.Name] = entry.Function
	}
	return definitions
}()

func chatTool(name string) json.RawMessage {
	definition, ok := toolDefinitions[name]
	if !ok {
		return nil
	}
	return definition.raw
}

var chatTools = func() json.RawMessage {
	return encodeAgentTools(toolNames)
}()

var chatCoreTools = func() json.RawMessage {
	return encodeAgentTools(coreToolNames)
}()

func encodeAgentTools(names []string) json.RawMessage {
	tools := make([]json.RawMessage, 0, len(names))
	for _, name := range names {
		if tool := chatTool(name); tool != nil {
			tools = append(tools, tool)
		}
	}
	data, err := json.Marshal(tools)
	if err != nil {
		panic(err)
	}
	return data
}

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

func prepareChat(body []byte, streaming, compatibility bool) (chatRequest, error) {
	body = stripClientMetadata(body)
	request := chatRequest{body: body, streaming: streaming}
	if !compatibility {
		return request, nil
	}
	var err error
	body, err = injectAgentPrompt(body)
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
	}
	tools, err := json.Marshal(ensureAgentTools(callerTools))
	if err != nil {
		return request, err
	}
	fields["tools"] = tools
	if len(callerTools) == 0 {
		fields["tool_choice"] = json.RawMessage(`"auto"`)
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
	if len(tools) == 0 {
		expanded := make([]json.RawMessage, 0, len(toolNames))
		for _, name := range toolNames {
			expanded = append(expanded, chatTool(name))
		}
		return expanded
	}
	present := make(map[string]bool, len(tools))
	for index, raw := range tools {
		var entry struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Function.Name == "" {
			continue
		}
		name := entry.Function.Name
		present[name] = true
		if _, known := toolDefinitions[name]; known {
			tools[index] = chatTool(name)
		}
	}
	for _, name := range coreToolNames {
		if !present[name] {
			tools = append(tools, chatTool(name))
		}
	}
	return tools
}
