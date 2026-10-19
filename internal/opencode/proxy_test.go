package opencode

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	routing "aizasy.pages.dev/ai-route/internal/server"
)

const openCodeOK = `{"choices":[{"message":{"role":"assistant","content":"winner"},"finish_reason":"stop"}]}`

var projectPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

const compatibilityToolInstructionText = "Only call the function tools supplied by the caller"

func TestOpenCodeRequestIdentity(t *testing.T) {
	var lock sync.Mutex
	var requests []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requests = append(requests, r.Header.Clone())
		lock.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	var previousSession string
	for logical := 0; logical < 2; logical++ {
		recorder := httptest.NewRecorder()
		request := openCodeTestRequest(`{"model":"big-pickle","stream":true}`)
		for _, name := range []string{"x-opencode-session-id", "x-opencode-session", "x-opencode-request", "x-opencode-client", "x-opencode-project", "x-opencode-parent-session-id", "User-Agent"} {
			request.Header.Set(name, "untrusted-caller-value")
		}
		testOpenCodeHandler(p).ServeHTTP(recorder, request)
		lock.Lock()
		if len(requests) != maxAttempts {
			t.Fatalf("expected ten upstream requests, got %d", len(requests))
		}
		session := requests[0].Get("x-opencode-session")
		if !sessionPattern.MatchString(session) || session == previousSession {
			t.Fatalf("invalid or reused request identity: %q", session)
		}
		for _, header := range requests {
			if header.Get("User-Agent") != config.OpenCodeUserAgent || header.Get("Authorization") != "Bearer public" || header.Get("Accept") != "text/event-stream" {
				t.Errorf("wrong protocol headers: %v", header)
			}
			if header.Get("x-opencode-session-id") != session || header.Get("x-opencode-session") != session || header.Get("x-session-affinity") != session || header.Get("X-Session-Id") != session || header.Get("x-opencode-client") != "cli" {
				t.Errorf("race attempts did not share one identity: %v", header)
			}
			if !projectPattern.MatchString(header.Get("x-opencode-project")) || header.Get("x-opencode-request") != "" {
				t.Errorf("wrong 2.x fingerprint headers: %v", header)
			}
			if header.Get("x-opencode-parent-session-id") != "" {
				t.Error("caller parent session leaked upstream")
			}
		}
		previousSession = session
		requests = nil
		lock.Unlock()
	}
}

func testOpenCodeProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	p := New(config.OpenCode{
		BaseURL: u, Models: map[string]bool{"big-pickle": true},
		WaitTimeout: 2 * time.Second, MaxBodyBytes: 64 << 20,
	}, nil, nil)
	p.sources = func(count int) ([]net.IP, error) {
		var sources []net.IP
		for i := 0; i < count; i++ {
			sources = append(sources, net.ParseIP(fmt.Sprintf("2001:db8::%x", i+1)))
		}
		return sources, nil
	}
	p.transport = func(source net.IP) *http.Transport {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DisableKeepAlives = true
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local test certificate only.
		transport.DialContext = (&net.Dialer{Timeout: time.Second}).DialContext
		return transport
	}
	return p
}

func testOpenCodeHandler(p *Proxy) http.Handler {
	return routing.NewRouter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) }), p, "", p.cfg.MaxBodyBytes)
}

func openCodeTestRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, routing.ChatPath, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer google-secret")
	r.Header.Set("X-Goog-Api-Key", "other-google-secret")
	r.Header.Set("Cookie", "secret=value")
	return r
}

func waitOpenCode(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for OpenCode task cleanup")
	}
}
func TestOpenCodeParallelWinnerCancelsAndPreservesBody(t *testing.T) {
	body := `{"model":"big-pickle","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}},{"type":"input_audio","input_audio":{"data":"YWJj","format":"wav"}}]}],"tools":[{"type":"function","function":{"name":"example"}}],"unknown_vendor_parameter":{"keep":true}}`
	var started, canceled atomic.Int32
	allStarted, allCanceled := make(chan struct{}), make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Errorf("request bytes changed: %s", got)
		}
		if r.URL.Path != "/chat/completions" || r.URL.RawQuery != "" {
			t.Errorf("wrong upstream URL: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer opencode-secret" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Error("caller credentials leaked or OpenCode key lost")
		}
		id := started.Add(1)
		if id == maxAttempts {
			close(allStarted)
		}
		select {
		case <-allStarted:
		case <-r.Context().Done():
			return
		}
		if id == 1 {
			w.Header().Set("X-Test-Winner", "yes")
			io.WriteString(w, openCodeOK)
			return
		}
		<-r.Context().Done()
		if canceled.Add(1) == maxAttempts-1 {
			close(allCanceled)
		}
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.APIKey = "opencode-secret"
	var lock sync.Mutex
	selected := map[string]bool{}
	factory := p.transport
	p.transport = func(source net.IP) *http.Transport {
		lock.Lock()
		if selected[source.String()] {
			t.Error("duplicate source address")
		}
		selected[source.String()] = true
		lock.Unlock()
		return factory(source)
	}
	recorder := httptest.NewRecorder()
	r := openCodeTestRequest(body)
	r.URL.RawQuery = "key=google-query-secret"
	testOpenCodeHandler(p).ServeHTTP(recorder, r)
	if recorder.Code != 200 || recorder.Body.String() != openCodeOK || recorder.Header().Get("X-Test-Winner") != "yes" {
		t.Fatalf("unexpected winner: %d %s", recorder.Code, recorder.Body)
	}
	waitOpenCode(t, allCanceled)
	if started.Load() != 10 || len(selected) != 10 {
		t.Fatalf("expected exactly 10 independent attempts, got %d/%d", started.Load(), len(selected))
	}
}

