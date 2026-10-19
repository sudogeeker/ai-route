package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testChatProvider struct{ t *testing.T }

func (p testChatProvider) SupportsModel(model string) bool { return model == "big-pickle" }
func (p testChatProvider) ServeChat(http.ResponseWriter, *http.Request, []byte, bool) {
	p.t.Error("a Gemini or invalid request reached the OpenCode provider")
}

func TestModelRouterPreservesGemini(t *testing.T) {
	for _, item := range []struct{ method, path, body string }{
		{"GET", "/v1/models", ""}, {"POST", "/v1beta/models/test:generateContent", `{"contents":[]}`},
		{"POST", "/upload/v1beta/files", "binary"}, {"GET", "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent", ""},
		{"POST", ChatPath, `{ "model": "gemini-test", "messages": [] }`},
	} {
		t.Run(item.path+item.method, func(t *testing.T) {
			called := false
			gemini := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				got, _ := io.ReadAll(r.Body)
				if r.URL.Path != item.path || string(got) != item.body || r.Header.Get("Authorization") != "Bearer original" {
					t.Error("Gemini request changed")
				}
				w.WriteHeader(204)
			})
			r := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
			r.Header.Set("Authorization", "Bearer original")
			recorder := httptest.NewRecorder()
			NewRouter(gemini, testChatProvider{t: t}, "", 64<<20).ServeHTTP(recorder, r)
			if !called || recorder.Code != 204 {
				t.Error("Gemini route not preserved")
			}
		})
	}
	for _, body := range []string{`not json`, `{}`, `{"model":"unknown-free"}`} {
		recorder := httptest.NewRecorder()
		fallback := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached Gemini") })
		NewRouter(fallback, testChatProvider{t: t}, "", 64<<20).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, ChatPath, strings.NewReader(body)))
		if recorder.Code != 400 {
			t.Errorf("invalid/unsupported request returned %d", recorder.Code)
		}
	}
}

type testResponsesProvider struct {
	t        *testing.T
	gemini   *bool
	response *string
}

