package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func CopyHeaders(dst, src http.Header) {
	blocked := map[string]bool{"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Content-Length": true}
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			blocked[http.CanonicalHeaderKey(strings.TrimSpace(token))] = true
		}
	}
	for name, values := range src {
		if blocked[http.CanonicalHeaderKey(name)] {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

type FlushWriter struct{ http.ResponseWriter }

func (w FlushWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err == nil {
		flushErr := http.NewResponseController(w.ResponseWriter).Flush()
		if flushErr != nil && !errors.Is(flushErr, http.ErrNotSupported) {
			err = flushErr
		}
	}
	return n, err
}

func WriteError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": kind, "message": message}})
}
