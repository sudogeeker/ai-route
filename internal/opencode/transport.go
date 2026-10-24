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
	port := p.cfg.BaseURL.Port()
	if port == "" {
		port = "443"
	}
	transport := &http.Transport{
		// Environment proxies are ignored so a configured IPv6 source is never
		// bypassed. The explicit proxy pool below still rotates normally.
		// HTTP/2 may replay REFUSED_STREAM/GOAWAY internally. HTTP/1.1 on a
		// fresh transport keeps actual requests within the configured attempt count.
		TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.cfg.BaseURL.Hostname()},
	}
	if proxy := p.selectProxy(); proxy != nil {
		switch strings.ToLower(proxy.Scheme) {
		case "socks5", "socks5h":
			transport.DialContext = p.socks5DialContext(proxy)
		default:
			transport.Proxy = http.ProxyURL(proxy)
		}
		return transport
	}
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		host, requestedPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, p.cfg.BaseURL.Hostname()) || requestedPort != port {
			return nil, errors.New("unexpected OpenCode upstream address")
		}
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		if source == nil {
			// Hosts without a usable IPv6 range still work through their
			// normal IPv4/IPv6 connectivity. A proxy pool provides rotation.
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
	return transport
}

func (p *Proxy) selectProxy() *url.URL {
	if len(p.cfg.ProxyURLs) == 0 {
		return nil
	}
	index := p.proxyIndex.Add(1) - 1
	return p.cfg.ProxyURLs[index%uint64(len(p.cfg.ProxyURLs))]
}
