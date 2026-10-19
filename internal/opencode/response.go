package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"aizasy.pages.dev/ai-route/internal/config"
)

type attemptResult struct {
	response *http.Response
	body     []byte
	stream   io.Reader
	err      error
	cleanup  func()
}

func (p *Proxy) attempt(ctx context.Context, source net.IP, request chatRequest, identity requestIdentity) attemptResult {
	transport := p.transport(source)
	result := attemptResult{cleanup: transport.CloseIdleConnections}
	target := *p.cfg.BaseURL
	target.Path += "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(request.body))
	if err != nil {
		result.err = err
		return result
	}
	// Do not copy caller credentials, cookies, Google keys or query parameters.
	req.Header.Set("Content-Type", "application/json")

	req.Header.Set("User-Agent", config.OpenCodeUserAgent)

	if request.streaming {
		req.Header.Set("Accept", "text/event-stream")
	}

	// Match the anonymous credential used by the OpenCode provider loader.
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer public")
	}

	// Every attempt carries the same OpenCode 2.x session fingerprint.
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session-id", identity.session)
	req.Header.Set("x-opencode-session", identity.session)
	req.Header.Set("x-session-affinity", identity.session)
	req.Header.Set("X-Session-Id", identity.session)
	req.Header.Set("x-opencode-project", clientProjectID(identity.session))
	resp, err := transport.RoundTrip(req)
	if err != nil {
		result.err = err
		return result
	}
	result.response = resp
	result.cleanup = func() { resp.Body.Close(); transport.CloseIdleConnections() }
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.body, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		result.err = fmt.Errorf("upstream HTTP %d", resp.StatusCode)
		return result
	}
	if request.complete != nil {
		result.body, result.err = request.complete(resp.Body)
		if result.err == nil {
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("ETag")
			resp.Header.Del("Content-MD5")
			resp.Header.Del("Digest")
			resp.Header.Del("Content-Digest")
		}
		return result
	}
	if request.collect {
		result.body, result.err = collectChat(resp.Body)
		if result.err == nil {
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("ETag")
			resp.Header.Del("Content-MD5")
			resp.Header.Del("Digest")
			resp.Header.Del("Content-Digest")
		}
		return result
	}
	if request.streaming {
		prefix, reader, err := readChatEvent(resp.Body)
		result.err = err
		if err == nil {
			result.stream = io.MultiReader(bytes.NewReader(prefix), reader)
		}
		return result
	}
	result.body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		result.err = err
		return result
	}
	if len(result.body) > maxResponseBytes {
		result.err = errors.New("OpenCode response exceeds buffer limit")
		return result
	}
	valid, err := validChat(result.body, false)
	if err != nil {
		result.err = err
	} else if !valid {
		result.err = errors.New("invalid OpenCode chat response")
	}
	return result
}

// A 200 containing an error, truncated JSON, or an empty choices list is not a winner.
func validChat(data []byte, streaming bool) (bool, error) {
	var envelope struct {
		Error   json.RawMessage              `json:"error"`
		Choices []map[string]json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, err
	}
	if len(envelope.Error) != 0 && string(envelope.Error) != "null" {
		return false, errors.New("OpenCode returned an error payload")
	}
	field := "message"
	if streaming {
		field = "delta"
	}
	for _, choice := range envelope.Choices {
		var object map[string]json.RawMessage
		if json.Unmarshal(choice[field], &object) == nil && len(object) != 0 {
			return true, nil
		}
	}
	return false, nil
}

// Preserve every byte read (including comments and CRLF) and the reader's buffered tail.
func readChatEvent(body io.Reader) ([]byte, io.Reader, error) {
	reader := bufio.NewReader(body)
	var prefix bytes.Buffer
	var line []byte
	var data []string
	event := ""
	for {
		chunk, err := reader.ReadSlice('\n')
		prefix.Write(chunk)
		if prefix.Len() > maxEventBytes {
			return nil, nil, errors.New("OpenCode SSE prelude exceeds limit")
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("OpenCode stream ended before a valid event: %w", err)
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		line = line[:0]
		if text != "" {
			field, value, _ := strings.Cut(text, ":")
			value = strings.TrimPrefix(value, " ")
			if field == "data" {
				data = append(data, value)
			}
			if field == "event" {
				event = value
			}
			continue
		}
		if event == "error" {
			return nil, nil, errors.New("OpenCode SSE error event")
		}
		if len(data) != 0 {
			payload := strings.Join(data, "\n")
			if payload == "[DONE]" {
				return nil, nil, errors.New("OpenCode stream finished before a valid chat event")
			}
			valid, err := validChat([]byte(payload), true)
			if err != nil {
				return nil, nil, err
			}
			if valid {
				return prefix.Bytes(), reader, nil
			}
		}
		data, event = nil, ""
	}
}
