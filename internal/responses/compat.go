package responses

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// CompatCall is a tool call emitted for a declaration that was added only to
// satisfy the upstream compatibility gate. It cannot be resolved to a caller
// tool, so the adapter returns an error result to the model instead.
type CompatCall struct {
	ID        string
	Name      string
	Arguments string
}

type recordingReader struct {
	reader io.Reader
	seen   bytes.Buffer
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.seen.Write(p[:n])
	}
	return n, err
}

func (r *recordingReader) replay() io.Reader {
	return io.MultiReader(bytes.NewReader(r.seen.Bytes()), r.reader)
}

type pendingCall struct {
	id        string
	name      string
	arguments strings.Builder
}

// compatTextLookahead bounds how much text is buffered while checking for a
// compatibility-only tool call. Some models emit a short preamble before the
// actual tool call; returning at the first text delta would let that call slip
// through and surface later as an undeclared tool.
const compatTextLookahead = 256

// ProbeCompat reads the start of a Chat stream and decides whether it calls a
// compatibility-only tool. It buffers a small text prefix before treating the
// response as ordinary text, so a tool call emitted after a preamble is still
// intercepted. When no retry is needed the returned reader replays every
// consumed byte.
func (p *Plan) ProbeCompat(stream io.Reader) ([]CompatCall, io.Reader, error) {
	if len(p.compat) == 0 {
		return nil, stream, nil
	}
	recorder := &recordingReader{reader: stream}
	scanner := bufio.NewScanner(recorder)
	scanner.Buffer(make([]byte, 4096), maxSSELine)
	var data []string
	event, eventBytes, totalBytes := "", 0, 0
	calls := make(map[int]*pendingCall)
	startedTools := false
	textBytes := 0
	for scanner.Scan() {
		line := scanner.Text()
		eventBytes += len(line) + 1
		totalBytes += len(line) + 1
		if eventBytes > maxSSELine {
			return nil, nil, errors.New("Chat stream event exceeds buffer limit")
		}
		if totalBytes > maxResponseStream {
			return nil, nil, errors.New("Chat stream response exceeds buffer limit")
		}
		if line != "" {
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
			case "event":
				event = value
			}
			continue
		}
		if event == "error" {
			return nil, nil, errors.New("Chat stream returned an error event")
		}
		payload := strings.Join(data, "\n")
		data, event, eventBytes = nil, "", 0
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return p.compatDecision(calls, recorder)
		}
		var chunk struct {
			Type    json.RawMessage `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index        *int            `json:"index"`
				FinishReason json.RawMessage `json:"finish_reason"`
				Delta        struct {
					Content   json.RawMessage `json:"content"`
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string          `json:"name"`
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
						Custom struct {
							Name  string          `json:"name"`
							Input json.RawMessage `json:"input"`
						} `json:"custom"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, nil, fmt.Errorf("invalid Chat stream JSON: %w", err)
		}
		if present(chunk.Error) || stringField(chunk.Type) == "error" {
			return nil, nil, errors.New("Chat stream returned an error payload")
		}
		for _, choice := range chunk.Choices {
			if choice.Index != nil && *choice.Index != 0 {
				continue
			}
			for position, call := range choice.Delta.ToolCalls {
				index := position
				if call.Index != nil {
					index = *call.Index
				}
				pending := calls[index]
				if pending == nil {
					pending = &pendingCall{}
					calls[index] = pending
				}
				if call.ID != "" {
					pending.id = call.ID
				}
				name, arguments := call.Function.Name, rawText(call.Function.Arguments)
				if call.Custom.Name != "" {
					name, arguments = call.Custom.Name, rawText(call.Custom.Input)
				}
				if name != "" {
					pending.name = name
				}
				pending.arguments.WriteString(arguments)
				startedTools = true
			}
			if text := deltaText(choice.Delta.Content); text != "" {
				textBytes += len(text)
			}
			if !startedTools && textBytes >= compatTextLookahead {
				// No tool call has begun and the text prefix is large enough to
				// be an ordinary answer. Stream the buffered bytes and continue.
				return nil, recorder.replay(), nil
			}
			if present(choice.FinishReason) {
				return p.compatDecision(calls, recorder)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return nil, nil, errors.New("Chat stream ended before [DONE]")
}

func (p *Plan) compatDecision(calls map[int]*pendingCall, recorder *recordingReader) ([]CompatCall, io.Reader, error) {
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var compat []CompatCall
	for _, index := range indexes {
		call := calls[index]
		if !p.compat[call.name] {
			continue
		}
		id := call.id
		if id == "" {
			id = randomID("call")
		}
		arguments := call.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		compat = append(compat, CompatCall{ID: id, Name: call.name, Arguments: arguments})
	}
	if len(compat) != 0 {
		return compat, nil, nil
	}
	return nil, recorder.replay(), nil
}

// CallerTools lists the upstream wire names of the caller tools that were sent
// in stable order. Retry notices must use these names because callers see the
// aliased functions, not their original names.
func (p *Plan) CallerTools() []string {
	names := make([]string, 0, len(p.tools.tools))
	for _, tool := range p.tools.tools {
		if !p.allowed[tool.Wire] {
			continue
		}
		names = append(names, tool.Wire)
	}
	sort.Strings(names)
	return names
}

// AppendCompatRetry appends a rejected assistant tool call and a tool error so
// the model can retry with a caller-declared tool.
func AppendCompatRetry(body []byte, calls []CompatCall, callerTools []string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("A JSON chat request is required")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(fields["messages"], &messages); err != nil {
		return nil, errors.New("Chat messages must be an array")
	}
	toolCalls := make([]object, 0, len(calls))
	results := make([]json.RawMessage, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, object{
			"id": call.ID, "type": "function",
			"function": object{"name": call.Name, "arguments": call.Arguments},
		})
		notice := compatibilityNotice(call.Name, callerTools)
		results = append(results, marshal(object{"role": "tool", "tool_call_id": call.ID, "content": notice}))
	}
	// Thinking-mode Chat upstreams reject replayed assistant tool calls that
	// omit reasoning_content, including calls synthesized for this retry. An
	// empty string counts as omitted; a single space is the accepted fallback.
	messages = append(messages, marshal(object{
		"role": "assistant", "content": nil, "reasoning_content": missingReasoningPlaceholder, "tool_calls": toolCalls,
	}))
	messages = append(messages, results...)
	fields["messages"], _ = json.Marshal(messages)
	return json.Marshal(fields)
}

func compatibilityNotice(name string, callerTools []string) string {
	if len(callerTools) == 0 {
		return fmt.Sprintf("The tool %q is unavailable on this compatibility endpoint, and no caller tools are declared. Answer directly without calling tools.", name)
	}
	return fmt.Sprintf("The tool %q is unavailable on this compatibility endpoint. Do not call compatibility tools. Use only these caller-provided tools: %s.", name, strings.Join(callerTools, ", "))
}
