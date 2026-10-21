package opencode

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/httpx"
	"aizasy.pages.dev/ai-route/internal/ipv6"
	"aizasy.pages.dev/ai-route/internal/logging"
	"aizasy.pages.dev/ai-route/internal/responses"
)

const (
	maxCompatRetries = 2
	maxErrorBytes    = 1 << 20
	maxEventBytes    = 1 << 20
	maxResponseBytes = 64 << 20
)

// Proxy owns OpenCode racing and keeps its transport and AAAA state isolated.
type Proxy struct {
	cfg       config.OpenCode
	logger    *logging.Logger
	dns       *dnsCache
	sources   func(int) ([]net.IP, error)
	transport func(net.IP) *http.Transport
}

func New(cfg config.OpenCode, sources *ipv6.SourcePool, logger *logging.Logger) *Proxy {
	p := &Proxy{
		cfg:     cfg,
		logger:  logger,
		dns:     &dnsCache{host: cfg.BaseURL.Hostname(), lookup: net.DefaultResolver.LookupIP},
		sources: sources.Unique,
	}
	p.transport = p.newTransport
	return p
}

func (p *Proxy) SupportsModel(model string) bool { return p.cfg.Models[model] }

func (p *Proxy) Models() []string {
	models := make([]string, 0, len(p.cfg.Models))
	for model := range p.cfg.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (p *Proxy) streamIdleTimeout() time.Duration {
	if p.cfg.StreamIdleTimeout > 0 {
		return p.cfg.StreamIdleTimeout
	}
	return 60 * time.Second
}

func (p *Proxy) concurrency() int {
	if p.cfg.Concurrency > 0 {
		return p.cfg.Concurrency
	}
	return config.DefaultOpenCodeConcurrency
}

func (p *Proxy) ServeChat(w http.ResponseWriter, r *http.Request, body []byte, streaming bool) {
	w = httpx.DeadlineWriter{ResponseWriter: w, Timeout: p.streamIdleTimeout()}
	request, err := prepareChat(body, streaming, p.cfg.FreeCompat)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	identity, err := newRequestIdentity()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Cannot allocate OpenCode request identifiers")
		return
	}
	p.serve(w, r, request, identity, func(w http.ResponseWriter, result *attemptResult) {
		httpx.CopyHeaders(w.Header(), result.response.Header)
		w.WriteHeader(result.response.StatusCode)
		if streaming {
			if err := copyChatStream(httpx.FlushWriter{ResponseWriter: w}, result.stream); err != nil && p.logger != nil && r.Context().Err() == nil {
				p.logger.Errorf("OpenCode chat stream failed: %v", err)
			}
		} else {
			_, _ = w.Write(result.body)
		}
	})
}

// ServeResponses translates stateless Responses API calls into the same Chat
// Completions races used by the direct chat endpoint.
func (p *Proxy) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	w = httpx.DeadlineWriter{ResponseWriter: w, Timeout: p.streamIdleTimeout()}
	padding := chatTools
	if requestHasTools(body) {
		padding = chatCoreTools
	}
	if !p.cfg.FreeCompat {
		padding = nil
	}
	plan, err := responses.Prepare(body, padding)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	identity, err := newRequestIdentity()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Cannot allocate OpenCode request identifiers")
		return
	}
	p.serveResponsesRound(w, r, plan, identity, plan.ChatBody, 0)
}

