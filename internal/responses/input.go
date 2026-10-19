package responses

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	compactionPrefix      = "airoute.compaction.v1."
	compactionInstruction = "You are compacting the conversation context. Produce a concise, self-contained summary that preserves goals, decisions, constraints, files, commands, tool results, errors, and next steps. Return only the summary text. Do not call tools."
)

func encodeCompactionSummary(summary string) string {
	data, _ := json.Marshal(object{"summary": summary})
	return compactionPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func decodeCompactionSummary(raw json.RawMessage) string {
	value := stringField(raw)
	if !strings.HasPrefix(value, compactionPrefix) {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, compactionPrefix))
	if err != nil {
		return ""
	}
	var payload struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	return payload.Summary
}

// inputMessages converts the stateless Responses input list into Chat
// Completions messages. Reasoning items are replayed as assistant
// reasoning_content, which thinking-mode Chat upstreams require on the next
// turn. Agent messages are agent-channel input and therefore map to user text.
func (p *Plan) inputMessages(input, instructions json.RawMessage) ([]object, error) {
	messages := make([]object, 0)
	if present(instructions) {
		text, err := textContent(instructions)
		if err != nil {
			return nil, fmt.Errorf("instructions must be a string or text content array: %w", err)
		}
		messages = append(messages, object{"role": "system", "content": text})
	}
	if !present(input) {
		return nil, errors.New("input is required")
	}
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, errors.New("input is required")
	}
	if trimmed[0] == '"' {
		text, err := textContent(input)
		if err != nil {
			return nil, errors.New("input must be a string or an array of input items")
		}
		return append(messages, object{"role": "user", "content": text}), nil
	}
	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(input, &items); err != nil {
			return nil, errors.New("input must be a string or an array of input items")
		}
	case '{':
		// A single input item is not in the Responses schema but accepting it
		// keeps small clients and hand-written probes working.
		items = []json.RawMessage{input}
	default:
		return nil, errors.New("input must be a string or an array of input items")
	}
	var attachments []any
	knownCalls := make(map[string]bool)
	answeredCalls := make(map[string]bool)
	var reasoning strings.Builder
	attachReasoning := func(message object) {
		text := reasoning.String()
		if text == "" || message == nil {
			return
		}
		if existing, _ := message["reasoning_content"].(string); existing != "" {
			if existing == text {
				return
			}
			text = existing + "\n" + text
		}
		message["reasoning_content"] = text
	}
	flushAttachments := func() {
		if len(attachments) == 0 {
			return
		}
		messages = append(messages, object{"role": "user", "content": attachments})
		attachments = nil
		reasoning.Reset()
	}
	// A tool output is only structurally valid directly after its matching
	// assistant tool call. Orphans, duplicates and missing ids are replayed as
	// user text so a long-running agent does not lose an entire turn.
	toolOutput := func(callID string, output json.RawMessage) {
		if callID != "" && knownCalls[callID] && !answeredCalls[callID] {
			message, images, err := inputToolOutput(callID, output)
			if err == nil {
				answeredCalls[callID] = true
				messages = append(messages, message)
				attachments = append(attachments, images...)
				reasoning.Reset()
				return
			}
		}
		text, images, _ := toolOutputContent(output)
		if callID != "" {
			text = fmt.Sprintf("Tool output for %q:\n%s", callID, text)
		}
		if strings.TrimSpace(text) == "" {
			text = "Tool output."
		}
		messages = append(messages, object{"role": "user", "content": text})
		attachments = append(attachments, images...)
		reasoning.Reset()
	}
	appendAssistantText := func(text string) {
		if text == "" {
			return
		}
		messages = append(messages, object{"role": "assistant", "content": text})
		attachReasoning(messages[len(messages)-1])
	}
	for _, raw := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil || item == nil {
			var text string
			if json.Unmarshal(raw, &text) == nil && text != "" {
				messages = append(messages, object{"role": "user", "content": text})
				reasoning.Reset()
			}
			continue
		}
		kind := stringField(item["type"])
		if kind == "" && present(item["role"]) {
			kind = "message"
		}
		if kind != "function_call_output" && kind != "custom_tool_call_output" && kind != "tool_search_output" {
			flushAttachments()
		}
		switch kind {
		case "message":
			next, assistantIndex, err := p.inputMessage(messages, item)
			if err != nil {
				return nil, err
			}
			messages = next
			if stringField(item["role"]) == "assistant" && assistantIndex >= 0 {
				attachReasoning(next[assistantIndex])
			} else {
				reasoning.Reset()
			}
		case "agent_message":
			text, _ := agentMessageText(item["content"])
			if text != "" {
				messages = append(messages, object{"role": "user", "content": text})
			}
			reasoning.Reset()
		case "function_call", "custom_tool_call":
			next, err := p.inputFunctionCall(messages, item, kind == "custom_tool_call")
			if err != nil {
				return nil, err
			}
			messages = next
			if len(next) != 0 {
				attachReasoning(next[len(next)-1])
			}
			knownCalls[toolCallID(item)] = true
		case "local_shell_call", "tool_search_call":
			synthetic := map[string]json.RawMessage{
				"call_id": marshal(toolCallID(item)),
			}
			if kind == "local_shell_call" {
				synthetic["name"] = json.RawMessage(`"local_shell"`)
				synthetic["arguments"] = item["action"]
				if !present(synthetic["arguments"]) {
					synthetic["arguments"] = json.RawMessage(`{}`)
				}
			} else {
				synthetic["name"] = json.RawMessage(`"tool_search"`)
				synthetic["arguments"] = item["arguments"]
			}
			next, err := p.inputFunctionCall(messages, synthetic, false)
			if err != nil {
				return nil, err
			}
			messages = next
			if len(next) != 0 {
				attachReasoning(next[len(next)-1])
			}
			knownCalls[toolCallID(item)] = true
		case "function_call_output", "custom_tool_call_output":
			toolOutput(stringField(item["call_id"]), item["output"])
		case "tool_search_output":
			if present(item["tools"]) {
				_ = p.tools.addList(item["tools"], "")
			}
			toolOutput(stringField(item["call_id"]), marshal(object{
				"status":    stringField(item["status"]),
				"execution": stringField(item["execution"]),
				"tools":     rawObject(item["tools"]),
			}))
		case "web_search_call":
			appendAssistantText(webSearchText(item))
			reasoning.Reset()
		case "image_generation_call":
			appendAssistantText(imageGenerationText(item))
			if result := stringField(item["result"]); result != "" {
				url := result
				if !strings.HasPrefix(url, "data:") && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
					url = "data:image/png;base64," + url
				}
				attachments = append(attachments, object{"type": "image_url", "image_url": object{"url": url}})
			}
			reasoning.Reset()
		case "configuration_update":
			var update struct {
				Reasoning struct {
					Effort json.RawMessage `json:"effort"`
				} `json:"reasoning"`
			}
			if json.Unmarshal(marshal(item), &update) == nil && present(update.Reasoning.Effort) {
				p.inputEffort = update.Reasoning.Effort
			}
		case "compaction", "context_compaction":
			// Compaction items produced by this adapter carry a self-contained
			// summary. Foreign encrypted payloads are opaque and cannot be
			// decoded statelessly, so they are skipped rather than rejected.
			if summary := decodeCompactionSummary(item["encrypted_content"]); summary != "" {
				messages = append(messages, object{"role": "user", "content": "Conversation summary:\n" + summary})
			}
			reasoning.Reset()
		case "compaction_trigger":
			p.compact = true
		case "reasoning":
			if text := reasoningText(item); text != "" {
				if reasoning.Len() != 0 {
					reasoning.WriteString("\n")
				}
				reasoning.WriteString(text)
			}
		default:
			// Responses evolves faster than this adapter. Ignore host-only or
			// future item types instead of failing the whole long-running turn.
			continue
		}
	}
	flushAttachments()
	ensureAssistantReasoning(messages)
	return messages, nil
}

