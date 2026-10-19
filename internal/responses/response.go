package responses

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	maxSSELine        = 8 << 20
	maxResponseStream = 64 << 20
)

// WriteStream converts a Chat Completions SSE stream into Responses API SSE
// events. The upstream reader must start at the first validated chat event; the
// OpenCode race has already rejected an error or empty prelude.
func (p *Plan) WriteStream(upstream io.Reader, w io.Writer) error {
	state := &streamState{plan: p, w: w}
	if err := state.emit("response.created", object{"response": p.responseObject("in_progress", []object{}, nil, nil)}); err != nil {
		return err
	}
	if err := state.emit("response.in_progress", object{"response": p.responseObject("in_progress", []object{}, nil, nil)}); err != nil {
		return err
	}
	if err := consumeChat(upstream, state, true); err != nil {
		response := p.responseObject("failed", state.output, state.usageObject(), nil)
		response["error"] = object{"code": "server_error", "message": err.Error()}
		_ = state.emit("response.failed", object{"response": response})
		return err
	}
	state.finish()
	event := "response.completed"
	if state.status == "incomplete" {
		event = "response.incomplete"
	}
	return state.emit(event, object{"response": p.responseObject(state.status, state.output, state.usageObject(), state.incompleteDetails())})
}

// Complete converts a Chat Completions SSE stream into one Responses object.
func (p *Plan) Complete(upstream io.Reader) ([]byte, error) {
	state := &streamState{plan: p}
	if err := consumeChat(upstream, state, false); err != nil {
		return nil, err
	}
	state.finish()
	return json.Marshal(p.responseObject(state.status, state.output, state.usageObject(), state.incompleteDetails()))
}

type streamState struct {
	plan       *Plan
	w          io.Writer
	sequence   int
	output     []object
	message    *messageState
	reasoning  *reasoningState
	calls      map[int]*callState
	callOrder  []int
	usage      json.RawMessage
	finishKind string
	status     string
	summary    strings.Builder
}

func (s *streamState) emit(kind string, payload object) error {
	if s.w == nil {
		return nil
	}
	payload["type"] = kind
	payload["sequence_number"] = s.sequence
	s.sequence++
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, data)
	return err
}

func (s *streamState) text(delta string) error {
	if delta == "" {
		return nil
	}
	if s.plan.compact {
		s.summary.WriteString(delta)
		return nil
	}
	if s.message != nil && s.message.closed {
		s.message = nil
	}
	if s.message == nil {
		if s.reasoning != nil && !s.reasoning.closed {
			s.closeReasoning()
		}
		s.message = &messageState{id: randomID("msg")}
		s.message.index = len(s.output)
		s.output = append(s.output, s.message.item(false))
		if err := s.emit("response.output_item.added", object{"output_index": s.message.index, "item": s.message.item(false)}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.added", object{
			"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
			"part": object{"type": "output_text", "text": "", "annotations": []any{}},
		}); err != nil {
			return err
		}
	}
	s.message.text.WriteString(delta)
	return s.emit("response.output_text.delta", object{
		"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
		"delta": delta, "logprobs": []any{},
	})
}

func (s *streamState) think(delta string) error {
	if delta == "" {
		return nil
	}
	if s.plan.compact {
		return nil
	}
	if s.reasoning != nil && s.reasoning.closed {
		s.reasoning = nil
	}
	if s.reasoning == nil {
		s.reasoning = &reasoningState{id: randomID("rs")}
		s.reasoning.index = len(s.output)
		s.output = append(s.output, s.reasoning.item(false))
		if err := s.emit("response.output_item.added", object{"output_index": s.reasoning.index, "item": s.reasoning.item(false)}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.added", object{
			"item_id": s.reasoning.id, "output_index": s.reasoning.index, "content_index": 0,
			"part": object{"type": "reasoning_text", "text": ""},
		}); err != nil {
			return err
		}
	}
	s.reasoning.text.WriteString(delta)
	return s.emit("response.reasoning_text.delta", object{
		"item_id": s.reasoning.id, "output_index": s.reasoning.index, "content_index": 0,
		"delta": delta,
	})
}

