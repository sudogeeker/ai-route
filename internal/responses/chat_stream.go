package responses

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	maxSSELine        = 8 << 20
	maxResponseStream = 64 << 20
)

var errMissingDone = errors.New("Chat stream ended before [DONE]")

type chatStream struct {
	scanner *bufio.Scanner
	total   int
}

func newChatStream(reader io.Reader) *chatStream {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxSSELine)
	return &chatStream{scanner: scanner}
}

func (stream *chatStream) next() (*chatChunk, error) {
	var data []string
	var event string
	var eventBytes int
	for stream.scanner.Scan() {
		line := stream.scanner.Text()
		size := len(line) + 1
		eventBytes += size
		stream.total += size
		if eventBytes > maxSSELine {
			return nil, errors.New("Chat stream event exceeds buffer limit")
		}
		if stream.total > maxResponseStream {
			return nil, errors.New("Chat stream response exceeds buffer limit")
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
			return nil, errors.New("Chat stream returned an error event")
		}
		payload := strings.Join(data, "\n")
		switch payload {
		case "":
			data, event, eventBytes = nil, "", 0
			continue
		case "[DONE]":
			return nil, nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("invalid Chat stream JSON: %w", err)
		}
		if present(chunk.Error) || stringField(chunk.Type) == "error" {
			return nil, errors.New("Chat stream returned an error payload")
		}
		return &chunk, nil
	}
	if err := stream.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errMissingDone
}

type chatChunk struct {
	Type    json.RawMessage `json:"type"`
	Error   json.RawMessage `json:"error"`
	Usage   json.RawMessage `json:"usage"`
	Choices []struct {
		Index        *int            `json:"index"`
		FinishReason json.RawMessage `json:"finish_reason"`
		Delta        struct {
			Content          json.RawMessage `json:"content"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			Reasoning        json.RawMessage `json:"reasoning"`
			ReasoningText    json.RawMessage `json:"reasoning_text"`
			ToolCalls        []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

type toolCallDelta struct {
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
}

func (call *toolCallDelta) fields(position int) (index int, name, arguments, kind string) {
	if call.Index != nil {
		position = *call.Index
	}
	if call.Custom.Name != "" {
		return position, call.Custom.Name, rawText(call.Custom.Input), "custom"
	}
	return position, call.Function.Name, rawText(call.Function.Arguments), call.Type
}
