package opencode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

const firstTokenStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"winner\"}}]}\n\n"
const finishedTokenStream = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func TestFirstTokenGraceRange(t *testing.T) {
	p := &Proxy{}
	if grace := p.firstTokenTimeout(); grace != config.DefaultOpenCodeFirstTokenGrace {
		t.Fatalf("unexpected default grace: %s", grace)
	}
	p.cfg.FirstTokenTimeout = 3 * time.Minute
	if grace := p.firstTokenTimeout(); grace != 3*time.Minute {
		t.Fatalf("configured grace ignored: %s", grace)
	}
}

func TestFirstTokenRequiresOutput(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		field := "message"
		if streaming {
			field = "delta"
		}
		for _, item := range []struct {
			delta string
			valid bool
		}{
			{`{"role":"assistant"}`, false},
			{`{"content":"","reasoning_content":""}`, false},
			{`{"tool_calls":[{"index":0,"id":"call_1","type":"function"}]}`, false},
			{`{"tool_calls":[{"index":0,"function":{"arguments":""}}]}`, false},
			{`{"audio":{"id":"audio_1","expires_at":1}}`, false},
			{`{"content":[{"type":"text"}]}`, false},
			{`{"content":"a"}`, true},
			{`{"content":[{"type":"text","text":"a"}]}`, true},
			{`{"reasoning_content":"think"}`, true},
			{`{"reasoning":"think"}`, true},
			{`{"reasoning_text":"think"}`, true},
			{`{"tool_calls":[{"index":0,"function":{"name":"read"}}]}`, true},
			{`{"tool_calls":[{"index":0,"function":{"arguments":"{"}}]}`, true},
			{`{"tool_calls":[{"index":0,"custom":{"name":"run","input":"a"}}]}`, true},
			{`{"function_call":{"name":"read"}}`, true},
			{`{"refusal":"no"}`, true},
			{`{"audio":{"data":"AA=="}}`, true},
		} {
			body := fmt.Sprintf(`{"choices":[{%q:%s}]}`, field, item.delta)
			valid, err := validChat([]byte(body), streaming)
			if err != nil || valid != item.valid {
				t.Errorf("streaming=%v delta=%s valid=%v want=%v err=%v", streaming, item.delta, valid, item.valid, err)
			}
		}
	}
}

func TestFirstTokenTimeoutRefillsSlotsWithFreshSources(t *testing.T) {
	for _, mode := range []string{"no-headers", "metadata-heartbeats"} {
		t.Run(mode, func(t *testing.T) {
			var started atomic.Int32
			cleaned := make(chan struct{}, 10)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { cleaned <- struct{}{} }()
				io.Copy(io.Discard, r.Body)
				if started.Add(1) > 4 {
					io.WriteString(w, firstTokenStream+finishedTokenStream)
					return
				}
				if mode == "no-headers" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"empty\"}]}}]}\n\n")
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					io.WriteString(w, ": still alive\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-ticker.C:
					case <-r.Context().Done():
						return
					}
				}
			}))
			defer func() { upstream.CloseClientConnections(); upstream.Close() }()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.Concurrency = 2
			p.firstTokenGrace = func() time.Duration { return 100 * time.Millisecond }
			originalTransport := p.transport
			var lock sync.Mutex
			seenSources := make(map[string]bool)
			p.transport = func(source net.IP) *http.Transport {
				lock.Lock()
				if seenSources[source.String()] {
					t.Errorf("source reused on replay: %s", source)
				}
				seenSources[source.String()] = true
				lock.Unlock()
				return originalTransport(source)
			}
			recorder := httptest.NewRecorder()
			testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle","stream":true}`))
			if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"content":"winner"`) || started.Load() < 5 {
				t.Fatalf("no first-token replay: started=%d status=%d body=%s", started.Load(), recorder.Code, recorder.Body)
			}
			for i := int32(0); i < started.Load(); i++ {
				waitOpenCode(t, cleaned)
			}
		})
	}
}

func TestFirstTokenGraceIsIndependentPerSlot(t *testing.T) {
	var started, clocks atomic.Int32
	replayed := make(chan struct{})
	cleaned := make(chan struct{}, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { cleaned <- struct{}{} }()
		io.Copy(io.Discard, r.Body)
		if started.Add(1) > 2 {
			close(replayed)
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-replayed:
			if r.Context().Err() == nil {
				io.WriteString(w, firstTokenStream+finishedTokenStream)
			}
		case <-r.Context().Done():
		}
	}))
	defer func() { upstream.CloseClientConnections(); upstream.Close() }()
	p := testOpenCodeProxy(t, upstream.URL)
	p.cfg.Concurrency = 2
	p.firstTokenGrace = func() time.Duration {
		if clocks.Add(1) == 1 {
			return 150 * time.Millisecond
		}
		return time.Second
	}
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle","stream":true}`))
	if recorder.Code != 200 || started.Load() != 3 || !strings.Contains(recorder.Body.String(), "winner") {
		t.Fatalf("unexpired slot was discarded: started=%d status=%d body=%s", started.Load(), recorder.Code, recorder.Body)
	}
	for i := 0; i < 3; i++ {
		waitOpenCode(t, cleaned)
	}
}