func (s *streamState) call(index int, id, wire, arguments string, kind string) error {
	if s.plan.compact {
		return nil
	}
	if wire != "" && !s.plan.allowed[wire] {
		return fmt.Errorf("upstream called undeclared tool %q", wire)
	}
	if s.calls == nil {
		s.calls = make(map[int]*callState)
	}
	call := s.calls[index]
	if call == nil {
		call = &callState{index: index, outputIndex: len(s.output), callID: id}
		if kind == "custom" {
			call.custom = true
		}
		if wire != "" {
			call.name, call.namespace, call.custom = s.plan.toolInfo(wire)
			if call.name == "" {
				call.name = wire
			}
		}
		if call.custom {
			call.itemID = randomID("ctc")
		} else {
			call.itemID = randomID("fc")
		}
		s.calls[index] = call
		s.callOrder = append(s.callOrder, index)
		s.output = append(s.output, call.item(false))
		if err := s.emit("response.output_item.added", object{"output_index": call.outputIndex, "item": call.item(false)}); err != nil {
			return err
		}
	} else {
		if id != "" && call.callID == "" {
			call.callID = id
		}
		if wire != "" && call.name == "" {
			call.name, call.namespace, call.custom = s.plan.toolInfo(wire)
			if call.name == "" {
				call.name = wire
			}
		}
	}
	if arguments == "" || call.custom {
		call.arguments.WriteString(arguments)
		return nil
	}
	call.arguments.WriteString(arguments)
	return s.emit("response.function_call_arguments.delta", object{
		"item_id": call.itemID, "output_index": call.outputIndex, "delta": arguments,
	})
}

func (s *streamState) finish() {
	if s.plan.compact {
		text := strings.TrimSpace(s.summary.String())
		if text == "" {
			text = "No summary available."
		}
		item := object{
			"id":                randomID("cmp"),
			"type":              "compaction",
			"encrypted_content": encodeCompactionSummary(text),
		}
		index := len(s.output)
		s.output = append(s.output, item)
		_ = s.emit("response.output_item.added", object{"output_index": index, "item": item})
		_ = s.emit("response.output_item.done", object{"output_index": index, "item": item})
		s.status = "completed"
		if s.finishKind == "length" || s.finishKind == "content_filter" {
			s.status = "incomplete"
		}
		return
	}
	type closing struct {
		index int
		close func()
	}
	items := make([]closing, 0, len(s.callOrder)+2)
	for _, index := range s.callOrder {
		call := s.calls[index]
		items = append(items, closing{call.outputIndex, func() { s.closeCall(call) }})
	}
	if s.reasoning != nil {
		items = append(items, closing{s.reasoning.index, s.closeReasoning})
	}
	if s.message != nil {
		items = append(items, closing{s.message.index, s.closeMessage})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
	for _, item := range items {
		item.close()
	}
	if len(s.output) == 0 {
		s.message = &messageState{id: randomID("msg")}
		s.message.index = len(s.output)
		s.output = append(s.output, s.message.item(false))
		_ = s.emit("response.output_item.added", object{"output_index": s.message.index, "item": s.message.item(false)})
		_ = s.emit("response.content_part.added", object{
			"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
			"part": object{"type": "output_text", "text": "", "annotations": []any{}},
		})
		s.closeMessage()
	}
	s.status = "completed"
	if s.finishKind == "length" || s.finishKind == "content_filter" {
		s.status = "incomplete"
	}
}

func (s *streamState) closeMessage() {
	if s.message == nil || s.message.closed {
		return
	}
	s.message.closed = true
	text := s.message.text.String()
	_ = s.emit("response.output_text.done", object{
		"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
		"text": text, "logprobs": []any{},
	})
	_ = s.emit("response.content_part.done", object{
		"item_id": s.message.id, "output_index": s.message.index, "content_index": 0,
		"part": object{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}},
	})
	s.output[s.message.index] = s.message.item(true)
	_ = s.emit("response.output_item.done", object{"output_index": s.message.index, "item": s.message.item(true)})
}

func (s *streamState) closeReasoning() {
	if s.reasoning == nil || s.reasoning.closed {
		return
	}
	s.reasoning.closed = true
	text := s.reasoning.text.String()
	_ = s.emit("response.reasoning_text.done", object{
		"item_id": s.reasoning.id, "output_index": s.reasoning.index, "content_index": 0, "text": text,
	})
	_ = s.emit("response.content_part.done", object{
		"item_id": s.reasoning.id, "output_index": s.reasoning.index, "content_index": 0,
		"part": object{"type": "reasoning_text", "text": text},
	})
	s.output[s.reasoning.index] = s.reasoning.item(true)
	_ = s.emit("response.output_item.done", object{"output_index": s.reasoning.index, "item": s.reasoning.item(true)})
}