// missingReasoningPlaceholder satisfies thinking-mode replay validation when
// the original reasoning is unavailable to this stateless adapter. DeepSeek
// treats an empty string as "not passed back"; a single space is accepted.
const missingReasoningPlaceholder = " "

// ensureAssistantReasoning keeps replayed assistant turns consistent for
// thinking-mode upstreams. When tools are present, these APIs apply an
// all-or-nothing rule: once any assistant turn carries reasoning_content,
// every assistant turn must carry it. Host-synthesized calls and turns whose
// reasoning item is absent therefore need a placeholder.
func ensureAssistantReasoning(messages []object) {
	thinking := false
	for _, message := range messages {
		if stringFieldValue(message["role"]) != "assistant" {
			continue
		}
		if text, _ := message["reasoning_content"].(string); text != "" {
			thinking = true
			break
		}
		if hasToolCalls(message) {
			thinking = true
			break
		}
	}
	if !thinking {
		return
	}
	for _, message := range messages {
		if stringFieldValue(message["role"]) != "assistant" {
			continue
		}
		if text, _ := message["reasoning_content"].(string); text == "" {
			message["reasoning_content"] = missingReasoningPlaceholder
		}
	}
}

func toolCallID(item map[string]json.RawMessage) string {
	if id := stringField(item["call_id"]); id != "" {
		return id
	}
	if id := stringField(item["id"]); id != "" {
		return id
	}
	data, _ := json.Marshal(item)
	sum := sha256.Sum256(data)
	return "call_" + hex.EncodeToString(sum[:8])
}

