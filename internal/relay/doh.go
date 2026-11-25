package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	dnsMediaType = "application/dns-message"
	maxDNSBytes  = 65535
	maxAliases   = 8
)

type dohResolver struct {
	endpoint string
	client   *http.Client
}

func newDoHResolver(cfg config.DoH) *dohResolver {
	dialer := net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 3 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 2,
		DialContext:         dialer.DialContext,
	}
	if len(cfg.BootstrapIPs) != 0 {
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			for _, ip := range cfg.BootstrapIPs {
				var conn net.Conn
				conn, err = dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, fmt.Errorf("dial DoH bootstrap addresses: %w", err)
		}
	}
	return &dohResolver{
		endpoint: cfg.URL,
		client: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (resolver *dohResolver) lookup(ctx context.Context, network, host string) ([]netip.Addr, error) {
	name, err := dnsmessage.NewName(strings.TrimSuffix(strings.ToLower(host), ".") + ".")
	if err != nil {
		return nil, err
	}
	switch network {
	case "ip4":
		return resolver.lookupType(ctx, name, dnsmessage.TypeA)
	case "ip6":
		return resolver.lookupType(ctx, name, dnsmessage.TypeAAAA)
	case "ip":
		type result struct {
			ips []netip.Addr
			err error
		}
		pending := make(chan result, 1)
		go func() {
			ips, err := resolver.lookupType(ctx, name, dnsmessage.TypeA)
			pending <- result{ips, err}
		}()
		ipv6, ipv6Err := resolver.lookupType(ctx, name, dnsmessage.TypeAAAA)
		ipv4 := <-pending
		ips := append(ipv4.ips, ipv6...)
		if len(ips) != 0 {
			return ips, nil
		}
		return nil, errors.Join(ipv4.err, ipv6Err)
	default:
		return nil, fmt.Errorf("unsupported DoH network %q", network)
	}
}

func (resolver *dohResolver) lookupType(ctx context.Context, name dnsmessage.Name, recordType dnsmessage.Type) ([]netip.Addr, error) {
	visited := make(map[string]bool, maxAliases)
	var answers []dnsmessage.Resource
	for range maxAliases + 1 {
		key := strings.ToLower(name.String())
		if visited[key] {
			return nil, errors.New("DoH returned a CNAME loop")
		}
		visited[key] = true
		ips, alias := addressAnswers(answers, name, recordType)
		if len(ips) == 0 && alias.Length == 0 {
			var err error
			answers, err = resolver.query(ctx, dnsmessage.Question{Name: name, Type: recordType, Class: dnsmessage.ClassINET})
			if err != nil {
				return nil, err
			}
			ips, alias = addressAnswers(answers, name, recordType)
		}
		if len(ips) != 0 || alias.Length == 0 {
			return ips, nil
		}
		name = alias
	}
	return nil, errors.New("DoH CNAME chain is too long")
}

func addressAnswers(answers []dnsmessage.Resource, name dnsmessage.Name, recordType dnsmessage.Type) ([]netip.Addr, dnsmessage.Name) {
	var ips []netip.Addr
	var alias dnsmessage.Name
	for _, answer := range answers {
		if answer.Header.Class != dnsmessage.ClassINET || !strings.EqualFold(answer.Header.Name.String(), name.String()) {
			continue
		}
		switch record := answer.Body.(type) {
		case *dnsmessage.AResource:
			if recordType == dnsmessage.TypeA {
				ips = append(ips, netip.AddrFrom4(record.A))
			}
		case *dnsmessage.AAAAResource:
			if recordType == dnsmessage.TypeAAAA {
				ips = append(ips, netip.AddrFrom16(record.AAAA))
			}
		case *dnsmessage.CNAMEResource:
			alias = record.CNAME
		}
	}
	return ips, alias
}

func (resolver *dohResolver) query(ctx context.Context, question dnsmessage.Question) ([]dnsmessage.Resource, error) {
	query := dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{question},
	}
	wire, err := query.Pack()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, resolver.endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", dnsMediaType)
	request.Header.Set("Accept", dnsMediaType)
	response, err := resolver.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("DoH request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH returned HTTP %d", response.StatusCode)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != dnsMediaType {
		return nil, errors.New("unexpected DoH response content type")
	}
	wire, err = io.ReadAll(io.LimitReader(response.Body, maxDNSBytes+1))
	if err != nil {
		return nil, err
	}
	var message dnsmessage.Message
	if len(wire) > maxDNSBytes || message.Unpack(wire) != nil || !message.Response || message.ID != query.ID ||
		message.OpCode != 0 || message.Truncated || len(message.Questions) != 1 {
		return nil, errors.New("invalid DoH DNS response")
	}
	reply := message.Questions[0]
	if reply.Type != question.Type || reply.Class != question.Class || !strings.EqualFold(reply.Name.String(), question.Name.String()) {
		return nil, errors.New("DoH response question does not match the request")
	}
	if message.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("DoH DNS lookup failed: %s", message.RCode)
	}
	return message.Answers, nil
}
