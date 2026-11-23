package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"aizasy.pages.dev/ai-route/internal/httpx"
)

const (
	ChatPath              = "/v1/chat/completions"
	ResponsesPath         = "/v1/responses"
	openAIBasePath        = "/openai"
	OpenAIPrefix          = openAIBasePath + "/v1"
	geminiBasePath        = "/gemini"
	geminiV1Prefix        = geminiBasePath + "/v1"
	OpenCodePrefix        = "/opencode/v1"
	OpenCodeModelsPath    = OpenCodePrefix + "/models"
	OpenCodeResponsesPath = OpenCodePrefix + "/responses"
)

// ChatProvider separates HTTP routing from provider transport and racing.
type ChatProvider interface {
	SupportsModel(string) bool
	ServeChat(http.ResponseWriter, *http.Request, []byte, bool)
}

// ResponsesProvider is implemented by providers that can translate the
// stateless Responses API. Providers without it leave those routes untouched.
type ResponsesProvider interface {
	ServeResponses(http.ResponseWriter, *http.Request, []byte)
}

// ModelProvider exposes the model IDs advertised by a provider.
type ModelProvider interface {
	Models() []string
}

func NewRouter(gemini, openai http.Handler, opencode ChatProvider, openCodeAuthKey string, maxBodyBytes int64, openAIHost string) http.Handler {
	sharedHost := openAIHost == "" && gemini != nil && openai != nil
	if gemini == nil {
		gemini = http.NotFoundHandler()
	}
	geminiV1 := http.StripPrefix(geminiBasePath, gemini)
	openAIAlias := http.NotFoundHandler()
	if openai != nil {
		openAIAlias = http.StripPrefix(openAIBasePath, openai)
	}
	responsesProvider, _ := opencode.(ResponsesProvider)
	modelProvider, _ := opencode.(ModelProvider)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case hasPathPrefix(path, OpenAIPrefix):
			openAIAlias.ServeHTTP(w, r)
			return
		case openai != nil && openAIHost != "" && matchHost(r.Host, openAIHost):
			if hasPathPrefix(path, "/v1") {
				openai.ServeHTTP(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		case hasPathPrefix(path, geminiV1Prefix):
			geminiV1.ServeHTTP(w, r)
			return
		case openai != nil && openAIHost == "" && hasPathPrefix(path, "/v1"):
			if sharedHost && (r.Header.Get("X-Goog-Api-Key") != "" || r.URL.Query().Get("key") != "") {
				if r.Header.Get("Authorization") != "" {
					httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "Ambiguous provider credentials. Use /gemini/v1 or /openai/v1 to select the upstream explicitly.")
					return
				}
				gemini.ServeHTTP(w, r)
				return
			}
			openai.ServeHTTP(w, r)
			return
		case hasPathPrefix(path, OpenCodePrefix):
			if opencode == nil {
				http.NotFound(w, r)
				return
			}
			serveOpenCode(w, r, opencode, responsesProvider, modelProvider, openCodeAuthKey, maxBodyBytes)
			return
		}
		if opencode == nil || r.Method != http.MethodPost || (path != ChatPath && path != ResponsesPath) {
			gemini.ServeHTTP(w, r)
			return
		}
		body, model, ok := readModelRequest(w, r, maxBodyBytes, "Cannot read request within the configured body limit")
		if !ok {
			return
		}
		if !opencode.SupportsModel(model) {
			if unsupportedFreeModel(model) {
				httpx.WriteError(w, http.StatusBadRequest, "model_not_supported", "This free model is not in opencode.models")
				return
			}
			gemini.ServeHTTP(w, r)
			return
		}
		if !requireOpenCodeAuth(w, r, openCodeAuthKey) {
			return
		}
		if path == ResponsesPath {
			if responsesProvider == nil {
				httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error", "This provider does not support the Responses API")
				return
			}
			responsesProvider.ServeResponses(w, r, body)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		opencode.ServeChat(w, r, body, request.Stream)
	})
}