// webSearchText keeps the search action readable when a hosted search item is
// replayed through Chat Completions, which has no web-search item type.
func webSearchText(item map[string]json.RawMessage) string {
	var action map[string]json.RawMessage
	if json.Unmarshal(item["action"], &action) != nil || action == nil {
		return ""
	}
	switch stringField(action["type"]) {
	case "search":
		if query := stringField(action["query"]); query != "" {
			return "Web search: " + query
		}
		var queries []string
		if json.Unmarshal(action["queries"], &queries) == nil && len(queries) != 0 {
			return "Web search: " + strings.Join(queries, ", ")
		}
	case "open_page":
		if url := stringField(action["url"]); url != "" {
			return "Open page: " + url
		}
	case "find_in_page":
		if url := stringField(action["url"]); url != "" {
			pattern := stringField(action["pattern"])
			return "Find in page (" + url + "): " + pattern
		}
	}
	return ""
}

func imageGenerationText(item map[string]json.RawMessage) string {
	if prompt := stringField(item["revised_prompt"]); prompt != "" {
		return "Image generated: " + prompt
	}
	return ""
}

// reasoningText extracts the replayable text from a Responses reasoning item.
// Codex sends raw reasoning_text parts back on subsequent turns; summaries are a
// fallback for providers that only expose summarized thinking.
func reasoningText(item map[string]json.RawMessage) string {
	for _, key := range []string{"content", "summary"} {
		if text := reasoningParts(item[key]); text != "" {
			return text
		}
	}
	return stringField(item["reasoning_content"])
}

func reasoningParts(raw json.RawMessage) string {
	if !present(raw) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var result strings.Builder
	for _, part := range parts {
		switch stringField(part["type"]) {
		case "reasoning_text", "input_text", "output_text", "text", "summary_text":
			result.WriteString(stringField(part["text"]))
		}
	}
	return result.String()
}

// agentMessageText accepts Codex agent_message content. Encrypted parts cannot
// be decoded by this stateless adapter, so plaintext parts are replayed and an
// encrypted-only message is omitted instead of failing the request.
func agentMessageText(raw json.RawMessage) (string, error) {
	if !present(raw) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		var single map[string]json.RawMessage
		if json.Unmarshal(raw, &single) != nil || single == nil {
			return "", nil
		}
		parts = []map[string]json.RawMessage{single}
	}
	var result strings.Builder
	for _, part := range parts {
		switch stringField(part["type"]) {
		case "input_text", "output_text", "text", "summary_text":
			result.WriteString(stringField(part["text"]))
		case "refusal":
			result.WriteString(stringField(part["refusal"]))
		case "encrypted_content":
			continue
		default:
			if value := stringField(part["text"]); value != "" {
				result.WriteString(value)
			}
		}
	}
	return result.String(), nil
}

func (p *Plan) inputMessage(messages []object, item map[string]json.RawMessage) ([]object, int, error) {
	role := stringField(item["role"])
	switch role {
	case "user", "assistant":
	case "developer", "system":
		role = "system"
	default:
		// Unknown roles and missing roles are future input channels. Treat
		// them as user text instead of rejecting the request.
		role = "user"
	}
	content, attachments, err := messageContent(item["content"], role)
	if err != nil {
		return messages, -1, err
	}
	index := -1
	if contentHasValue(content) || role == "user" {
		index = len(messages)
		messages = append(messages, object{"role": role, "content": content})
	}
	if len(attachments) != 0 {
		messages = append(messages, object{"role": "user", "content": attachments})
	}
	return messages, index, nil
}

func (p *Plan) inputFunctionCall(messages []object, item map[string]json.RawMessage, custom bool) ([]object, error) {
	callID, name := toolCallID(item), stringField(item["name"])
	if name == "" {
		name = "unknown_tool"
	}
	wire := p.wireName(name, stringField(item["namespace"]))
	var arguments string
	if custom {
		input := item["input"]
		if !present(input) {
			input = json.RawMessage(`""`)
		}
		var text string
		if json.Unmarshal(input, &text) != nil {
			text = string(bytes.TrimSpace(input))
		}
		arguments = string(marshal(object{"input": text}))
	} else {
		arguments = toolArguments(item["arguments"])
	}
	call := object{"id": callID, "type": "function", "function": object{"name": wire, "arguments": arguments}}
	// Consecutive tool calls belong to one assistant turn. A tool output or a
	// user message closes the turn.
	if n := len(messages); n > 0 && messages[n-1]["role"] == "assistant" && messages[n-1]["content"] == nil {
		if calls, ok := messages[n-1]["tool_calls"].([]any); ok {
			messages[n-1]["tool_calls"] = append(calls, call)
			return messages, nil
		}
	}
	return append(messages, object{"role": "assistant", "content": nil, "tool_calls": []any{call}}), nil
}

