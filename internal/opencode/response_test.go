package opencode

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aizasy.pages.dev/ai-route/internal/httpx"
)

func TestOpenCodeInvalidStreams(t *testing.T) {
	for _, body := range []string{
		"event: error\ndata: {\"message\":\"refused\"}\n\n",
		"data: {\"error\":{\"message\":\"refused\"}}\n\n",
		"data: [DONE]\n\n", "data: {invalid}\n\n", "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"truncated\"}}]}",
	} {
		t.Run(body, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { attempts.Add(1); io.WriteString(w, body) }))
			defer server.Close()
			p := testOpenCodeProxy(t, server.URL)
			recorder := httptest.NewRecorder()
			testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle","stream":true}`))
			if recorder.Code != 502 || attempts.Load() != 10 {
				t.Fatalf("invalid stream won: %d attempts=%d", recorder.Code, attempts.Load())
			}
		})
	}
}

func TestOpenCodeEventPreludeBounded(t *testing.T) {
	_, _, err := readChatEvent(strings.NewReader(":" + strings.Repeat("x", maxEventBytes)))
	if err == nil {
		t.Fatal("oversized SSE prelude accepted")
	}
	for _, data := range []string{`null`, `{}`, `{"choices":[]}`, `{"choices":[{"message":{}}]}`, `{"choices":[`} {
		valid, _ := validChat([]byte(data), false)
		if valid {
			t.Errorf("invalid chat accepted: %s", data)
		}
	}
	var dst = make(http.Header)
	httpx.CopyHeaders(dst, http.Header{"Connection": []string{"X-Private"}, "X-Private": []string{"hidden"}, "Content-Length": []string{"100"}, "Content-Type": []string{"application/json"}})
	if dst.Get("X-Private") != "" || dst.Get("Content-Length") != "" || dst.Get("Content-Type") == "" {
		t.Fatal("hop-by-hop or stale length header copied")
	}
}