func (s *streamState) closeCall(call *callState) {
	if call == nil || call.closed {
		return
	}
	call.closed = true
	if call.callID == "" {
		call.callID = randomID("call")
	}
	if call.custom {
		input := customInput(call.arguments.String())
		if input != "" {
			_ = s.emit("response.custom_tool_call_input.delta", object{
				"item_id": call.itemID, "output_index": call.outputIndex, "delta": input,
			})
		}
		_ = s.emit("response.custom_tool_call_input.done", object{
			"item_id": call.itemID, "output_index": call.outputIndex, "input": input,
		})
	} else {
		_ = s.emit("response.function_call_arguments.done", object{
			"item_id": call.itemID, "output_index": call.outputIndex, "arguments": call.argumentsString(),
		})
	}
	s.output[call.outputIndex] = call.item(true)
	_ = s.emit("response.output_item.done", object{"output_index": call.outputIndex, "item": call.item(true)})
}

func (s *streamState) incompleteDetails() object {
	switch s.finishKind {
	case "length":
		return object{"reason": "max_output_tokens"}
	case "content_filter":
		return object{"reason": "content_filter"}
	default:
		return nil
	}
}

func (s *streamState) usageObject() object {
	if !present(s.usage) {
		return nil
	}
	var usage struct {
		PromptTokens        *int64 `json:"prompt_tokens"`
		CompletionTokens    *int64 `json:"completion_tokens"`
		TotalTokens         *int64 `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(s.usage, &usage) != nil {
		return nil
	}
	input, output, total := int64(0), int64(0), int64(0)
	if usage.PromptTokens != nil {
		input = *usage.PromptTokens
	}
	if usage.CompletionTokens != nil {
		output = *usage.CompletionTokens
	}
	if usage.TotalTokens != nil {
		total = *usage.TotalTokens
	} else {
		total = input + output
	}
	return object{
		"input_tokens":          input,
		"input_tokens_details":  object{"cached_tokens": usage.PromptTokensDetails.CachedTokens},
		"output_tokens":         output,
		"output_tokens_details": object{"reasoning_tokens": usage.CompletionTokensDetails.ReasoningTokens},
		"total_tokens":          total,
	}
}

type messageState struct {
	id     string
	index  int
	closed bool
	text   strings.Builder
}

func (m *messageState) item(done bool) object {
	status, content := "in_progress", []any{}
	if done {
		status = "completed"
		content = []any{object{"type": "output_text", "text": m.text.String(), "annotations": []any{}, "logprobs": []any{}}}
	}
	return object{"id": m.id, "type": "message", "status": status, "content": content, "role": "assistant"}
}

type reasoningState struct {
	id     string
	index  int
	closed bool
	text   strings.Builder
}

func (r *reasoningState) item(done bool) object {
	status, content := "in_progress", []any{}
	if done {
		status = "completed"
		content = []any{object{"type": "reasoning_text", "text": r.text.String()}}
	}
	return object{"id": r.id, "type": "reasoning", "status": status, "content": content, "summary": []any{}}
}

type callState struct {
	index       int
	outputIndex int
	itemID      string
	callID      string
	name        string
	namespace   string
	custom      bool
	closed      bool
	arguments   strings.Builder
}

func (c *callState) argumentsString() string {
	if c.arguments.Len() == 0 {
		return "{}"
	}
	return c.arguments.String()
}

func (c *callState) item(done bool) object {
	status := "in_progress"
	if done {
		status = "completed"
	}
	if c.custom {
		item := object{"id": c.itemID, "type": "custom_tool_call", "status": status, "call_id": c.callID, "name": c.name, "input": ""}
		if done {
			item["input"] = customInput(c.arguments.String())
		}
		return item
	}
	item := object{"id": c.itemID, "type": "function_call", "status": status, "call_id": c.callID, "name": c.name, "arguments": ""}
	if done {
		item["arguments"] = c.argumentsString()
	}
	if c.namespace != "" {
		item["namespace"] = c.namespace
	}
	return item
}

func customInput(arguments string) string {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if input, ok := value["input"]; ok {
			var text string
			if json.Unmarshal(input, &text) == nil {
				return text
			}
		}
	}
	return strings.TrimSpace(arguments)
}

func (p *Plan) toolInfo(wire string) (string, string, bool) {
	if t := p.tools.byWire[wire]; t != nil {
		return t.Name, t.Namespace, t.Custom
	}
	return wire, "", false
}

func (p *Plan) responseObject(status string, output []object, usage, incomplete object) object {
	if output == nil {
		output = []object{}
	}
	return object{
		"id":                   p.id,
		"object":               "response",
		"created_at":           p.created,
		"status":               status,
		"error":                nil,
		"incomplete_details":   incomplete,
		"instructions":         rawValue(p.original["instructions"]),
		"max_output_tokens":    rawValue(p.original["max_output_tokens"]),
		"model":                p.Model,
		"output":               output,
		"parallel_tool_calls":  defaultRaw(p.original["parallel_tool_calls"], true),
		"previous_response_id": nil,
		"reasoning":            rawValue(p.original["reasoning"]),
		"store":                false,
		"temperature":          rawValue(p.original["temperature"]),
		"text":                 defaultRaw(p.original["text"], object{"format": object{"type": "text"}}),
		"tool_choice":          defaultRaw(p.original["tool_choice"], "auto"),
		"tools":                defaultRaw(p.original["tools"], []any{}),
		"top_p":                rawValue(p.original["top_p"]),
		"truncation":           defaultRaw(p.original["truncation"], "disabled"),
		"usage":                usage,
		"user":                 rawValue(p.original["user"]),
		"metadata":             defaultRaw(p.original["metadata"], object{}),
	}
}

func rawValue(raw json.RawMessage) any {
	if !present(raw) {
		return nil
	}
	return raw
}

func defaultRaw(raw json.RawMessage, fallback any) any {
	if !present(raw) {
		return fallback
	}
	return raw
}

// consumeChat accepts the same SSE shape as Chat Completions, including tool
// call deltas, reasoning content and an optional final usage-only chunk.
func consumeChat(r io.Reader, state *streamState, allowMissingDone bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxSSELine)
	var data []string
	event, eventBytes, totalBytes := "", 0, 0
	for scanner.Scan() {
		line := scanner.Text()
		eventBytes += len(line) + 1
		totalBytes += len(line) + 1
		if eventBytes > maxSSELine {
			return errors.New("Chat stream event exceeds buffer limit")
		}
		if totalBytes > maxResponseStream {
			return errors.New("Chat stream response exceeds buffer limit")
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
			return errors.New("Chat stream returned an error event")
		}
		payload := strings.Join(data, "\n")
		data, event, eventBytes = nil, "", 0
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return nil
		}
		if err := state.chunk([]byte(payload)); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// Some Chat-compatible upstreams close the stream after the final
	// finish_reason without sending the optional [DONE] sentinel. Streaming
	// callers can safely treat that as a completed response; non-streaming
	// callers keep requiring [DONE] because they cannot expose partial output.
	if allowMissingDone && state.finishKind != "" {
		return nil
	}
	return errors.New("Chat stream ended before [DONE]")
}

func (s *streamState) chunk(data []byte) error {
	var chunk struct {
		Type    json.RawMessage `json:"type"`
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index        *int            `json:"index"`
			FinishReason json.RawMessage `json:"finish_reason"`
			Delta        struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				ReasoningText    json.RawMessage `json:"reasoning_text"`
				ToolCalls        []struct {
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
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("invalid Chat stream JSON: %w", err)
	}
	if present(chunk.Error) || stringField(chunk.Type) == "error" {
		return errors.New("Chat stream returned an error payload")
	}
	if present(chunk.Usage) {
		s.usage = append(json.RawMessage(nil), chunk.Usage...)
	}
	for _, choice := range chunk.Choices {
		if choice.Index != nil && *choice.Index != 0 {
			continue
		}
		reasoning := deltaText(choice.Delta.ReasoningContent)
		if reasoning == "" {
			reasoning = deltaText(choice.Delta.Reasoning)
		}
		if reasoning == "" {
			reasoning = deltaText(choice.Delta.ReasoningText)
		}
		if reasoning != "" {
			if err := s.think(reasoning); err != nil {
				return err
			}
		}
		if text := deltaText(choice.Delta.Content); text != "" {
			if err := s.text(text); err != nil {
				return err
			}
		}
		for position, call := range choice.Delta.ToolCalls {
			index := position
			if call.Index != nil {
				index = *call.Index
			}
			kind := call.Type
			wire := call.Function.Name
			arguments := rawText(call.Function.Arguments)
			if call.Custom.Name != "" {
				kind, wire = "custom", call.Custom.Name
				arguments = rawText(call.Custom.Input)
			}
			if err := s.call(index, call.ID, wire, arguments, kind); err != nil {
				return err
			}
		}
		if present(choice.FinishReason) {
			s.finishKind = stringField(choice.FinishReason)
		}
	}
	return nil
}

func deltaText(raw json.RawMessage) string {
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
		if text := stringField(part["text"]); text != "" {
			result.WriteString(text)
		}
	}
	return result.String()
}

func rawText(raw json.RawMessage) string {
	if !present(raw) {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(bytes.TrimSpace(raw))
}

func randomID(prefix string) string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err == nil {
		return prefix + "_" + hex.EncodeToString(random[:])
	}
	return prefix + "_" + "000000000000000000000000"
}
