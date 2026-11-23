package server

import (
	"net/http"
	"strings"
)

func isAllowedPath(path string) bool {
	switch path {
	case "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
		"/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent":
		return true
	}
	return hasPathPrefix(path, "/upload/v1beta/files") ||
		hasPathPrefix(path, OpenCodePrefix) ||
		hasPathPrefix(path, OpenAIPrefix) ||
		hasPathPrefix(path, geminiV1Prefix) ||
		hasPathPrefix(path, "/v1") ||
		hasPathPrefix(path, "/v1beta") ||
		hasPathPrefix(path, "/v1alpha")
}

func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
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
