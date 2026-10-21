package httpx

import (
	"net/http"
	"time"
)

// DeadlineWriter prevents a downstream that stops reading from holding a
// completed upstream response forever. Each write gets a fresh idle budget.
type DeadlineWriter struct {
	http.ResponseWriter
	Timeout time.Duration
}

func (w DeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w DeadlineWriter) deadline() {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.Timeout))
}

func (w DeadlineWriter) Write(data []byte) (int, error) {
	w.deadline()
	return w.ResponseWriter.Write(data)
}

func (w DeadlineWriter) WriteHeader(status int) {
	w.deadline()
	w.ResponseWriter.WriteHeader(status)
}

func (w DeadlineWriter) FlushError() error {
	w.deadline()
	return http.NewResponseController(w.ResponseWriter).Flush()
}
