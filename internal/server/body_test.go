package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIncompleteRequestBodyTimesOutBeforeDispatch(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if _, ok := readBodyWithin(w, r, 1024, "bad body", 75*time.Millisecond); ok {
			t.Error("incomplete request was dispatched")
		}
	}))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: local\r\nContent-Length: 1000\r\n\r\n{\"model\":")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("got %s, want 408", resp.Status)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("body timeout left the handler blocked")
	}
}

func TestBodyReadDeadlineClearedForNextKeepAliveRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := readBodyWithin(w, r, 1024, "bad body", 50*time.Millisecond); ok {
			w.Write(body)
		}
	}))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	for i := 0; i < 2; i++ {
		fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: local\r\nContent-Length: 2\r\n\r\n{}")
		resp, err := http.ReadResponse(reader, &http.Request{Method: "POST"})
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "{}" {
			t.Fatalf("keep-alive request %d failed: status=%d body=%q err=%v", i, resp.StatusCode, body, err)
		}
		if i == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
