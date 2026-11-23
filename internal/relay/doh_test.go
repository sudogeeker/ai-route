package relay

import (
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/logging"
	"golang.org/x/net/dns/dnsmessage"
)

func TestIndependentDoHRelays(t *testing.T) {
	var fail atomic.Bool
	var queries atomic.Int32
	resolver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "example.com" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected DoH TLS server name or auth")
		}
		query := readDoHQuery(t, r)
		if query == nil {
			return
		}
		queries.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		question := query.Questions[0]
		var records []dnsmessage.Resource
		switch r.URL.Path {
		case "/gemini":
			if question.Type != dnsmessage.TypeAAAA || question.Name.String() != "gemini.invalid." {
				t.Errorf("unexpected Gemini DoH question: %+v", question)
			}
			records = []dnsmessage.Resource{dnsAnswer(question.Name, dnsmessage.TypeAAAA, &dnsmessage.AAAAResource{
				AAAA: netip.MustParseAddr("2001:db8::1").As16(),
			})}
		case "/openai":
			switch {
			case question.Name.String() == "openai.invalid.":
				alias := dnsmessage.MustNewName("edge.invalid.")
				records = []dnsmessage.Resource{
					dnsAnswer(question.Name, dnsmessage.TypeCNAME, &dnsmessage.CNAMEResource{CNAME: alias}),
					dnsAnswer(dnsmessage.MustNewName("unrelated.invalid."), dnsmessage.TypeA, &dnsmessage.AResource{A: [4]byte{198, 51, 100, 9}}),
				}
				if question.Type == dnsmessage.TypeAAAA {
					records = append(records, dnsAnswer(alias, dnsmessage.TypeAAAA, &dnsmessage.AAAAResource{
						AAAA: netip.MustParseAddr("2001:db8::2").As16(),
					}))
				}
			case question.Type == dnsmessage.TypeA && question.Name.String() == "edge.invalid.":
				records = []dnsmessage.Resource{dnsAnswer(question.Name, dnsmessage.TypeA, &dnsmessage.AResource{A: [4]byte{192, 0, 2, 2}})}
			default:
				t.Errorf("unexpected OpenAI DoH question: %+v", question)
			}
		default:
			t.Errorf("unexpected DoH URL: %s", r.URL)
		}
		writeDoHAnswer(t, w, query, records)
	}))
	defer resolver.Close()
	endpoint := "https://" + net.JoinHostPort("example.com", resolverPort(resolver))
	logger := logging.New(logging.LevelError, io.Discard)
	defer logger.Close()
	for _, test := range []struct {
		provider, family string
		addresses        []netip.Addr
	}{
		{"gemini", "ipv6", []netip.Addr{netip.MustParseAddr("2001:db8::1")}},
		{"openai", "dual", []netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}},
	} {
		t.Run(test.provider, func(t *testing.T) {
			fail.Store(false)
			proxy := New(config.Relay{
				TargetHost: test.provider + ".invalid",
				IPFamily:   test.family,
				DoH: config.DoH{
					URL:          endpoint + "/" + test.provider,
					BootstrapIPs: []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")},
				},
			}, nil, logger)
			defer proxy.Close()
			trustDoHCertificate(proxy.doh, resolver.Certificate())
			if err := proxy.refresh(t.Context()); err != nil || !reflect.DeepEqual(proxy.snapshot(), test.addresses) {
				t.Fatalf("DoH refresh: %v, got %v, want %v", err, proxy.snapshot(), test.addresses)
			}
			fail.Store(true)
			if err := proxy.refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "DoH returned HTTP 503") ||
				!reflect.DeepEqual(proxy.snapshot(), test.addresses) {
				t.Fatalf("expected DoH error and last known addresses, got %v, %v", err, proxy.snapshot())
			}
		})
	}
	if queries.Load() != 7 {
		t.Fatalf("unexpected DoH query count: %d", queries.Load())
	}
	proxy := New(config.Relay{
		TargetHost:     "openai.invalid",
		IPFamily:       "ipv4",
		DestinationIPs: []netip.Addr{netip.MustParseAddr("192.0.2.1")},
		DoH:            config.DoH{URL: endpoint + "/openai"},
	}, nil, logger)
	defer proxy.Close()
	if proxy.doh != nil || !proxy.static {
		t.Fatal("explicit destination IPs should bypass DoH")
	}
}

func TestDoHRejectsRedirectAndUnrelatedResponse(t *testing.T) {
	resolver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/not-allowed", http.StatusTemporaryRedirect)
		case "/query":
			query := readDoHQuery(t, r)
			if query != nil {
				query.Questions[0].Name = dnsmessage.MustNewName("unrelated.invalid.")
				writeDoHAnswer(t, w, query, nil)
			}
		default:
			t.Error("DoH followed a redirect")
		}
	}))
	defer resolver.Close()
	for path, want := range map[string]string{
		"/redirect": "HTTP 307",
		"/query":    "question does not match",
	} {
		client := newDoHResolver(config.DoH{URL: resolver.URL + path})
		trustDoHCertificate(client, resolver.Certificate())
		defer client.client.CloseIdleConnections()
		if _, err := client.lookup(t.Context(), "ip4", "target.invalid"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("lookup %s: want %q, got %v", path, want, err)
		}
	}
}

func resolverPort(server *httptest.Server) string {
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	return port
}

func trustDoHCertificate(resolver *dohResolver, cert *x509.Certificate) {
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	resolver.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
}

func readDoHQuery(t *testing.T, r *http.Request) *dnsmessage.Message {
	t.Helper()
	wire, err := io.ReadAll(r.Body)
	var query dnsmessage.Message
	if err != nil || query.Unpack(wire) != nil || len(query.Questions) != 1 ||
		r.Method != http.MethodPost || r.Header.Get("Content-Type") != dnsMediaType || r.Header.Get("Accept") != dnsMediaType {
		t.Errorf("invalid wire-format DNS request to %s", r.URL)
		return nil
	}
	return &query
}

func dnsAnswer(name dnsmessage.Name, kind dnsmessage.Type, body dnsmessage.ResourceBody) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET, Type: kind, TTL: 60},
		Body:   body,
	}
}

func writeDoHAnswer(t *testing.T, w http.ResponseWriter, query *dnsmessage.Message, answers []dnsmessage.Resource) {
	t.Helper()
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true},
		Questions: query.Questions,
		Answers:   answers,
	}
	wire, err := response.Pack()
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Content-Type", dnsMediaType)
	_, _ = w.Write(wire)
}