func TestFirstTokenWinnerStopsReplayForJSONAndSSE(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			var started atomic.Int32
			loserCancelled := make(chan struct{}, 1)
			bothStarted := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				id := started.Add(1)
				if id == 2 {
					close(bothStarted)
				}
				select {
				case <-bothStarted:
				case <-r.Context().Done():
					return
				}
				if id != 1 {
					<-r.Context().Done()
					loserCancelled <- struct{}{}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, firstTokenStream)
				w.(http.Flusher).Flush()
				// A JSON caller must already have selected this winner and
				// cancelled the loser before waiting for the complete answer.
				select {
				case <-loserCancelled:
				case <-r.Context().Done():
					return
				}
				for i := 0; i < 8; i++ {
					select {
					case <-time.After(30 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					io.WriteString(w, ": working\n\n")
					w.(http.Flusher).Flush()
				}
				io.WriteString(w, finishedTokenStream)
			}))
			defer func() { upstream.CloseClientConnections(); upstream.Close() }()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.FreeCompat = true
			p.cfg.Concurrency = 2
			p.cfg.WaitTimeout = 180 * time.Millisecond
			p.firstTokenGrace = func() time.Duration { return 100 * time.Millisecond }
			recorder := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"big-pickle","stream":%v}`, streaming)
			testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(body))
			if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "winner") || started.Load() != 2 {
				t.Fatalf("winner replayed or lost: attempts=%d status=%d body=%s", started.Load(), recorder.Code, recorder.Body)
			}
			if streaming && !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") {
				t.Fatal("healthy stream interrupted after first token grace")
			}
		})
	}
}

func TestFirstTokenReplaysStopOnBudgetOrCallerCancellation(t *testing.T) {
	for _, callerCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(callerCancel), func(t *testing.T) {
			var started atomic.Int32
			replayed := make(chan struct{})
			cleaned := make(chan struct{}, 10)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { cleaned <- struct{}{} }()
				io.Copy(io.Discard, r.Body)
				if started.Add(1) == 2 {
					close(replayed)
				}
				<-r.Context().Done()
			}))
			defer func() { upstream.CloseClientConnections(); upstream.Close() }()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.Concurrency = 1
			p.cfg.WaitTimeout = 250 * time.Millisecond
			p.firstTokenGrace = func() time.Duration { return 100 * time.Millisecond }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder, done := httptest.NewRecorder(), make(chan struct{})
			go func() {
				defer close(done)
				testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle","stream":true}`).WithContext(ctx))
			}()
			waitOpenCode(t, replayed)
			if callerCancel {
				cancel()
			}
			waitOpenCode(t, done)
			if callerCancel && recorder.Body.Len() != 0 {
				t.Fatalf("wrote response after client cancellation: %s", recorder.Body)
			}
			if !callerCancel && recorder.Code != 504 {
				t.Fatalf("overall budget not enforced: %d %s", recorder.Code, recorder.Body)
			}
			for i := int32(0); i < started.Load(); i++ {
				waitOpenCode(t, cleaned)
			}
		})
	}
}

func TestFirstTokenGraceStartsAfterUpload(t *testing.T) {
	p := testOpenCodeProxy(t, "http://opencode.test")
	p.transport = func(net.IP) *http.Transport {
		return &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				request, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					return
				}
				defer request.Body.Close()
				buffer := make([]byte, 8<<10)
				for {
					n, err := request.Body.Read(buffer)
					if n > 0 {
						time.Sleep(4 * time.Millisecond)
					}
					if err == io.EOF {
						break
					}
					if err != nil {
						return
					}
				}
				io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n"+firstTokenStream+finishedTokenStream)
			}()
			return client, nil
		}}
	}
	p.cfg.Concurrency = 1
	p.cfg.WaitTimeout = 4 * time.Second
	p.firstTokenGrace = func() time.Duration { return 80 * time.Millisecond }
	body := fmt.Sprintf(`{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":%q}]}`, strings.Repeat("x", 512<<10))
	recorder := httptest.NewRecorder()
	started := time.Now()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(body))
	elapsed := time.Since(started)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "winner") {
		t.Fatalf("slow upload lost its first-token grace: status=%d elapsed=%s", recorder.Code, elapsed)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("test did not exercise a slow upload: %s", elapsed)
	}
}
