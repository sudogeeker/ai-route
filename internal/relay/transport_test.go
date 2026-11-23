package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/logging"
	"aizasy.pages.dev/ai-route/internal/server"
)

func TestOpenAIAudioThroughRelay(t *testing.T) {
	var body bytes.Buffer
	upload := multipart.NewWriter(&body)
	if err := upload.WriteField("model", "any-audio-model"); err != nil {
		t.Fatal(err)
	}
	audio := []byte{0, 1, 127, 128, 255}
	file, err := upload.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Method != http.MethodPost ||
			r.Header.Get("Content-Type") != upload.FormDataContentType() ||
			r.Header.Get("Authorization") != "Bearer audio-key" {
			t.Errorf("unexpected audio request: %s %s %v", r.Method, r.URL, r.Header)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body.Bytes()) {
			t.Errorf("multipart audio changed: %v", err)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(audio)
	}))
	defer upstream.Close()
	logger := logging.New(logging.LevelError, io.Discard)
	defer logger.Close()
	proxy := New(config.Relay{TargetHost: upstream.Listener.Addr().String(), IPFamily: "ipv4"}, nil, logger)
	defer proxy.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	proxy.transport.TLSClientConfig.RootCAs = roots
	handler := server.Allowlist(server.NewRouter(nil, proxy.Handler(), nil, "", 1, ""))
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body.Bytes()))
	request.Header.Set("Authorization", "Bearer audio-key")
	request.Header.Set("Content-Type", upload.FormDataContentType())
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	if result.Code != http.StatusOK || result.Header().Get("Content-Type") != "audio/mpeg" || !bytes.Equal(result.Body.Bytes(), audio) {
		t.Fatalf("audio response changed: %d, %s, %q", result.Code, result.Header().Get("Content-Type"), result.Body.Bytes())
	}
}

func TestRelayRefreshAndRotateDestinations(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := logging.New(logging.LevelError, io.Discard)
	defer logger.Close()
	target := net.JoinHostPort("relay.example", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
	proxy := New(config.Relay{TargetHost: target, IPFamily: "ipv4"}, nil, logger)
	defer proxy.Close()
	first := netip.MustParseAddr("127.0.0.1")
	second := netip.MustParseAddr("127.0.0.2")
	lookups := 0
	proxy.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip4" || host != "relay.example" {
			t.Errorf("unexpected lookup: %s %s", network, host)
		}
		lookups++
		if lookups == 2 {
			return nil, errors.New("temporary DNS failure")
		}
		return []netip.Addr{first, second, first, netip.MustParseAddr("::1")}, nil
	}
	if err := proxy.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := proxy.refresh(t.Context()); err == nil {
		t.Fatal("failed lookup reported success")
	}
	if got := proxy.snapshot(); !reflect.DeepEqual(got, []netip.Addr{first, second}) {
		t.Fatalf("unexpected cache after failed refresh: %v", got)
	}
	for _, want := range []netip.Addr{first, second, first} {
		conn, err := proxy.dialContext(t.Context(), "tcp", target)
		if err != nil {
			t.Fatal(err)
		}
		got := conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr()
		conn.Close()
		if got != want {
			t.Fatalf("dialed %s, want %s", got, want)
		}
	}
}

func TestOpenAIStreamingThroughRelay(t *testing.T) {
	const requestBody = ` { "model": "big-pickle", "stream": true } `
	const responseEvent = "data: {\"type\":\"response.created\"}\n\n"
	continueStream := make(chan struct{})
	var requestCount atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != requestBody || r.Method != http.MethodPost ||
			r.URL.Path != server.ResponsesPath || r.URL.RawQuery != "include=output%2Ftext" ||
			r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("X-Forwarded-For") != "" ||
			r.TLS.ServerName != "example.com" || !strings.HasPrefix(r.Host, "example.com:") {
			t.Errorf("unexpected upstream request: %v %s %s %s %q", err, r.Method, r.Host, r.URL, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, responseEvent)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		select {
		case <-continueStream:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	logger := logging.New(logging.LevelError, io.Discard)
	defer logger.Close()
	port := fmt.Sprint(upstream.Listener.Addr().(*net.TCPAddr).Port)
	proxy := New(config.Relay{
		TargetHost: net.JoinHostPort("example.com", port),
		IPFamily:   "dual",
		DestinationIPs: []netip.Addr{
			netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1"),
		},
	}, nil, logger)
	defer proxy.Close()
	var ipv6Dials atomic.Int32
	proxy.source = func() net.IP {
		ipv6Dials.Add(1)
		return net.ParseIP("2001:db8::1234")
	}
	proxy.transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	proxy.transport.TLSClientConfig.RootCAs.AddCert(upstream.Certificate())

	fallback := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("OpenAI request reached Gemini") })
	gateway := httptest.NewServer(server.Allowlist(server.NewRouter(fallback, proxy.Handler(), nil, "", 1, "openai.gateway.example")))
	defer gateway.Close()
	defer close(continueStream)
	client := gateway.Client()
	client.Timeout = 5 * time.Second
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gateway.URL+"/v1/responses?include=output%2Ftext", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Host = "openai.gateway.example"
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	event, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || resp.StatusCode != http.StatusAccepted || event+"\n" != responseEvent {
		t.Fatalf("unexpected SSE response: %d, %q, %v", resp.StatusCode, event, err)
	}
	if ipv6Dials.Load() != 1 || requestCount.Load() != 1 {
		t.Fatalf("expected IPv6 fallback to deliver exactly one request: IPv6=%d requests=%d", ipv6Dials.Load(), requestCount.Load())
	}
}