// toolArguments normalizes Responses function-call arguments into the JSON
// string Chat Completions expects. Some clients omit the field or send an
// object directly; both previously reached upstream as invalid arguments.
func toolArguments(raw json.RawMessage) string {
	if !present(raw) {
		return "{}"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if strings.TrimSpace(text) == "" {
			return "{}"
		}
		return text
	}
	return string(bytes.TrimSpace(raw))
}

func inputToolOutput(callID string, output json.RawMessage) (object, []any, error) {
	text, attachments, err := toolOutputContent(output)
	if err != nil {
		return nil, nil, fmt.Errorf("function call output for %q is invalid: %w", callID, err)
	}
	if text == "" && len(attachments) != 0 {
		text = "Attachment output attached."
	}
	if text == "" {
		text = "Tool completed without text output."
	}
	return object{"role": "tool", "tool_call_id": callID, "content": text}, attachments, nil
}

// toolOutputContent mirrors textContent for function outputs. Chat tool
// messages only accept text, so images are returned separately and replayed as
// a user message after the complete tool-call group. Unknown parts degrade to
// readable placeholders rather than rejecting the whole turn.
func toolOutputContent(raw json.RawMessage) (string, []any, error) {
	if !present(raw) {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(bytes.TrimSpace(raw)), nil, nil
	}
	var result strings.Builder
	var attachments []any
	for _, part := range parts {
		kind := stringField(part["type"])
		switch kind {
		case "input_text", "output_text", "text", "summary_text":
			result.WriteString(stringField(part["text"]))
		case "refusal":
			result.WriteString(stringField(part["refusal"]))
		case "encrypted_content":
			// Encrypted-only history cannot be decoded by a stateless adapter.
			continue
		case "input_image", "input_file", "input_audio":
			value, include, err := messagePart(part)
			if err != nil || !include {
				result.WriteString(contentPlaceholder(part))
				continue
			}
			if valueObject, ok := value.(object); ok && stringFieldValue(valueObject["type"]) == "text" {
				result.WriteString(stringFieldValue(valueObject["text"]))
				continue
			}
			attachments = append(attachments, value)
		default:
			if value := stringField(part["text"]); value != "" {
				result.WriteString(value)
			} else {
				result.WriteString(contentPlaceholder(part))
			}
		}
	}
	return result.String(), attachments, nil
}

func stringFieldValue(value any) string {
	text, _ := value.(string)
	return text
}

func (p *Plan) wireName(name, namespace string) string {
	if t := p.tools.byName[toolKey(name, namespace)]; t != nil {
		return t.Wire
	}
	return name
}

// textContent accepts a plain string or an array of Responses content parts.
// Function outputs and instructions use this narrower helper because they have
// no image or file equivalents in a Chat tool message.
func textContent(raw json.RawMessage) (string, error) {
	if !present(raw) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(bytes.TrimSpace(raw)), nil
	}
	var result strings.Builder
	for _, part := range parts {
		kind := stringField(part["type"])
		switch kind {
		case "input_text", "output_text", "text", "summary_text":
			result.WriteString(stringField(part["text"]))
		case "refusal":
			result.WriteString(stringField(part["refusal"]))
		default:
			if value := stringField(part["text"]); value != "" {
				result.WriteString(value)
			} else {
				result.WriteString(contentPlaceholder(part))
			}
		}
	}
	return result.String(), nil
}

