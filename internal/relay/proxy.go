package relay

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/ipv6"
	"aizasy.pages.dev/ai-route/internal/logging"
)

type Proxy struct {
	cfg          config.Relay
	target       *url.URL
	logger       *logging.Logger
	source       func() net.IP
	lookup       func(context.Context, string, string) ([]netip.Addr, error)
	doh          *dohResolver
	static       bool
	mu           sync.RWMutex
	destinations []netip.Addr
	next         atomic.Uint64
	transport    *http.Transport
	handler      *httputil.ReverseProxy
}

func New(cfg config.Relay, sources *ipv6.SourcePool, logger *logging.Logger) *Proxy {
	target := &url.URL{Scheme: "https", Host: cfg.TargetHost}
	p := &Proxy{
		cfg: cfg, target: target, logger: logger,
		source: sources.Random, lookup: net.DefaultResolver.LookupNetIP,
	}
	if len(cfg.DestinationIPs) > 0 {
		p.static = true
		p.destinations = p.filterIPs(cfg.DestinationIPs)
	} else if ip, err := netip.ParseAddr(target.Hostname()); err == nil {
		p.static = true
		p.destinations = p.filterIPs([]netip.Addr{ip})
	} else if cfg.DoH.URL != "" {
		p.doh = newDoHResolver(cfg.DoH)
		p.lookup = p.doh.lookup
	}
	p.transport = p.newTransport()
	p.handler = newReverseProxy(target, p.transport, logger)
	return p
}

func (p *Proxy) Handler() http.Handler { return p.handler }
func (p *Proxy) Close() {
	p.transport.CloseIdleConnections()
	if p.doh != nil {
		p.doh.client.CloseIdleConnections()
	}
}

func newReverseProxy(targetURL *url.URL, transport http.RoundTripper, logger *logging.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.Out.URL.Scheme = targetURL.Scheme
			req.Out.URL.Host = targetURL.Host
			req.Out.URL.RawQuery = req.In.URL.RawQuery
			req.Out.Host = targetURL.Host
			if _, ok := req.Out.Header["User-Agent"]; !ok {
				req.Out.Header.Set("User-Agent", "")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				logger.Errorf("Proxy error for %s: %v", targetURL.Host, err)
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}
}