func TestOpenCodeFailuresAllAttempted(t *testing.T) {
	for _, status := range []int{301, 401, 403, 404, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			body := `{"error":{"type":"FreeTierError","message":"original refusal"}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer public" {
					t.Error("keyless request did not use the anonymous credential")
				}
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Upstream-Error", "preserved")
				w.WriteHeader(status)
				io.WriteString(w, body)
			}))
			defer server.Close()
			p := testOpenCodeProxy(t, server.URL)
			recorder := httptest.NewRecorder()
			testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
			if attempts.Load() != 10 || recorder.Code != status || recorder.Body.String() != body || recorder.Header().Get("X-Upstream-Error") != "preserved" {
				t.Fatalf("attempts=%d status=%d body=%s", attempts.Load(), recorder.Code, recorder.Body)
			}
		})
	}
}

func TestOpenCodeLastHTTPErrorAfterMixedFailures(t *testing.T) {
	var count atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := count.Add(1)
		if id == 10 {
			close(ready)
		}
		<-ready
		if id == 10 {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(429)
			io.WriteString(w, "last refusal")
			return
		}
		w.WriteHeader(403)
		io.WriteString(w, "earlier refusal")
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
	if recorder.Code != 429 || recorder.Body.String() != "last refusal" {
		t.Fatalf("wrong final error: %d %s", recorder.Code, recorder.Body)
	}
}

func TestOpenCodeInvalidSuccessDoesNotWin(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := attempts.Add(1)
		if id <= 5 {
			io.WriteString(w, `{"error":{"message":"not a successful completion"}}`)
			return
		}
		if id < 10 {
			io.WriteString(w, `{"choices":[{"message":{}}]}`)
			return
		}
		io.WriteString(w, openCodeOK)
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
	if recorder.Code != 200 || recorder.Body.String() != openCodeOK || attempts.Load() != 10 {
		t.Fatalf("wrong winner: %d %s attempts=%d", recorder.Code, recorder.Body, attempts.Load())
	}
}

func TestOpenCodeStreamingWinnerAndCleanupBeforeEnd(t *testing.T) {
	var attempts, canceled atomic.Int32
	ready, losersDone, releaseWinner := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWinner) }) }
	prefix := ": ping\r\n\r\ndata: {\"choices\": []}\r\n\r\ndata: {\"choices\":\r\ndata: [{\"delta\":{\"content\":\"first\"}}]}\r\n\r\n"
	tail := "data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\ndata: [DONE]\n\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		id := attempts.Add(1)
		if id == 10 {
			close(ready)
		}
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if id == 1 {
			io.WriteString(w, prefix)
			w.(http.Flusher).Flush()
			select {
			case <-releaseWinner:
				io.WriteString(w, tail)
			case <-r.Context().Done():
			}
			return
		}
		// HTTP 200 without a valid event must not beat the real stream.
		io.WriteString(w, ": still waiting\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		if canceled.Add(1) == 9 {
			close(losersDone)
		}
	}))
	defer server.Close()
	defer release()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.WaitTimeout = 500 * time.Millisecond
	proxyServer := httptest.NewServer(testOpenCodeHandler(p))
	defer proxyServer.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(proxyServer.URL+routing.ChatPath, "application/json", strings.NewReader(`{"model":"big-pickle","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	gotPrefix := make([]byte, len(prefix))
	if _, err := io.ReadFull(resp.Body, gotPrefix); err != nil || string(gotPrefix) != prefix {
		t.Fatalf("stream prefix changed: %q %v", gotPrefix, err)
	}
	waitOpenCode(t, losersDone)
	// The wait deadline must no longer cancel a selected, active stream.
	time.Sleep(600 * time.Millisecond)
	release()
	gotTail, err := io.ReadAll(resp.Body)
	if err != nil || string(gotTail) != tail {
		t.Fatalf("stream tail changed or timed out: %q %v", gotTail, err)
	}
}

func TestOpenCodeTimeoutAndCallerCancellation(t *testing.T) {
	for _, callerCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(callerCancel), func(t *testing.T) {
			var started, canceled atomic.Int32
			ready, finished := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if started.Add(1) == 10 {
					close(ready)
				}
				<-r.Context().Done()
				if canceled.Add(1) == 10 {
					close(finished)
				}
			}))
			defer server.Close()
			p := testOpenCodeProxy(t, server.URL)
			p.cfg.WaitTimeout = 200 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder, done := httptest.NewRecorder(), make(chan struct{})
			go func() {
				testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`).WithContext(ctx))
				close(done)
			}()
			waitOpenCode(t, ready)
			if callerCancel {
				cancel()
			}
			waitOpenCode(t, done)
			waitOpenCode(t, finished)
			if !callerCancel && recorder.Code != 504 {
				t.Fatalf("expected timeout, got %d", recorder.Code)
			}
			if callerCancel && recorder.Body.Len() != 0 {
				t.Error("wrote an error after caller canceled")
			}
		})
	}
}

func TestOpenCodeNetworkFailuresAndBodyLimit(t *testing.T) {
	p := testOpenCodeProxy(t, "http://localhost:1")
	var dials atomic.Int32
	p.transport = func(net.IP) *http.Transport {
		return &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("synthetic dial failure")
		}}
	}
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
	if recorder.Code != 502 || dials.Load() != 10 {
		t.Fatalf("network failures: %d/%d", recorder.Code, dials.Load())
	}
	p.cfg.MaxBodyBytes = 5
	recorder = httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
	if recorder.Code != 413 || dials.Load() != 10 {
		t.Fatalf("body limit failed: %d/%d", recorder.Code, dials.Load())
	}
}

func TestOpenCodeResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Stream     bool              `json:"stream"`
					Tools      []json.RawMessage `json:"tools"`
					ToolChoice string            `json:"tool_choice"`
				}
				if r.URL.Path != "/chat/completions" {
					t.Errorf("unexpected upstream path: %s", r.URL.Path)
					return
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					// A canceled losing attempt may not deliver its full body.
					return
				}
				if !request.Stream || len(request.Tools) != 12 || request.ToolChoice != "none" {
					t.Errorf("unexpected upstream Responses request: %+v", request)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, completeStream)
			}))
			defer server.Close()
			p := testOpenCodeProxy(t, server.URL)
			p.cfg.FreeCompat = true
			body := fmt.Sprintf(`{"model":"big-pickle","input":"hi","stream":%t}`, streaming)
			recorder := httptest.NewRecorder()
			p.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, routing.OpenCodeResponsesPath, strings.NewReader(body)), []byte(body))
			if recorder.Code != http.StatusOK {
				t.Fatalf("responses failed: %d %s", recorder.Code, recorder.Body)
			}
			if streaming {
				if recorder.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(recorder.Body.String(), "event: response.completed") || !strings.Contains(recorder.Body.String(), `"delta":"hello"`) {
					t.Fatalf("bad Responses stream: %v %s", recorder.Header(), recorder.Body)
				}
			} else {
				var response struct {
					Object string `json:"object"`
					Status string `json:"status"`
					Output []struct {
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"output"`
				}
				if recorder.Header().Get("Content-Type") != "application/json" || json.Unmarshal(recorder.Body.Bytes(), &response) != nil || response.Object != "response" || response.Status != "completed" {
					t.Fatalf("bad Responses JSON: %v %s", recorder.Header(), recorder.Body)
				}
				text := ""
				for _, item := range response.Output {
					for _, part := range item.Content {
						if part.Type == "output_text" {
							text += part.Text
						}
					}
				}
				if text != "hello world" {
					t.Fatalf("bad Responses text %q: %s", text, recorder.Body)
				}
			}
		})
	}
}

