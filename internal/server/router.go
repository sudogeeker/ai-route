package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"aizasy.pages.dev/ai-route/internal/httpx"
)

const (
	ChatPath              = "/v1/chat/completions"
	ResponsesPath         = "/v1/responses"
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

// All other paths and models retain the existing Gemini handler and request bytes.
func NewRouter(gemini http.Handler, opencode ChatProvider, openCodeAuthKey string, maxBodyBytes int64) http.Handler {
	responsesProvider, _ := opencode.(ResponsesProvider)
	modelProvider, _ := opencode.(ModelProvider)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, OpenCodePrefix) {
			serveOpenCode(w, r, opencode, responsesProvider, modelProvider, openCodeAuthKey, maxBodyBytes)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == ResponsesPath {
			body, ok := readBody(w, r, maxBodyBytes, "Cannot read Responses request within the configured body limit")
			if !ok {
				return
			}
			model, err := bodyModel(body)
			if err != nil {
				httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			if opencode.SupportsModel(model) {
				if !requireOpenCodeAuth(w, r, openCodeAuthKey) {
					return
				}
				if responsesProvider == nil {
					httpx.WriteError(w, http.StatusNotImplemented, "invalid_request_error", "This provider does not support the Responses API")
					return
				}
				responsesProvider.ServeResponses(w, r, body)
				return
			}
			if unsupportedFreeModel(model) {
				httpx.WriteError(w, http.StatusBadRequest, "model_not_supported", "This free model is not in OPENCODE_MODELS")
				return
			}
			gemini.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != ChatPath {
			gemini.ServeHTTP(w, r)
			return
		}
		body, ok := readBody(w, r, maxBodyBytes, "Cannot read chat request within the configured body limit")
		if !ok {
			return
		}
		model, err := bodyModel(body)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if !opencode.SupportsModel(model) {
			if unsupportedFreeModel(model) {
				httpx.WriteError(w, http.StatusBadRequest, "model_not_supported", "This free model is not in OPENCODE_MODELS")
				return
			}
			gemini.ServeHTTP(w, r)
			return
		}
		if !requireOpenCodeAuth(w, r, openCodeAuthKey) {
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		opencode.ServeChat(w, r, body, request.Stream)
	})
}

func serveOpenCode(w http.ResponseWriter, r *http.Request, provider ChatProvider, responsesProvider ResponsesProvider, modelProvider ModelProvider, authKey string, maxBodyBytes int64) {
	if !requireOpenCodeAuth(w, r, authKey) {
		return
	}
	switch {
	case r.URL.Path == OpenCodeModelsPath || strings.HasPrefix(r.URL.Path, OpenCodeModelsPath+"/"):
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
		body, ok := readBody(w, r, maxBodyBytes, "Cannot read Responses request within the configured body limit")
		if !ok {
			return
		}
		model, err := bodyModel(body)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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

func bodyModel(body []byte) (string, error) {
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &request); err != nil || request.Model == "" {
		return "", errors.New("A valid JSON body with a model ID is required")
	}
	return request.Model, nil
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

func readBody(w http.ResponseWriter, r *http.Request, maxBodyBytes int64, message string) ([]byte, bool) {
	originalBody := r.Body
	body, err := io.ReadAll(http.MaxBytesReader(w, originalBody, maxBodyBytes))
	originalBody.Close()
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		httpx.WriteError(w, status, "invalid_request_error", message)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}
