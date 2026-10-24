package opencode

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

func TestOpenCodeDirectTransportWithoutSource(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, openCodeOK)
	}))
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	transport := p.newTransport(nil)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	resp, err := client.Post(server.URL+"/chat/completions", "application/json", strings.NewReader(`{"model":"big-pickle"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != openCodeOK {
		t.Fatalf("direct fallback failed: %s", body)
	}
}

func TestOpenCodeProxyRotation(t *testing.T) {
	first, _ := url.Parse("http://127.0.0.1:10001")
	second, _ := url.Parse("socks5://127.0.0.1:10002")
	p := &Proxy{cfg: config.OpenCode{ProxyURLs: []*url.URL{first, second}}}
	if got := p.selectProxy(); got.String() != first.String() {
		t.Fatalf("unexpected first proxy: %s", got)
	}
	if got := p.selectProxy(); got.String() != second.String() {
		t.Fatalf("unexpected second proxy: %s", got)
	}
	if got := p.selectProxy(); got.String() != first.String() {
		t.Fatalf("proxy pool did not wrap: %s", got)
	}
}

func TestOpenCodeProductionTransportIPv6(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	// Check host permissions independently, so a production dialer bug cannot cause a skip.
	probeDialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.IPv6loopback}}
	probe, err := probeDialer.Dial("tcp6", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Skipf("Host cannot connect with a bound IPv6 loopback source: %v", err)
	}
	accepted, err := listener.Accept()
	probe.Close()
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	accepted.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != "::1" {
			t.Errorf("upstream did not receive IPv6: %s", r.RemoteAddr)
		}
		if r.ProtoMajor != 1 {
			t.Errorf("OpenCode should use HTTP/1.1 to avoid transport replays: %s", r.Proto)
		}
		io.WriteString(w, openCodeOK)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	transport := p.newTransport(net.IPv6loopback)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	resp, err := client.Post(server.URL+"/chat/completions", "application/json", strings.NewReader(`{"model":"big-pickle"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != openCodeOK || transport.Proxy != nil || !transport.DisableKeepAlives {
		t.Fatal("production IPv6 transport failed")
	}
}

// Exercise production TLS negotiation even when this host blocks IPv6 sockets.
func TestOpenCodeProductionHTTP1AttemptBudget(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.ProtoMajor != 1 {
			t.Errorf("unexpected protocol: %s", r.Proto)
		}
		w.WriteHeader(503)
		io.WriteString(w, "try another source")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	p := testOpenCodeProxy(t, server.URL)
	p.transport = func(source net.IP) *http.Transport {
		transport := p.newTransport(source)
		transport.DialContext = (&net.Dialer{Timeout: time.Second}).DialContext
		transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		return transport
	}
	recorder := httptest.NewRecorder()
	testOpenCodeHandler(p).ServeHTTP(recorder, openCodeTestRequest(`{"model":"big-pickle"}`))
	if attempts.Load() != 10 || recorder.Code != 503 {
		t.Fatalf("attempt budget/protocol failed: %d/%d", attempts.Load(), recorder.Code)
	}
}
