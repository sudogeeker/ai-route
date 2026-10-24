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
	"strconv"
	"strings"
	"sync"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

type attemptResult struct {
	response   *http.Response
	body       []byte
	stream     io.Reader
	err        error
	retryAfter time.Duration
	egress     string
	terminal   bool
	cleanup    func()
}

func (p *Proxy) attempt(ctx context.Context, source net.IP, request chatRequest, identity requestIdentity) (result attemptResult) {
	ctx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(context.Canceled) }
	grace := p.firstTokenTimeout()
	deadline := newFirstTokenDeadline(cancelCause)
	var lease warpEgress
	defer func() {
		if deadline.stop() {
			result.err = fmt.Errorf("%w after %s", errFirstTokenTimeout, grace)
			p.failWARP(lease, "first-token timeout")
		}
	}()
	var transport *http.Transport
	if p.usesWARP() {
		var err error
		lease, err = p.warpPool.Acquire(ctx)
		if err != nil {
			cancel()
			result.err = fmt.Errorf("acquire WARP egress: %w", err)
			return result
		}
		if p.warpTransport != nil {
			transport = p.warpTransport(lease)
		} else {
			transport = p.newWARPTransport(lease)
		}
		result.egress = lease.EgressKey()
	} else {
		transport = p.transport(source)
	}
	release := func() {
		if lease != nil {
			lease.Release()
		}
	}
	result.cleanup = func() { cancel(); transport.CloseIdleConnections(); release() }
	target := *p.cfg.BaseURL
	target.Path += "/chat/completions"
	uploaded := make(chan struct{})
	body := &uploadTrackingBody{reader: bytes.NewReader(request.body), remaining: int64(len(request.body)), uploaded: uploaded}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		result.err = err
		return result
	}
	req.ContentLength = int64(len(request.body))
	// Do not copy caller credentials, cookies, Google keys or query parameters.
	req.Header.Set("Content-Type", "application/json")

	req.Header.Set("User-Agent", config.OpenCodeUserAgent)
	req.Header.Set("Accept", "*/*")

	// Match the anonymous credential used by the OpenCode provider loader.
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer public")
	}

	// Every attempt carries the same OpenCode 1.19.4 request fingerprint.
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session-id", identity.session)
	req.Header.Set("x-opencode-session", identity.session)
	req.Header.Set("x-opencode-request", identity.request)
	req.Header.Set("x-opencode-project", identity.project)
	go func() {
		select {
		case <-uploaded:
			deadline.start(grace)
		case <-ctx.Done():
		}
	}()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			p.failWARP(lease, "transport error")
		}
		result.err = err
		return result
	}
	result.response = resp
	resp.Body = newIdleResponseBody(resp.Body, p.streamIdleTimeout(), cancel)
	result.cleanup = func() { cancel(); resp.Body.Close(); transport.CloseIdleConnections(); release() }
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.body, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		result.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		result.terminal = terminalLimit(resp.StatusCode, result.body)
		if p.warpPool != nil && lease != nil && !result.terminal {
			p.warpPool.Report(lease, resp.StatusCode, result.body)
		}
		result.err = fmt.Errorf("upstream HTTP %d", resp.StatusCode)
		return result
	}
	if p.warpPool != nil && lease != nil {
		p.warpPool.Report(lease, resp.StatusCode, nil)
	}
	if request.streaming {
		prefix, reader, err := readChatEvent(resp.Body)
		if err != nil && ctx.Err() == nil {
			p.failWARP(lease, "stream failure")
		}
		result.err = err
		if err == nil {
			result.stream = io.MultiReader(bytes.NewReader(prefix), reader)
		}
		return result
	}
	result.body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() == nil {
			p.failWARP(lease, "response read failure")
		}
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

// terminalLimit identifies client-fingerprint rejections that no amount of
// source-IP or WARP rotation can resolve. FreeUsageLimitError is deliberately
// not terminal: the same request succeeds through WARP IPv4 while WARP IPv6
// returns it, so it behaves as an egress-level limit in practice.
func terminalLimit(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	message := strings.ToLower(string(body))
	for _, marker := range []string{
		"freetiererror",
		"can only be used from within opencode",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

type uploadTrackingBody struct {
	reader    io.Reader
	remaining int64
	uploaded  chan struct{}
	once      sync.Once
}

func (b *uploadTrackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.remaining -= int64(n)
	if b.remaining <= 0 || err == io.EOF {
		b.once.Do(func() { close(b.uploaded) })
	}
	return n, err
}

func (b *uploadTrackingBody) Close() error { return nil }

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
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
		if json.Unmarshal(choice[field], &object) == nil && hasChatOutput(object) {
			return true, nil
		}
	}
	return false, nil
}

func hasChatOutput(object map[string]json.RawMessage) bool {
	for _, key := range []string{"content", "reasoning_content", "reasoning", "reasoning_text", "refusal"} {
		if hasOutputText(object[key]) {
			return true
		}
	}
	var calls []map[string]json.RawMessage
	_ = json.Unmarshal(object["tool_calls"], &calls)
	calls = append(calls, map[string]json.RawMessage{"function": object["function_call"]})
	for _, call := range calls {
		for _, kind := range []string{"function", "custom"} {
			var tool map[string]json.RawMessage
			if json.Unmarshal(call[kind], &tool) == nil {
				for _, field := range []string{"name", "arguments", "input"} {
					if hasOutputText(tool[field]) {
						return true
					}
				}
			}
		}
	}
	var audio map[string]json.RawMessage
	if json.Unmarshal(object["audio"], &audio) == nil {
		return hasOutputText(audio["data"]) || hasOutputText(audio["transcript"])
	}
	return false
}

// Content parts must contain actual text; IDs, indexes and type fields alone
// are only metadata, even when enclosed in nonempty arrays or objects.
func hasOutputText(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text != ""
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		for _, part := range parts {
			if hasOutputText(part["text"]) || hasOutputText(part["refusal"]) {
				return true
			}
		}
	}
	return false
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
