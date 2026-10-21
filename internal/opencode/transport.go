package opencode

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

func (p *Proxy) newTransport(source net.IP) *http.Transport {
	port := p.cfg.BaseURL.Port()
	if port == "" {
		port = "443"
	}
	return &http.Transport{
		// Direct egress is required: an environment proxy would bypass source binding.
		// HTTP/2 may replay REFUSED_STREAM/GOAWAY internally. HTTP/1.1 on a
		// fresh transport keeps actual requests within the configured attempt count.
		TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.cfg.BaseURL.Hostname()},
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			host, requestedPort, err := net.SplitHostPort(address)
			if err != nil || !strings.EqualFold(host, p.cfg.BaseURL.Hostname()) || requestedPort != port {
				return nil, errors.New("unexpected OpenCode upstream address")
			}
			if source.To16() == nil || source.To4() != nil {
				return nil, errors.New("invalid OpenCode source IPv6")
			}
			destination, err := p.dns.pick(ctx)
			if err != nil {
				return nil, err
			}
			dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, LocalAddr: &net.TCPAddr{IP: source}}
			return dialer.DialContext(ctx, "tcp6", net.JoinHostPort(destination.String(), port))
		},
	}
}
