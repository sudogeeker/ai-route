package gemini

import (
	"net/http"
	"strings"
)

func isAllowedPath(path string) bool {
	// Common Generative Language API prefixes
	// Examples:
	// - /v1/models:generateContent
	// - /v1beta/models/gemini-2.5-flash:streamGenerateContent
	// - /v1alpha/...
	switch path {
	case "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
		"/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent":
		return true
	case "/v1", "/v1beta", "/v1alpha":
		return true
	case "/upload/v1beta/files":
		return true
	case "/opencode/v1":
		return true
	default:
		return strings.HasPrefix(path, "/upload/v1beta/files/") ||
			strings.HasPrefix(path, "/opencode/v1/") ||
			strings.HasPrefix(path, "/v1/") ||
			strings.HasPrefix(path, "/v1beta/") ||
			strings.HasPrefix(path, "/v1alpha/")
	}
}

func Allowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