func (p *Proxy) serveResponsesRound(w http.ResponseWriter, r *http.Request, plan *responses.Plan, identity requestIdentity, body []byte, round int) {
	p.serve(w, r, chatRequest{body: body, streaming: true}, identity, func(w http.ResponseWriter, result *attemptResult) {
		compat, stream, err := plan.ProbeCompat(result.stream)
		if err != nil {
			if p.logger != nil && r.Context().Err() == nil {
				p.logger.Errorf("OpenCode Responses prelude failed: %v", err)
			}
			httpx.WriteError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		if len(compat) != 0 {
			if round >= maxCompatRetries {
				httpx.WriteError(w, http.StatusBadGateway, "upstream_error", "OpenCode repeatedly called compatibility-only tools")
				return
			}
			next, err := responses.AppendCompatRetry(body, compat, plan.CallerTools())
			if err != nil {
				httpx.WriteError(w, http.StatusBadGateway, "upstream_error", err.Error())
				return
			}
			p.serveResponsesRound(w, r, plan, identity, next, round+1)
			return
		}
		if plan.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			if err := plan.WriteStream(stream, httpx.FlushWriter{ResponseWriter: w}); err != nil && p.logger != nil && r.Context().Err() == nil {
				p.logger.Errorf("OpenCode Responses stream failed: %v", err)
			}
			return
		}
		data, err := plan.Complete(stream)
		if err != nil {
			if p.logger != nil && r.Context().Err() == nil {
				p.logger.Errorf("OpenCode Responses collection failed: %v", err)
			}
			httpx.WriteError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}

// serve owns the configured race and hands the winning transport response to the
// protocol-specific writer. The winner remains readable until deliver returns.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, request chatRequest, identity requestIdentity, deliver func(http.ResponseWriter, *attemptResult)) {
	maxAttempts := p.concurrency()
	sources, err := p.sources(maxAttempts)
	if err != nil || len(sources) != maxAttempts {
		httpx.WriteError(w, http.StatusBadGateway, "connection_error", "Cannot allocate OpenCode source IPv6 addresses")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cancels := make([]context.CancelFunc, maxAttempts)
	type numberedResult struct {
		index  int
		result attemptResult
	}
	numbered := make(chan numberedResult, maxAttempts)
	for index, source := range sources {
		attemptCtx, attemptCancel := context.WithCancel(ctx)
		cancels[index] = attemptCancel
		go func(index int, source net.IP) {
			numbered <- numberedResult{index: index, result: p.attempt(attemptCtx, source, request, identity)}
		}(index, source)
	}
	received, winner := 0, -1
	defer func() {
		for index, stop := range cancels {
			if index != winner {
				stop()
			}
		}
		// Results already in flight still own response bodies, including other winners.
		if received < maxAttempts {
			go func(remaining int) {
				for i := 0; i < remaining; i++ {
					result := <-numbered
					result.result.cleanup()
				}
			}(maxAttempts - received)
		}
	}()
	timer := time.NewTimer(p.cfg.WaitTimeout)
	defer timer.Stop()
	var lastHTTP *attemptResult
	var lastErr error
	for received < maxAttempts {
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			if p.logger != nil {
				p.logger.Errorf("OpenCode race timed out after %s: %v", p.cfg.WaitTimeout, lastErr)
			}
			httpx.WriteError(w, http.StatusGatewayTimeout, "timeout_error", "OpenCode race timed out")
			return
		case outcome := <-numbered:
			received++
			result := outcome.result
			if p.logger != nil {
				status := 0
				if result.response != nil {
					status = result.response.StatusCode
				}
				p.logger.Debugf("OpenCode attempt=%d source=%s status=%d error=%v", outcome.index+1, sources[outcome.index], status, result.err)
			}
			if result.err != nil {
				lastErr = result.err
				if result.response != nil && (result.response.StatusCode < 200 || result.response.StatusCode >= 300) {
					copy := result
					lastHTTP = &copy
				}
				result.cleanup()
				continue
			}
			winner = outcome.index
			for index, stop := range cancels {
				if index != winner {
					stop()
				}
			}
			remaining := maxAttempts - received
			go func() {
				for i := 0; i < remaining; i++ {
					pending := <-numbered
					pending.result.cleanup()
				}
			}()
			received = maxAttempts
			timer.Stop()
			defer result.cleanup()
			deliver(w, &result)
			return
		}
	}
	if p.logger != nil {
		p.logger.Errorf("All %d OpenCode attempts failed: %v", maxAttempts, lastErr)
	}
	if lastHTTP != nil {
		if lastHTTP.response.StatusCode == http.StatusTooManyRequests {
			httpx.WriteError(w, http.StatusServiceUnavailable, "upstream_rate_limit", "OpenCode upstream returned HTTP 429")
			return
		}
		httpx.CopyHeaders(w.Header(), lastHTTP.response.Header)
		w.WriteHeader(lastHTTP.response.StatusCode)
		_, _ = w.Write(lastHTTP.body)
		return
	}
	httpx.WriteError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("All %d OpenCode attempts failed without a valid response", maxAttempts))
}
