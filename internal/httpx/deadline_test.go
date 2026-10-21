package httpx

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

type pipeWriter struct {
	conn   net.Conn
	header http.Header
}

func (w *pipeWriter) Header() http.Header            { return w.header }
func (w *pipeWriter) WriteHeader(int)                {}
func (w *pipeWriter) Write(data []byte) (int, error) { return w.conn.Write(data) }
func (w *pipeWriter) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}

func TestClientThatStopsReadingCannotBlockWriteForever(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	w := DeadlineWriter{ResponseWriter: &pipeWriter{conn: server, header: make(http.Header)}, Timeout: 50 * time.Millisecond}
	done := make(chan error, 1)
	go func() { _, err := w.Write([]byte("output")); done <- err }()
	select {
	case err := <-done:
		if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
			t.Fatalf("expected a write timeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a non-reading client left the write blocked")
	}
}

func TestWriteDeadlineRenewsForHealthyLongStream(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	w := DeadlineWriter{ResponseWriter: &pipeWriter{conn: server, header: make(http.Header)}, Timeout: 100 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 4; i++ {
			if _, err := w.Write([]byte("x")); err != nil {
				done <- err
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		done <- nil
	}()
	client.SetReadDeadline(time.Now().Add(time.Second))
	data := make([]byte, 4)
	if _, err := io.ReadFull(client, data); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
