package logging

import (
	"bufio"
	"net"
	"net/http"
	"regexp"
	"time"
)

var keyRegex = regexp.MustCompile(`(?i)(key|api_key)=([^&]+)`)

// Middleware logs requests and redacts sensitive keys.
func (logger *Logger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if logger.level > LevelInfo {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		ww := &responseWrapper{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(ww, r)

		safeURL := keyRegex.ReplaceAllString(r.URL.String(), "$1=[REDACTED]")
		logger.Infof("[%d] %s %s | %s | %v", ww.statusCode, r.Method, safeURL, r.RemoteAddr, time.Since(start))
	})
}

type responseWrapper struct {
	http.ResponseWriter
	statusCode int
}

// Preserve the underlying connection for ReverseProxy's WebSocket upgrade.
func (rw *responseWrapper) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (rw *responseWrapper) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(rw.ResponseWriter).Hijack()
	if err == nil {
		rw.statusCode = http.StatusSwitchingProtocols
	}
	return conn, buffered, err
}

func (rw *responseWrapper) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWrapper) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
