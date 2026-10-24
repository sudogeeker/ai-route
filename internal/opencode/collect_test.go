package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const completeStream = ": ping\r\n\r\n" +
	"data: {\"id\":\"chat-test\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\",\"reasoning_content\":\"think \"},\"finish_reason\":null}]}\r\n\r\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\",\"reasoning_content\":\"more\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

func TestCollectChat(t *testing.T) {
	data, err := collectChat(strings.NewReader(completeStream))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Created int    `json:"created"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Role      string `json:"role"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "chat-test" || response.Object != "chat.completion" || response.Model != "big-pickle" || response.Created != 123 || response.Usage.TotalTokens != 5 || len(response.Choices) != 1 {
		t.Fatalf("lost chat metadata: %s", data)
	}
	choice := response.Choices[0]
	if choice.Message.Content != "hello world" || choice.Message.Role != "assistant" || choice.Message.Reasoning != "think more" || choice.FinishReason != "stop" {
		t.Fatalf("lost chat deltas: %s", data)
	}
}

func TestCollectChatRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{
		"data: [DONE]\n\n",
		"event: error\ndata: {}\n\n",
		"data: {\"error\":{\"message\":\"no\"}}\n\n",
		"data: {invalid}\n\n",
		"data: null\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"no index\"}}]}\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"unfinished\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\n",
		strings.TrimSuffix(completeStream, "data: [DONE]\n\n"),
		strings.TrimSuffix(completeStream, "\n\n"),
		":" + strings.Repeat("x", maxEventBytes),
	} {
		if data, err := collectChat(strings.NewReader(body)); err == nil {
			t.Errorf("invalid stream became JSON: %s", data)
		}
	}
}

func TestCollectChatMultipleChoicesAndLogprobs(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"second\"},\"finish_reason\":\"length\"},{\"index\":0,\"delta\":{\"content\":\"a\",\"annotations\":[1]},\"logprobs\":{\"content\":[{\"token\":\"a\"}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\",\"annotations\":[2]},\"finish_reason\":\"stop\",\"logprobs\":{\"content\":[{\"token\":\"b\"}]}}]}\n\n" +
		"data: {\"usage\":{\"total_tokens\":3}}\n\ndata: [DONE]\n\n"
	data, err := collectChat(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content     string `json:"content"`
				Annotations []int  `json:"annotations"`
			} `json:"message"`
			Logprobs struct {
				Content []json.RawMessage `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &response) != nil || len(response.Choices) != 2 {
		t.Fatalf("invalid multi-choice response: %s", data)
	}
	first, second := response.Choices[0], response.Choices[1]
	if first.Index != 0 || first.Message.Content != "ab" || len(first.Message.Annotations) != 2 || len(first.Logprobs.Content) != 2 || second.Index != 1 || second.Message.Content != "second" {
		t.Fatalf("lost or reordered deltas: %s", data)
	}
}

func TestOpenCodeIncompleteCompatibilityStreamDoesNotWin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.TrimSuffix(completeStream, "data: [DONE]\n\n"))
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.cfg.FreeCompat = true
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("truncated stream won: %d %s", recorder.Code, recorder.Body)
	}
}

func TestOpenCodeCompatibilityResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Stream     bool              `json:"stream"`
				Tools      []json.RawMessage `json:"tools"`
				ToolChoice string            `json:"tool_choice"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				// A canceled losing attempt may not deliver its full body.
				return
			}
			if !request.Stream || len(request.Tools) != 11 || request.ToolChoice != "auto" {
				t.Error("upstream request did not use disabled compatibility tools and SSE")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("ETag", "upstream-stream")
			io.WriteString(w, completeStream)
		}))
		p := testOpenCodeProxy(t, server.URL)
		p.cfg.FreeCompat = true
		recorder := httptest.NewRecorder()
		body := `{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`
		if streaming {
			body = strings.TrimSuffix(body, "}") + `,"stream":true}`
		}
		testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(body))
		if recorder.Code != http.StatusOK {
			t.Fatalf("compatibility failed: %d %s", recorder.Code, recorder.Body)
		}
		if streaming {
			if recorder.Body.String() != completeStream || recorder.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatal("SSE bytes or content type changed")
			}
		} else if recorder.Header().Get("Content-Type") != "application/json" || recorder.Header().Get("ETag") != "" || !json.Valid(recorder.Body.Bytes()) || !strings.Contains(recorder.Body.String(), "hello world") {
			t.Fatalf("invalid collected JSON response: %v %s", recorder.Header(), recorder.Body)
		}
		server.Close()
	}
}
