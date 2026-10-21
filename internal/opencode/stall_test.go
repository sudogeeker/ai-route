package opencode

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	routing "aizasy.pages.dev/ai-route/internal/server"
)

func TestSilentUpstreamCannotHoldRequestForever(t *testing.T) {
	for _, item := range []struct {
		name, path, request, delta string
		wantStatus                 int
		wantBody                   string
	}{
		{"role-only", routing.OpenCodeResponsesPath, `{"model":"big-pickle","stream":true,"input":"hi"}`, `{"role":"assistant"}`, 502, "All 10 OpenCode attempts failed"},
		{"compat-probe", routing.OpenCodeResponsesPath, `{"model":"big-pickle","stream":true,"input":"hi"}`, `{"content":"short preamble"}`, 502, "stopped sending data"},
		{"responses-stream", routing.OpenCodeResponsesPath, `{"model":"big-pickle","stream":true,"input":"hi"}`, `{"content":"` + strings.Repeat("x", 256) + `"}`, 200, "response.failed"},
		{"responses-json", routing.OpenCodeResponsesPath, `{"model":"big-pickle","stream":false,"input":"hi"}`, `{"content":"` + strings.Repeat("x", 256) + `"}`, 502, "stopped sending data"},
		{"chat-stream", routing.ChatPath, `{"model":"big-pickle","stream":true}`, `{"content":"partial"}`, 200, "partial"},
	} {
		t.Run(item.name, func(t *testing.T) {
			var started, finished atomic.Int32
			ready, cleaned := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					if finished.Add(1) == maxAttempts {
						close(cleaned)
					}
				}()
				io.Copy(io.Discard, r.Body)
				if started.Add(1) == maxAttempts {
					close(ready)
				}
				select {
				case <-ready:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}]}\n\n", item.delta)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer func() { upstream.CloseClientConnections(); upstream.Close() }()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.FreeCompat = true
			p.cfg.StreamIdleTimeout = 150 * time.Millisecond
			proxy := httptest.NewServer(testOpenCodeHandler(p))
			defer func() { proxy.CloseClientConnections(); proxy.Close() }()
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Post(proxy.URL+item.path, "application/json", strings.NewReader(item.request))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			// A direct Chat stream has no Responses response.failed event.
			// Closing an incomplete HTTP stream is also a valid failure signal.
			readFailed := err != nil && !(item.name == "chat-stream" && errors.Is(err, io.ErrUnexpectedEOF))
			if readFailed || resp.StatusCode != item.wantStatus || !strings.Contains(string(body), item.wantBody) {
				t.Fatalf("stalled request did not terminate correctly: status=%d body=%s err=%v", resp.StatusCode, body, err)
			}
			if strings.Contains(string(body), "response.completed") {
				t.Fatal("a stalled stream was reported as a complete response")
			}
			waitOpenCode(t, cleaned)
		})
	}
}

func TestRoleOnlyPreludeCannotBeatUsefulOutput(t *testing.T) {
	roleSent := make(chan struct{})
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts.Add(1) == 1 {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			w.(http.Flusher).Flush()
			close(roleSent)
			<-r.Context().Done()
			return
		}
		select {
		case <-roleSent:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"useful winner\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer func() { upstream.CloseClientConnections(); upstream.Close() }()
	p := testOpenCodeProxy(t, upstream.URL)
	p.cfg.StreamIdleTimeout = 150 * time.Millisecond
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle","stream":true}`))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "useful winner") {
		t.Fatalf("role prelude won the race: %d %s", recorder.Code, recorder.Body)
	}
}

func TestChatDoneFinishesWithoutWaitingForUpstreamEOF(t *testing.T) {
	const stream = ": ping\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, stream)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer func() { upstream.CloseClientConnections(); upstream.Close() }()
	p := testOpenCodeProxy(t, upstream.URL)
	// Much longer than the caller's budget: [DONE], not an idle timeout,
	// must be what completes the request and cancels the upstream.
	p.cfg.StreamIdleTimeout = 10 * time.Second
	proxy := httptest.NewServer(testOpenCodeHandler(p))
	defer func() { proxy.CloseClientConnections(); proxy.Close() }()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Post(proxy.URL+routing.ChatPath, "application/json", strings.NewReader(`{"model":"big-pickle","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != stream {
		t.Fatalf("finished chat stalled or changed bytes: %q %v", body, err)
	}
}

func TestHealthyStreamCanOutliveBothTimeoutBudgets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", i)
			w.(http.Flusher).Flush()
			select {
			case <-time.After(40 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	p := testOpenCodeProxy(t, upstream.URL)
	p.cfg.WaitTimeout = 100 * time.Millisecond
	p.cfg.StreamIdleTimeout = 150 * time.Millisecond
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle","stream":true}`))
	if recorder.Code != 200 || !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") || !strings.Contains(recorder.Body.String(), `"content":"7"`) {
		t.Fatalf("healthy long stream was interrupted: %d %s", recorder.Code, recorder.Body)
	}
}
