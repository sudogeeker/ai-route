package gemini

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/logging"
)

const livePath = "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

func TestGeminiPathAllowlist(t *testing.T) {
	for _, path := range []string{livePath, strings.Replace(livePath, "v1beta", "v1alpha", 1), "/v1/models", "/v1beta/models/test:generateContent", "/v1alpha/models", "/upload/v1beta/files", "/upload/v1beta/files/session", "/opencode/v1", "/opencode/v1/models", "/opencode/v1/responses"} {
		r := httptest.NewRecorder()
		Allowlist(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 204 {
			t.Errorf("allowed path %q returned %d", path, r.Code)
		}
	}
	for _, path := range []string{"/", "/ws/anything", livePath + "/extra", livePath + "Constrained", "/v1beta-other", "/admin", "/upload/v1beta/files-other", "/upload/admin"} {
		r := httptest.NewRecorder()
		Allowlist(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("blocked request reached upstream") })).ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 404 {
			t.Errorf("blocked path %q returned %d", path, r.Code)
		}
	}
}

// Exercise real TCP upgrades through the production allowlist, logger and proxy,
// with a TLS upstream advertising HTTP/2. No Google API key or IPv6 is needed.
func TestLiveWebSocketProxy(t *testing.T) {
	for _, level := range []int{logging.LevelDebug, logging.LevelInfo, logging.LevelWarn, logging.LevelError} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			var logs bytes.Buffer
			logger := logging.New(level, &logs)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != livePath || r.URL.RawQuery != "key=synthetic%2Fkey%2Bvalue" || r.Header.Get("Upgrade") != "websocket" || r.ProtoMajor != 1 || r.Host == "gapi.example.com" {
					t.Errorf("unexpected upstream request: %s %s host=%s", r.Proto, r.URL, r.Host)
					w.WriteHeader(400)
					return
				}
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				hash := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(hash[:]))
				if err := rw.Flush(); err != nil {
					t.Error(err)
					return
				}
				for i := 0; i < 2; i++ {
					opcode, payload, err := readTestFrame(rw)
					if err != nil {
						t.Error(err)
						return
					}
					if err := writeTestFrame(rw, opcode, payload, false); err != nil {
						t.Error(err)
						return
					}
				}
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			transport := New(config.Gemini{}, nil, logger).newTransport()
			// Keep the production HTTP/2 settings; replace only the IPv6 dialer
			// and trust settings so this test uses the local TLS server.
			transport.Proxy = nil
			transport.DialContext = (&net.Dialer{Timeout: time.Second}).DialContext
			transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			defer transport.CloseIdleConnections()
			done := make(chan struct{})
			handler := Allowlist(logger.Middleware(newReverseProxy(target, transport, logger)))
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r)
				close(done)
			}))
			defer proxy.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			conn, err := net.DialTimeout("tcp", proxyURL.Host, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprintf(conn, "GET %s?key=synthetic%%2Fkey%%2Bvalue HTTP/1.1\r\nHost: gapi.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", livePath)
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 101 {
				t.Fatalf("upgrade returned %s", resp.Status)
			}
			if resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
				t.Fatal("handshake accept header lost")
			}
			for _, frame := range []struct {
				opcode  byte
				payload []byte
			}{{1, []byte(`{"setup":{"model":"models/test"}}`)}, {2, []byte{0, 1, 127, 128, 255}}} {
				if err := writeTestFrame(conn, frame.opcode, frame.payload, true); err != nil {
					t.Fatal(err)
				}
				opcode, payload, err := readTestFrame(reader)
				if err != nil {
					t.Fatal(err)
				}
				if opcode != frame.opcode || !bytes.Equal(payload, frame.payload) {
					t.Fatalf("frame changed: %d %x", opcode, payload)
				}
			}
			conn.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("proxy did not finish after socket close")
			}
			if level <= logging.LevelInfo {
				if !strings.Contains(logs.String(), "[101]") || !strings.Contains(logs.String(), "[REDACTED]") || strings.Contains(logs.String(), "synthetic") {
					t.Fatalf("unexpected upgrade log: %s", &logs)
				}
			}
		})
	}
}

// Short valid RFC 6455 frames, enough to verify text/binary and mask preservation.
func writeTestFrame(w io.Writer, opcode byte, payload []byte, masked bool) error {
	frame := []byte{0x80 | opcode, byte(len(payload))}
	data := append([]byte(nil), payload...)
	if masked {
		frame[1] |= 0x80
		mask := []byte{1, 2, 3, 4}
		frame = append(frame, mask...)
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	_, err := w.Write(append(frame, data...))
	if f, ok := w.(interface{ Flush() error }); ok && err == nil {
		err = f.Flush()
	}
	return err
}

func readTestFrame(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := int(header[1] & 0x7f)
	if size >= 126 {
		return 0, nil, fmt.Errorf("test frame too large")
	}
	var mask [4]byte
	if header[1]&0x80 != 0 {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 != 0 {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return header[0] & 0xf, payload, nil
}