func messageContent(raw json.RawMessage, role string) (any, []any, error) {
	if !present(raw) {
		return nil, nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		var single map[string]json.RawMessage
		if json.Unmarshal(raw, &single) != nil || single == nil {
			return string(bytes.TrimSpace(raw)), nil, nil
		}
		parts = []map[string]json.RawMessage{single}
	}
	converted := make([]any, 0, len(parts))
	var attachments []any
	for _, part := range parts {
		kind := stringField(part["type"])
		if role != "user" && isAttachmentContentPart(kind) {
			// Chat only accepts attachments in user content. Replay valid
			// assistant/system attachments as a following user message; keep
			// malformed or opaque attachments as text in the original role.
			value, include, err := messagePart(part)
			if err == nil && include {
				if valueObject, ok := value.(object); ok && stringFieldValue(valueObject["type"]) == "text" {
					converted = append(converted, value)
				} else {
					attachments = append(attachments, value)
				}
				continue
			}
			if placeholder := contentPlaceholder(part); placeholder != "" {
				converted = append(converted, object{"type": "text", "text": placeholder})
			}
			continue
		}
		if role != "user" && !isTextContentPart(kind) {
			if placeholder := contentPlaceholder(part); placeholder != "" {
				converted = append(converted, object{"type": "text", "text": placeholder})
			}
			continue
		}
		value, include, err := messagePart(part)
		if err != nil || !include {
			if placeholder := contentPlaceholder(part); placeholder != "" {
				converted = append(converted, object{"type": "text", "text": placeholder})
			}
			continue
		}
		converted = append(converted, value)
	}
	return converted, attachments, nil
}

func contentHasValue(content any) bool {
	switch value := content.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) != 0
	default:
		return true
	}
}

func isAttachmentContentPart(kind string) bool {
	switch kind {
	case "input_image", "input_file", "input_audio":
		return true
	}
	return false
}

func isTextContentPart(kind string) bool {
	switch kind {
	case "input_text", "output_text", "text", "summary_text", "refusal":
		return true
	}
	return false
}

func contentPlaceholder(part map[string]json.RawMessage) string {
	kind := stringField(part["type"])
	if text := stringField(part["text"]); text != "" {
		return text
	}
	if refusal := stringField(part["refusal"]); refusal != "" {
		return refusal
	}
	switch kind {
	case "input_image":
		if fileID := stringField(part["file_id"]); fileID != "" {
			return "[image file: " + fileID + "]"
		}
		return "[image attachment]"
	case "input_file":
		if name := stringField(part["filename"]); name != "" {
			return "[file attachment: " + name + "]"
		}
		if fileID := stringField(part["file_id"]); fileID != "" {
			return "[file attachment: " + fileID + "]"
		}
		return "[file attachment]"
	case "input_audio":
		return "[audio attachment]"
	case "encrypted_content":
		return ""
	case "":
		return "[content part]"
	default:
		return "[" + kind + " content]"
	}
}

func messagePart(part map[string]json.RawMessage) (any, bool, error) {
	kind := stringField(part["type"])
	switch kind {
	case "input_text", "output_text", "text", "summary_text":
		return object{"type": "text", "text": stringField(part["text"])}, true, nil
	case "refusal":
		return object{"type": "text", "text": stringField(part["refusal"])}, true, nil
	case "input_image":
		url, detail := imageURL(part)
		if detail == "" {
			detail = stringField(part["detail"])
		}
		detail = normalizeImageDetail(detail)
		if url == "" {
			return object{"type": "text", "text": contentPlaceholder(part)}, true, nil
		}
		if detail != "" {
			return object{"type": "image_url", "image_url": object{"url": url, "detail": detail}}, true, nil
		}
		return object{"type": "image_url", "image_url": object{"url": url}}, true, nil
	case "input_file":
		file := object{}
		for _, key := range []string{"file_data", "file_id", "filename"} {
			if present(part[key]) {
				var value string
				if json.Unmarshal(part[key], &value) == nil && value != "" {
					file[key] = value
				}
			}
		}
		if len(file) == 0 {
			return object{"type": "text", "text": contentPlaceholder(part)}, true, nil
		}
		return object{"type": "file", "file": file}, true, nil
	case "input_audio":
		if audio := rawObject(part["input_audio"]); audio != nil {
			return object{"type": "input_audio", "input_audio": audio}, true, nil
		}
		return object{"type": "text", "text": contentPlaceholder(part)}, true, nil
	default:
		return object{"type": "text", "text": contentPlaceholder(part)}, true, nil
	}
}

func imageURL(part map[string]json.RawMessage) (string, string) {
	raw := part["image_url"]
	if !present(raw) {
		return "", ""
	}
	var url string
	if json.Unmarshal(raw, &url) == nil {
		return url, ""
	}
	var image struct {
		URL    string `json:"url"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &image) == nil {
		return image.URL, image.Detail
	}
	return "", ""
}

func normalizeImageDetail(detail string) string {
	switch detail {
	case "", "low", "high", "auto":
		return detail
	case "original":
		// Codex view_image uses original to request the unresized image. Chat
		// Completions has no equivalent, so high detail is the closest value.
		return "high"
	default:
		// New detail levels should not reject an otherwise valid request.
		return ""
	}
}

func rawObject(raw json.RawMessage) any {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func stringField(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}