func TestOpenCodeResponsesRejectsInvalidModelRequest(t *testing.T) {
	p := testOpenCodeProxy(t, "http://localhost:1")
	for _, body := range []string{`not json`, `{"model":"big-pickle"}`, `{"model":"big-pickle","store":true}`, `{"model":"big-pickle","previous_response_id":"resp_1"}`} {
		recorder := httptest.NewRecorder()
		p.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, routing.OpenCodeResponsesPath, strings.NewReader(body)), []byte(body))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("invalid body %s returned %d", body, recorder.Code)
		}
	}
}

func TestOpenCodeResponsesRejectsTruncatedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.TrimSuffix(completeStream, "data: [DONE]\n\n"))
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.FreeCompat = true
	body := []byte(`{"model":"big-pickle","input":"hi","stream":false}`)
	recorder := httptest.NewRecorder()
	p.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, routing.OpenCodeResponsesPath, strings.NewReader(string(body))), body)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("truncated Responses stream won: %d %s", recorder.Code, recorder.Body)
	}
}

func TestOpenCodeResponsesToolCall(t *testing.T) {
	digest := sha256.Sum256([]byte("\x00read_file"))
	wire := "route_read_file_" + hex.EncodeToString(digest[:8])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []decodedTool `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			return
		}
		tools := make(map[string]decodedTool, len(request.Tools))
		for _, tool := range request.Tools {
			tools[tool.Function.Name] = tool
		}
		if len(tools) != 6 || tools[wire].Function.Name != wire || tools["shell"].Function.Parameters.Properties["command"] == nil {
			names := make([]string, 0, len(tools))
			for name := range tools {
				names = append(names, name)
			}
			t.Errorf("Responses did not merge caller tools with core compatibility tools: got %d tools %v", len(tools), names)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w,
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\""+wire+"\",\"arguments\":\"{\\\"cmd\\\":\\\"pwd\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"+
				"data: [DONE]\n\n")
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.FreeCompat = true
	// The readable alias is derived from the declared tool, not guessed by the
	// upstream response, so exercise a real declaration end to end.
	body := []byte(`{"model":"big-pickle","input":"run pwd","stream":false,"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"tool_choice":"required"}`)
	recorder := httptest.NewRecorder()
	p.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, routing.OpenCodeResponsesPath, strings.NewReader(string(body))), body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("tool response failed: %d %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"function_call"`) || !strings.Contains(recorder.Body.String(), `"name":"read_file"`) || !strings.Contains(recorder.Body.String(), `"call_id":"call_1"`) || !strings.Contains(recorder.Body.String(), `"arguments":"{\"cmd\":\"pwd\"}"`) {
		t.Fatalf("tool call lost: %s", recorder.Body)
	}
}

func TestOpenCodeResponsesRetriesCompatibilityTool(t *testing.T) {
	var firstRound, retryRound atomic.Int32
	var sawGuard, sawRetry atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		if strings.Contains(text, compatibilityToolInstructionText) {
			sawGuard.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(text, `"role":"tool"`) {
			retryRound.Add(1)
			if strings.Contains(text, "unavailable") && strings.Contains(text, "read_file") {
				sawRetry.Store(true)
			}
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"retried\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
			return
		}
		firstRound.Add(1)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_compat\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.FreeCompat = true
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"big-pickle","input":"run pwd","stream":%t,"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`, streaming))
			recorder := httptest.NewRecorder()
			p.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, routing.OpenCodeResponsesPath, strings.NewReader(string(body))), body)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "retried") {
				t.Fatalf("compatibility retry failed: %d %s", recorder.Code, recorder.Body)
			}
			if streaming && !strings.Contains(recorder.Body.String(), "event: response.completed") {
				t.Fatalf("retry did not produce a completed Responses stream: %s", recorder.Body)
			}
		})
	}
	if firstRound.Load() == 0 || retryRound.Load() == 0 {
		t.Fatalf("missing racing round: first=%d retry=%d", firstRound.Load(), retryRound.Load())
	}
	if !sawGuard.Load() || !sawRetry.Load() {
		t.Fatalf("guard=%t retry=%t", sawGuard.Load(), sawRetry.Load())
	}
}

func TestOpenCodeResponsesStopsRepeatedCompatibilityTools(t *testing.T) {
	var maxToolMessages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		count := int32(strings.Count(string(body), `"role":"tool"`))
		for {
			current := maxToolMessages.Load()
			if count <= current || maxToolMessages.CompareAndSwap(current, count) {
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_compat\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.FreeCompat = true
	body := []byte(`{"model":"big-pickle","input":"run pwd","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)
	recorder := httptest.NewRecorder()
	p.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, routing.OpenCodeResponsesPath, strings.NewReader(string(body))), body)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "repeatedly called compatibility-only tools") {
		t.Fatalf("retry cap failed: %d %s", recorder.Code, recorder.Body)
	}
	if maxToolMessages.Load() != maxCompatRetries {
		t.Fatalf("expected %d retries, saw %d tool results", maxCompatRetries, maxToolMessages.Load())
	}
}
