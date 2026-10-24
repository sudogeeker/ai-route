package opencode

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (p *Proxy) newTransport(source net.IP) *http.Transport {
	transport := p.baseTransport()
	port := p.upstreamPort()
	if proxy := p.selectProxy(); proxy != nil {
		switch strings.ToLower(proxy.Scheme) {
		case "socks5", "socks5h":
			dial := p.socks5DialContext(proxy)
			transport.DialContext = dial
			p.useOpenCodeTLS(transport, dial)
		default:
			transport.Proxy = http.ProxyURL(proxy)
		}
		return transport
	}
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		host, requestedPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, p.cfg.BaseURL.Hostname()) || requestedPort != port {
			return nil, errors.New("unexpected OpenCode upstream address")
		}
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		if source == nil {
			return dialer.DialContext(ctx, "tcp", address)
		}
		if source.To16() == nil || source.To4() != nil {
			return nil, errors.New("invalid OpenCode source IPv6")
		}
		destination, err := p.dns.pick(ctx)
		if err != nil {
			return nil, err
		}
		dialer.LocalAddr = &net.TCPAddr{IP: source}
		return dialer.DialContext(ctx, "tcp6", net.JoinHostPort(destination.String(), port))
	}
	transport.DialContext = dial
	p.useOpenCodeTLS(transport, dial)
	return transport
}

func (p *Proxy) newWARPTransport(egress warpEgress) *http.Transport {
	transport := p.baseTransport()
	port := p.upstreamPort()
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		host, requestedPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, p.cfg.BaseURL.Hostname()) || requestedPort != port {
			return nil, errors.New("unexpected OpenCode upstream address")
		}
		return egress.DialContext(ctx, egress.DialNetwork(), address)
	}
	transport.DialContext = dial
	p.useOpenCodeTLS(transport, dial)
	return transport
}

func (p *Proxy) baseTransport() *http.Transport {
	return &http.Transport{
		// Environment proxies are ignored so a configured source or WARP exit
		// is never bypassed. The explicit proxy pool still rotates normally.
		// The official Bun client negotiates HTTP/1.1 only.
		TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.cfg.BaseURL.Hostname()},
	}
}

func (p *Proxy) upstreamPort() string {
	port := p.cfg.BaseURL.Port()
	if port == "" {
		return "443"
	}
	return port
}

func (p *Proxy) selectProxy() *url.URL {
	if len(p.cfg.ProxyURLs) == 0 {
		return nil
	}
	index := p.proxyIndex.Add(1) - 1
	return p.cfg.ProxyURLs[index%uint64(len(p.cfg.ProxyURLs))]
}
