package gemini

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/ipv6"
	"aizasy.pages.dev/ai-route/internal/logging"
)

// Proxy owns Gemini configuration, DNS state and its reusable connection pool.
type Proxy struct {
	cfg          config.Gemini
	logger       *logging.Logger
	source       func() net.IP
	mu           sync.RWMutex
	destinations []string
	transport    *http.Transport
	handler      *httputil.ReverseProxy
}

func New(cfg config.Gemini, sources *ipv6.SourcePool, logger *logging.Logger) *Proxy {
	source := func() net.IP { return nil }
	if sources != nil {
		source = sources.Random
	}
	p := &Proxy{cfg: cfg, logger: logger, source: source}
	p.transport = p.newTransport()
	p.handler = newReverseProxy(&url.URL{Scheme: "https", Host: cfg.TargetHost}, p.transport, logger)
	return p
}

func (p *Proxy) Handler() http.Handler { return p.handler }
func (p *Proxy) Close()                { p.transport.CloseIdleConnections() }

func newReverseProxy(targetURL *url.URL, transport http.RoundTripper, logger *logging.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1, // Disable buffering for streaming support
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host
			req.Header.Del("X-Forwarded-For")
			if _, ok := req.Header["User-Agent"]; !ok {
				req.Header.Set("User-Agent", "")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				logger.Errorf("Proxy error: %v", err)
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}
}
