package opencode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfiguredConcurrencyWinnerCancelsOtherAttempts(t *testing.T) {
	for _, count := range []int{1, 3, 16} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var started, finished atomic.Int32
			ready, cleaned := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					if finished.Add(1) == int32(count) {
						close(cleaned)
					}
				}()
				io.Copy(io.Discard, r.Body)
				id := started.Add(1)
				if id == int32(count) {
					close(ready)
				}
				select {
				case <-ready:
				case <-r.Context().Done():
					return
				}
				if id == 1 {
					io.WriteString(w, openCodeOK)
					return
				}
				<-r.Context().Done()
			}))
			defer func() { upstream.CloseClientConnections(); upstream.Close() }()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.Concurrency = count
			recorder := httptest.NewRecorder()
			testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
			if recorder.Code != 200 || recorder.Body.String() != openCodeOK || started.Load() != int32(count) {
				t.Fatalf("attempts=%d, want=%d, status=%d body=%s", started.Load(), count, recorder.Code, recorder.Body)
			}
			waitOpenCode(t, cleaned)
		})
	}
}

func TestConfiguredConcurrencyAllFailures(t *testing.T) {
	for _, count := range []int{1, 3, 16} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				attempts.Add(1)
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.Concurrency = count
			recorder := httptest.NewRecorder()
			testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
			wantError := fmt.Sprintf("All %d OpenCode attempts failed", count)
			if attempts.Load() != int32(count) || recorder.Code != 502 || !strings.Contains(recorder.Body.String(), wantError) {
				t.Fatalf("attempts=%d, want=%d, status=%d body=%s", attempts.Load(), count, recorder.Code, recorder.Body)
			}
		})
	}
}

func TestConfiguredConcurrencyTimeoutAndCancellation(t *testing.T) {
	for _, callerCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(callerCancel), func(t *testing.T) {
			const count = 3
			var started, finished atomic.Int32
			ready, cleaned := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					if finished.Add(1) == count {
						close(cleaned)
					}
				}()
				io.Copy(io.Discard, r.Body)
				if started.Add(1) == count {
					close(ready)
				}
				<-r.Context().Done()
			}))
			defer func() { upstream.CloseClientConnections(); upstream.Close() }()
			p := testOpenCodeProxy(t, upstream.URL)
			p.cfg.Concurrency = count
			p.cfg.WaitTimeout = 200 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recorder, done := httptest.NewRecorder(), make(chan struct{})
			go func() {
				defer close(done)
				testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`).WithContext(ctx))
			}()
			waitOpenCode(t, ready)
			if callerCancel {
				cancel()
			}
			waitOpenCode(t, done)
			waitOpenCode(t, cleaned)
			if !callerCancel && recorder.Code != 504 {
				t.Fatalf("expected timeout, got %d", recorder.Code)
			}
			if callerCancel && recorder.Body.Len() != 0 {
				t.Fatalf("response after caller cancellation: %s", recorder.Body)
			}
		})
	}
}
