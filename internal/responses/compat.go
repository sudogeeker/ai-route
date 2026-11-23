package responses

import (
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
	var seen bytes.Buffer
	events := newChatStream(io.TeeReader(stream, &seen))
	replay := io.MultiReader(&seen, stream)
	calls := make(map[int]*pendingCall)
	textBytes := 0
	for {
		chunk, err := events.next()
		if err != nil {
			return nil, nil, err
		}
		if chunk == nil {
			return p.compatDecision(calls, replay)
		}
		for _, choice := range chunk.Choices {
			if choice.Index != nil && *choice.Index != 0 {
				continue
			}
			for position, call := range choice.Delta.ToolCalls {
				index, name, arguments, _ := call.fields(position)
				pending := calls[index]
				if pending == nil {
					pending = &pendingCall{}
					calls[index] = pending
				}
				if call.ID != "" {
					pending.id = call.ID
				}
				if name != "" {
					pending.name = name
				}
				pending.arguments.WriteString(arguments)
			}
			textBytes += len(deltaText(choice.Delta.Content))
			if len(calls) == 0 && textBytes >= compatTextLookahead {
				// No tool call has begun and the text prefix is large enough to
				// be an ordinary answer. Stream the buffered bytes and continue.
				return nil, replay, nil
			}
			if present(choice.FinishReason) {
				return p.compatDecision(calls, replay)
			}
		}
	}
}

func (p *Plan) compatDecision(calls map[int]*pendingCall, replay io.Reader) ([]CompatCall, io.Reader, error) {
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
	return nil, replay, nil
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