func (p testResponsesProvider) SupportsModel(model string) bool { return model == "big-pickle" }
func (p testResponsesProvider) ServeChat(http.ResponseWriter, *http.Request, []byte, bool) {
	p.t.Error("unexpected chat request")
}
func (p testResponsesProvider) ServeResponses(w http.ResponseWriter, _ *http.Request, body []byte) {
	*p.response = string(body)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"resp_test"}`))
}
func (p testResponsesProvider) Models() []string { return []string{"big-pickle", "other-free"} }

func TestResponsesRoutes(t *testing.T) {
	model := "big-pickle"
	for _, item := range []struct {
		name, method, path, body string
		status                   int
		provider, gemini         bool
	}{
		{"opencode model on v1", "POST", ResponsesPath, `{"model":"big-pickle","input":"hi"}`, 200, true, false},
		{"gemini model on v1", "POST", ResponsesPath, `{"model":"gemini-test","input":"hi"}`, 418, false, true},
		{"opencode model on dedicated path", "POST", OpenCodeResponsesPath, `{"model":"big-pickle","input":"hi"}`, 200, true, false},
		{"gemini model on dedicated path", "POST", OpenCodeResponsesPath, `{"model":"gemini-test","input":"hi"}`, 400, false, false},
		{"unknown free model", "POST", OpenCodeResponsesPath, `{"model":"unknown-free"}`, 400, false, false},
		{"invalid body", "POST", OpenCodeResponsesPath, `not json`, 400, false, false},
		{"model list", "GET", OpenCodeModelsPath, "", 200, false, false},
		{"single model", "GET", OpenCodeModelsPath + "/big-pickle", "", 200, false, false},
		{"unknown model", "GET", OpenCodeModelsPath + "/missing", "", 404, false, false},
		{"unknown opencode path", "GET", OpenCodePrefix + "/other", "", 404, false, false},
		{"wrong responses method", "GET", OpenCodeResponsesPath, "", 405, false, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			providerCalled, geminiCalled := false, false
			gemini := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				geminiCalled = true
				got, _ := io.ReadAll(r.Body)
				if string(got) != item.body {
					t.Errorf("Gemini body changed: %q", got)
				}
				w.WriteHeader(418)
			})
			provider := testResponsesProvider{t: t, gemini: &geminiCalled, response: &model}
			// Replace the model handler so the boolean reflects routing without
			// adding another test-only type.
			handler := NewRouter(gemini, responseRecorderProvider{testResponsesProvider: provider, called: &providerCalled}, "", 64<<20)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(item.method, item.path, strings.NewReader(item.body)))
			if recorder.Code != item.status {
				t.Fatalf("status %d, want %d: %s", recorder.Code, item.status, recorder.Body)
			}
			if providerCalled != item.provider || geminiCalled != item.gemini {
				t.Fatalf("provider=%t gemini=%t, want provider=%t gemini=%t", providerCalled, geminiCalled, item.provider, item.gemini)
			}
			if item.path == OpenCodeModelsPath && (!strings.Contains(recorder.Body.String(), "big-pickle") || !strings.Contains(recorder.Body.String(), `"object":"list"`)) {
				t.Fatalf("bad model list: %s", recorder.Body)
			}
		})
	}
}

type responseRecorderProvider struct {
	testResponsesProvider
	called *bool
}

func (p responseRecorderProvider) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	*p.called = true
	p.testResponsesProvider.ServeResponses(w, r, body)
}

type authTestProvider struct {
	chatCalls     int
	responseCalls int
}

func (p *authTestProvider) SupportsModel(model string) bool { return model == "big-pickle" }
func (p *authTestProvider) ServeChat(w http.ResponseWriter, _ *http.Request, _ []byte, _ bool) {
	p.chatCalls++
	w.WriteHeader(http.StatusOK)
}
func (p *authTestProvider) ServeResponses(w http.ResponseWriter, _ *http.Request, _ []byte) {
	p.responseCalls++
	w.WriteHeader(http.StatusOK)
}
func (p *authTestProvider) Models() []string { return []string{"big-pickle"} }

func TestOpenCodeAuth(t *testing.T) {
	const key = "secret-key"
	newRouter := func() (*authTestProvider, *int, http.Handler) {
		provider := &authTestProvider{}
		geminiCalls := 0
		gemini := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			geminiCalls++
			w.WriteHeader(418)
		})
		return provider, &geminiCalls, NewRouter(gemini, provider, key, 64<<20)
	}
	do := func(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("model list", func(t *testing.T) {
		provider, _, handler := newRouter()
		for _, test := range []struct {
			name    string
			headers map[string]string
			status  int
		}{
			{"missing", nil, 401},
			{"wrong bearer", map[string]string{"Authorization": "Bearer wrong"}, 401},
			{"bearer", map[string]string{"Authorization": "Bearer " + key}, 200},
			{"x-api-key", map[string]string{"x-api-key": key}, 200},
			{"api-key", map[string]string{"api-key": key}, 200},
			{"raw authorization", map[string]string{"Authorization": key}, 200},
		} {
			t.Run(test.name, func(t *testing.T) {
				recorder := do(handler, http.MethodGet, OpenCodeModelsPath, "", test.headers)
				if recorder.Code != test.status {
					t.Fatalf("status %d, want %d: %s", recorder.Code, test.status, recorder.Body)
				}
				if test.status == http.StatusUnauthorized {
					if recorder.Header().Get("WWW-Authenticate") == "" || !strings.Contains(recorder.Body.String(), "authentication_error") {
						t.Fatalf("missing authentication challenge: %v %s", recorder.Header(), recorder.Body)
					}
				}
			})
		}
		if provider.chatCalls != 0 || provider.responseCalls != 0 {
			t.Fatal("unauthorized model request reached the provider")
		}
	})

	t.Run("dedicated responses", func(t *testing.T) {
		provider, _, handler := newRouter()
		body := `{"model":"big-pickle","input":"hi"}`
		if recorder := do(handler, http.MethodPost, OpenCodeResponsesPath, body, nil); recorder.Code != 401 {
			t.Fatalf("unauthenticated responses returned %d: %s", recorder.Code, recorder.Body)
		}
		if provider.responseCalls != 0 {
			t.Fatal("unauthenticated responses reached the provider")
		}
		if recorder := do(handler, http.MethodPost, OpenCodeResponsesPath, body, map[string]string{"Authorization": "Bearer " + key}); recorder.Code != 200 || provider.responseCalls != 1 {
			t.Fatalf("authenticated responses returned %d: %s", recorder.Code, recorder.Body)
		}
	})

	t.Run("compatibility responses", func(t *testing.T) {
		provider, geminiCalls, handler := newRouter()
		if recorder := do(handler, http.MethodPost, ResponsesPath, `{"model":"big-pickle","input":"hi"}`, nil); recorder.Code != 401 || provider.responseCalls != 0 {
			t.Fatalf("unauthenticated compatibility response returned %d: %s", recorder.Code, recorder.Body)
		}
		if recorder := do(handler, http.MethodPost, ResponsesPath, `{"model":"big-pickle","input":"hi"}`, map[string]string{"x-api-key": key}); recorder.Code != 200 || provider.responseCalls != 1 {
			t.Fatalf("authenticated compatibility response returned %d: %s", recorder.Code, recorder.Body)
		}
		if recorder := do(handler, http.MethodPost, ResponsesPath, `{"model":"gemini-test","input":"hi"}`, nil); recorder.Code != 418 || *geminiCalls != 1 {
			t.Fatalf("Gemini compatibility route was not preserved: %d %s", recorder.Code, recorder.Body)
		}
	})

	t.Run("compatibility chat", func(t *testing.T) {
		provider, _, handler := newRouter()
		body := `{"model":"big-pickle","messages":[],"stream":false}`
		if recorder := do(handler, http.MethodPost, ChatPath, body, nil); recorder.Code != 401 || provider.chatCalls != 0 {
			t.Fatalf("unauthenticated chat returned %d: %s", recorder.Code, recorder.Body)
		}
		if recorder := do(handler, http.MethodPost, ChatPath, body, map[string]string{"Authorization": "Bearer " + key}); recorder.Code != 200 || provider.chatCalls != 1 {
			t.Fatalf("authenticated chat returned %d: %s", recorder.Code, recorder.Body)
		}
	})

	t.Run("unknown opencode path", func(t *testing.T) {
		_, _, handler := newRouter()
		if recorder := do(handler, http.MethodGet, OpenCodePrefix+"/unknown", "", nil); recorder.Code != 401 {
			t.Fatalf("unauthenticated unknown path returned %d: %s", recorder.Code, recorder.Body)
		}
		if recorder := do(handler, http.MethodGet, OpenCodePrefix+"/unknown", "", map[string]string{"Authorization": "Bearer " + key}); recorder.Code != 404 {
			t.Fatalf("authenticated unknown path returned %d: %s", recorder.Code, recorder.Body)
		}
	})

	t.Run("empty key preserves access", func(t *testing.T) {
		provider := &authTestProvider{}
		handler := NewRouter(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), provider, "", 64<<20)
		if recorder := do(handler, http.MethodGet, OpenCodeModelsPath, "", nil); recorder.Code != 200 {
			t.Fatalf("empty auth key blocked access: %d %s", recorder.Code, recorder.Body)
		}
	})
}
