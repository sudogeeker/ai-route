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
		{"POST", "/v1beta/openai/chat/completions", `{"model":"gemini-test"}`},
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
			NewRouter(gemini, nil, testChatProvider{t: t}, "", 64<<20, "").ServeHTTP(recorder, r)
			if !called || recorder.Code != 204 {
				t.Error("Gemini route not preserved")
			}
		})
	}
	for _, body := range []string{`not json`, `{}`, `{"model":"unknown-free"}`} {
		recorder := httptest.NewRecorder()
		fallback := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached Gemini") })
		NewRouter(fallback, nil, testChatProvider{t: t}, "", 64<<20, "").ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, ChatPath, strings.NewReader(body)))
		if recorder.Code != 400 {
			t.Errorf("invalid/unsupported request returned %d", recorder.Code)
		}
	}
}

func TestDisabledProvidersDoNotInterceptFallback(t *testing.T) {
	gemini := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "unchanged" {
			t.Errorf("unexpected forwarded body: %q, %v", body, err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := Allowlist(NewRouter(gemini, nil, nil, "unused", 0, ""))
	for _, test := range []struct {
		path string
		want int
	}{
		{ChatPath, http.StatusNoContent},
		{ResponsesPath, http.StatusNoContent},
		{OpenCodeModelsPath, http.StatusNotFound},
		{OpenCodeResponsesPath, http.StatusNotFound},
		{OpenAIPrefix + "/responses", http.StatusNotFound},
		{"/openai/v10/responses", http.StatusNotFound},
		{"/opencode/v10/responses", http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("unchanged")))
		if recorder.Code != test.want {
			t.Errorf("%s: got %d, want %d", test.path, recorder.Code, test.want)
		}
	}
}

func TestNativeOpenAIRoutes(t *testing.T) {
	const host = "openai.gateway.example"
	const body = `{"model":"future-model-free"}`
	for _, test := range []struct {
		name, inbound, host, path, upstreamPath string
		status                                  int
	}{
		{"models", host, host, "/v1/models", "/v1/models", 200},
		{"model details", host, host, "/v1/models/project:test", "/v1/models/project:test", 200},
		{"chat", host, host, ChatPath, ChatPath, 200},
		{"responses", host, "OPENAI.gateway.example:8443", ResponsesPath, ResponsesPath, 200},
		{"speech", host, host, "/v1/audio/speech", "/v1/audio/speech", 200},
		{"transcribe", host, host, "/v1/audio/transcriptions", "/v1/audio/transcriptions", 200},
		{"translate", host, host, "/v1/audio/translations", "/v1/audio/translations", 200},
		{"realtime calls", host, host, "/v1/realtime/calls", "/v1/realtime/calls", 200},
		{"future endpoint", host, host, "/v1/new/endpoint", "/v1/new/endpoint", 200},
		{"old alias", host, "gemini.example", "/openai/v1/responses", ResponsesPath, 200},
		{"Gemini", host, "gemini.example", "/v1/models", "", 418},
		{"no forwarded host trust", host, "gemini.example", "/v1/audio/speech", "", 418},
		{"dedicated host isolation", host, host, "/opencode/v1/models", "", 404},
		{"single host native route", "", "gateway.example", ResponsesPath, ResponsesPath, 200},
		{"single host Gemini intact", "", "gateway.example", "/v1beta/openai/models", "", 418},
	} {
		t.Run(test.name, func(t *testing.T) {
			free := &authTestProvider{}
			gemini := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) })
			openai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != body || r.URL.Path != test.upstreamPath ||
					r.URL.RawQuery != "extra=value%2Fvalue" || r.Header.Get("Authorization") != "Bearer openai-test" {
					t.Errorf("modified OpenAI request: %s, %q, %v", r.URL, data, err)
				}
				w.WriteHeader(http.StatusOK)
			})
			handler := Allowlist(NewRouter(gemini, openai, free, "opencode-secret", 1, test.inbound))
			req := httptest.NewRequest(http.MethodPost, test.path+"?extra=value%2Fvalue", strings.NewReader(body))
			req.Host = test.host
			req.Header.Set("Authorization", "Bearer openai-test")
			req.Header.Set("X-Forwarded-Host", host)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != test.status || free.chatCalls+free.responseCalls != 0 {
				t.Fatalf("status=%d, OpenCode chat=%d responses=%d", recorder.Code, free.chatCalls, free.responseCalls)
			}
		})
	}
}

func TestSharedGateway(t *testing.T) {
	upstream := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Provider", name)
			w.Header().Set("Forwarded-URI", r.URL.RequestURI())
			w.WriteHeader(http.StatusNoContent)
		})
	}
	router := Allowlist(NewRouter(upstream("gemini"), upstream("openai"), nil, "", 0, ""))
	for _, test := range []struct {
		path, googleKey, bearer string
		status                  int
		provider, uri           string
	}{
		{"/v1/models", "", "Bearer test", 204, "openai", "/v1/models"},
		{"/v1/models", "test", "", 204, "gemini", "/v1/models"},
		{"/v1/models/demo:generateContent?key=test&extra=a%2Fb", "", "", 204, "gemini", "/v1/models/demo:generateContent?key=test&extra=a%2Fb"},
		{"/v1/models?key=test", "", "Bearer test", 400, "", ""},
		{"/v1/models", "test", "Bearer test", 400, "", ""},
		{"/gemini/v1/models?extra=a%2Fb", "", "Bearer oauth", 204, "gemini", "/v1/models?extra=a%2Fb"},
		{"/gemini/v10/models", "", "Bearer oauth", 404, "", ""},
		{"/gemini/v1beta/models", "", "", 404, "", ""},
		{"/v1beta/models", "", "Bearer oauth", 204, "gemini", "/v1beta/models"},
		{"/v1beta/openai/chat/completions", "", "Bearer test", 204, "gemini", "/v1beta/openai/chat/completions"},
		{"/openai/v1/models?key=test", "", "Bearer test", 204, "openai", "/v1/models?key=test"},
	} {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			req.Host = "gateway.example"
			req.Header.Set("X-Goog-Api-Key", test.googleKey)
			req.Header.Set("Authorization", test.bearer)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != test.status || rec.Header().Get("Provider") != test.provider || rec.Header().Get("Forwarded-URI") != test.uri {
				t.Fatalf("status=%d provider=%q URI=%q", rec.Code, rec.Header().Get("Provider"), rec.Header().Get("Forwarded-URI"))
			}
		})
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
			handler := NewRouter(gemini, nil, responseRecorderProvider{testResponsesProvider: provider, called: &providerCalled}, "", 64<<20, "")
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
		return provider, &geminiCalls, NewRouter(gemini, nil, provider, key, 64<<20, "")
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
		handler := NewRouter(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil, provider, "", 64<<20, "")
		if recorder := do(handler, http.MethodGet, OpenCodeModelsPath, "", nil); recorder.Code != 200 {
			t.Fatalf("empty auth key blocked access: %d %s", recorder.Code, recorder.Body)
		}
	})
}
