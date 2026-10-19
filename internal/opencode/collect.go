package opencode

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

type collectedChoice struct {
	Index        int                        `json:"index"`
	Message      map[string]json.RawMessage `json:"message"`
	FinishReason json.RawMessage            `json:"finish_reason"`
	Logprobs     map[string]json.RawMessage `json:"logprobs,omitempty"`
	messageParts collectedFields
	logprobParts collectedFields
}

// collectChat only produces a winner after a complete SSE response, including
// finish reasons and [DONE]. A truncated stream must never become valid JSON.
func collectChat(body io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: body, N: maxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	envelope := make(map[string]json.RawMessage)
	choices := make(map[int]*collectedChoice)
	var data []string
	event, eventBytes := "", 0
	for scanner.Scan() {
		line := scanner.Text()
		eventBytes += len(line) + 1
		if eventBytes > maxEventBytes || limited.N == 0 {
			return nil, errors.New("OpenCode SSE response exceeds buffer limit")
		}
		if line != "" {
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			if field == "data" {
				data = append(data, value)
			} else if field == "event" {
				event = value
			}
			continue
		}
		if event == "error" {
			return nil, errors.New("OpenCode SSE error event")
		}
		payload := strings.Join(data, "\n")
		data, event, eventBytes = nil, "", 0
		if payload == "[DONE]" {
			if len(choices) == 0 {
				return nil, errors.New("OpenCode stream finished without choices")
			}
			ordered := make([]*collectedChoice, 0, len(choices))
			for _, choice := range choices {
				var reason string
				if json.Unmarshal(choice.FinishReason, &reason) != nil || reason == "" {
					return nil, errors.New("OpenCode stream finished without a finish reason")
				}
				if reason == "tool_calls" || reason == "function_call" {
					return nil, errors.New("OpenCode stream finished with an unexpected tool call")
				}
				choice.Message = choice.messageParts.finish()
				if !nonNull(choice.Message["role"]) {
					choice.Message["role"] = json.RawMessage(`"assistant"`)
				}
				if len(choice.Message["content"]) == 0 {
					choice.Message["content"] = json.RawMessage(`null`)
				}
				choice.Logprobs = choice.logprobParts.finish()
				ordered = append(ordered, choice)
			}
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
			envelope["choices"], _ = json.Marshal(ordered)
			envelope["object"] = json.RawMessage(`"chat.completion"`)
			return json.Marshal(envelope)
		}
		if payload == "" {
			continue
		}
		var chunk map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || chunk == nil {
			return nil, errors.New("Invalid OpenCode SSE JSON")
		}
		if nonNull(chunk["error"]) || string(chunk["type"]) == `"error"` {
			return nil, errors.New("OpenCode returned an error payload")
		}
		var deltas []struct {
			Index        *int                       `json:"index"`
			Delta        map[string]json.RawMessage `json:"delta"`
			FinishReason json.RawMessage            `json:"finish_reason"`
			Logprobs     map[string]json.RawMessage `json:"logprobs"`
		}
		if raw := chunk["choices"]; len(raw) != 0 {
			if err := json.Unmarshal(raw, &deltas); err != nil {
				return nil, fmt.Errorf("Invalid OpenCode SSE choices: %w", err)
			}
		} else if !nonNull(chunk["usage"]) {
			return nil, errors.New("OpenCode SSE event contains neither choices nor usage")
		}
		for _, delta := range deltas {
			if delta.Index == nil || *delta.Index < 0 {
				return nil, errors.New("OpenCode SSE choice has no valid index")
			}
			choice := choices[*delta.Index]
			if choice == nil {
				choice = &collectedChoice{Index: *delta.Index}
				choices[*delta.Index] = choice
			}
			if err := choice.messageParts.append(delta.Delta); err != nil {
				return nil, err
			}
			if len(delta.Logprobs) != 0 {
				if err := choice.logprobParts.append(delta.Logprobs); err != nil {
					return nil, err
				}
			}
			if nonNull(delta.FinishReason) {
				choice.FinishReason = delta.FinishReason
			}
		}
		for key, value := range chunk {
			if key != "choices" && key != "object" && key != "error" && nonNull(value) {
				envelope[key] = value
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("OpenCode stream ended before [DONE]")
}

func nonNull(value json.RawMessage) bool {
	return len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// Accumulate deltas without repeatedly copying the entire answer per token.
type collectedFields struct {
	values map[string]json.RawMessage
	text   map[string]*strings.Builder
	arrays map[string][]json.RawMessage
}

func (f *collectedFields) append(delta map[string]json.RawMessage) error {
	if f.values == nil {
		f.values = make(map[string]json.RawMessage)
		f.text = make(map[string]*strings.Builder)
		f.arrays = make(map[string][]json.RawMessage)
	}
	for key, value := range delta {
		if !nonNull(value) {
			continue
		}
		if key == "tool_calls" {
			var calls []json.RawMessage
			if json.Unmarshal(value, &calls) == nil && len(calls) == 0 {
				continue
			}
		}
		if key == "tool_calls" || key == "function_call" {
			return errors.New("OpenCode returned a tool call despite tool_choice none")
		}
		var text string
		if key != "role" && json.Unmarshal(value, &text) == nil {
			builder := f.text[key]
			if builder == nil {
				builder = &strings.Builder{}
				f.text[key] = builder
			}
			builder.WriteString(text)
			continue
		}
		var array []json.RawMessage
		if json.Unmarshal(value, &array) == nil {
			f.arrays[key] = append(f.arrays[key], array...)
			continue
		}
		f.values[key] = value
	}
	return nil
}

func (f *collectedFields) finish() map[string]json.RawMessage {
	if f.values == nil {
		return make(map[string]json.RawMessage)
	}
	for key, builder := range f.text {
		f.values[key], _ = json.Marshal(builder.String())
	}
	for key, array := range f.arrays {
		f.values[key], _ = json.Marshal(array)
	}
	return f.values
}
