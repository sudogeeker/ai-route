package logging

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockedWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(data), nil
}

func TestBlockedOutputDoesNotBlockRequests(t *testing.T) {
	for _, level := range []int{LevelDebug, LevelInfo} {
		t.Run(map[int]string{LevelDebug: "debug", LevelInfo: "info"}[level], func(t *testing.T) {
			output := &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
			logger := New(level, output)
			handler := logger.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				logger.Debugf("upstream completed")
				io.WriteString(w, `{"ok":true}`)
			}))
			server := httptest.NewServer(handler)
			defer func() {
				close(output.release)
				server.Close()
				logger.Close()
			}()
			client := &http.Client{Timeout: 500 * time.Millisecond}
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatalf("a blocked log sink stalled the HTTP response: %v", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != `{"ok":true}` {
				t.Fatalf("response stalled: %q %v", body, err)
			}
			select {
			case <-output.started:
			case <-time.After(time.Second):
				t.Fatal("test did not exercise a blocked output")
			}
			// Saturate the bounded queue while its only writer is blocked.
			done := make(chan struct{})
			go func() {
				for i := 0; i < 4096; i++ {
					logger.Errorf("queued message %d", i)
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("a full log queue blocked the caller")
			}
		})
	}
}

func TestCloseFlushesLogsAndRejectsLateWrites(t *testing.T) {
	var output bytes.Buffer
	logger := New(LevelInfo, &output)
	logger.Infof("before close")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	logger.Errorf("after close")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "before close") || strings.Contains(output.String(), "after close") {
		t.Fatalf("incorrect log lifecycle: %s", output.String())
	}
}

func TestCloseDoesNotHangOnBlockedOutput(t *testing.T) {
	output := &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
	logger := New(LevelError, output)
	defer close(output.release)
	go logger.Errorf("blocked")
	select {
	case <-output.started:
	case <-time.After(time.Second):
		t.Fatal("logger did not reach output")
	}
	done := make(chan error, 1)
	go func() { done <- logger.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, errLogOutputBlocked) {
			t.Fatalf("unexpected close result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing a blocked log sink never returned")
	}
}

func TestConcurrentLoggingAndClose(t *testing.T) {
	logger := New(LevelDebug, io.Discard)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				logger.Debugf("concurrent message %d", j)
			}
		}()
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
}
