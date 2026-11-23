package relay

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

func (p *Proxy) newTransport() *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: p.target.Hostname(),
		},
		DialContext: p.dialContext,
	}
}

func (p *Proxy) dialContext(parent context.Context, _, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !strings.EqualFold(host, p.target.Hostname()) {
		return nil, fmt.Errorf("unexpected relay dial address %q", addr)
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	destinations := p.snapshot()
	if len(destinations) == 0 && !p.static {
		if err := p.refresh(ctx); err != nil {
			return nil, err
		}
		destinations = p.snapshot()
	}
	if len(destinations) == 0 {
		return nil, fmt.Errorf("no relay addresses for %q", host)
	}
	dialer := net.Dialer{KeepAlive: 30 * time.Second}
	if len(destinations) > 1 {
		deadline, _ := ctx.Deadline()
		dialer.Timeout = min(3*time.Second, time.Until(deadline)/time.Duration(len(destinations)))
	}
	start := (p.next.Add(1) - 1) % uint64(len(destinations))
	for offset := range destinations {
		ip := destinations[(int(start)+offset)%len(destinations)]
		network := "tcp4"
		dialer.LocalAddr = nil
		if ip.Is6() {
			network = "tcp6"
			if source := p.source(); source != nil {
				dialer.LocalAddr = &net.TCPAddr{IP: source}
			}
		}
		destination := net.JoinHostPort(ip.String(), port)
		p.logger.Debugf("Relay dial %s: %v -> %s", host, dialer.LocalAddr, destination)
		var conn net.Conn
		conn, err = dialer.DialContext(ctx, network, destination)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("dial %s on %d address(es): %w", host, len(destinations), err)
}