func matchHost(authority, host string) bool {
	if name, _, err := net.SplitHostPort(authority); err == nil {
		authority = name
	}
	return strings.EqualFold(strings.TrimSuffix(authority, "."), host)
}

func serveOpenCode(w http.ResponseWriter, r *http.Request, provider ChatProvider, responsesProvider ResponsesProvider, modelProvider ModelProvider, authKey string, maxBodyBytes int64) {
	if !requireOpenCodeAuth(w, r, authKey) {
		return
	}
	switch {
	case hasPathPrefix(r.URL.Path, OpenCodeModelsPath):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httpx.WriteError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only GET is supported for the model list")
			return
		}
		if modelProvider == nil {
			httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error", "This provider does not expose a model list")
			return
		}
		writeModels(w, modelProvider.Models(), strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, OpenCodeModelsPath), "/"))
	case r.URL.Path == OpenCodeResponsesPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			httpx.WriteError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Only POST is supported for responses")
			return
		}
		if responsesProvider == nil {
			httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error", "This provider does not support the Responses API")
			return
		}
		body, model, ok := readModelRequest(w, r, maxBodyBytes, "Cannot read Responses request within the configured body limit")
		if !ok {
			return
		}
		if !provider.SupportsModel(model) {
			kind := "invalid_request_error"
			if unsupportedFreeModel(model) {
				kind = "model_not_supported"
			}
			httpx.WriteError(w, http.StatusBadRequest, kind, "The requested model is not available on /opencode/v1")
			return
		}
		responsesProvider.ServeResponses(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

func writeModels(w http.ResponseWriter, models []string, requested string) {
	if requested != "" {
		for _, model := range models {
			if model == requested {
				writeJSON(w, http.StatusOK, modelObject(model))
				return
			}
		}
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "Model not found")
		return
	}
	data := make([]any, 0, len(models))
	for _, model := range models {
		data = append(data, modelObject(model))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func modelObject(model string) map[string]any {
	return map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "opencode"}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readModelRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64, message string) ([]byte, string, bool) {
	body, ok := readBodyWithin(w, r, maxBodyBytes, message, 30*time.Second)
	if !ok {
		return nil, "", false
	}
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &request); err != nil || request.Model == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "A valid JSON body with a model ID is required")
		return nil, "", false
	}
	return body, request.Model, true
}

func unsupportedFreeModel(model string) bool {
	return strings.HasSuffix(model, "-free") || model == "big-pickle"
}

// requireOpenCodeAuth protects every OpenCode entry point, including the
// compatibility routes that dispatch by model. An empty key preserves the
// previous unauthenticated behavior for existing deployments.
func requireOpenCodeAuth(w http.ResponseWriter, r *http.Request, key string) bool {
	if key == "" || openCodeAuthorized(r, key) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="opencode"`)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteError(w, http.StatusUnauthorized, "authentication_error", "A valid OpenCode auth key is required")
	return false
}

func openCodeAuthorized(r *http.Request, key string) bool {
	var candidates []string
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authorization) >= len("Bearer ") && strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
		candidates = append(candidates, strings.TrimSpace(authorization[len("Bearer "):]))
	} else if authorization != "" {
		candidates = append(candidates, authorization)
	}
	for _, name := range []string{"x-api-key", "api-key"} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			candidates = append(candidates, value)
		}
	}
	for _, candidate := range candidates {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func readBodyWithin(w http.ResponseWriter, r *http.Request, maxBodyBytes int64, message string, timeout time.Duration) ([]byte, bool) {
	// Limit JSON uploads before the provider's race timer starts. Clear this
	// deadline afterwards so long SSE responses and keep-alive requests work.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(timeout))
	defer controller.SetReadDeadline(time.Time{})
	originalBody := r.Body
	body, err := io.ReadAll(http.MaxBytesReader(w, originalBody, maxBodyBytes))
	originalBody.Close()
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		var timeoutErr net.Error
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		} else if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
			status = http.StatusRequestTimeout
			message = "Timed out reading the request body"
		}
		httpx.WriteError(w, status, "invalid_request_error", message)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}
