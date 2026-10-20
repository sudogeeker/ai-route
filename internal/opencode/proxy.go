package opencode

import (
	"context"
	"io"
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
	maxAttempts      = 10
	maxCompatRetries = 2
	maxEventBytes    = 1 << 20
	maxResponseBytes = 64 << 20
	initialRetryWait = 100 * time.Millisecond
	maxRetryWait     = 2 * time.Second
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

func (p *Proxy) ServeChat(w http.ResponseWriter, r *http.Request, body []byte, streaming bool) {
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
			_, _ = io.Copy(httpx.FlushWriter{ResponseWriter: w}, result.stream)
		} else {
			_, _ = w.Write(result.body)
		}
	})
}

// ServeResponses translates stateless Responses API calls into the same Chat
// Completions races used by the direct chat endpoint.
func (p *Proxy) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
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
			_ = plan.WriteStream(stream, httpx.FlushWriter{ResponseWriter: w})
			return
		}
		data, err := plan.Complete(stream)
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}

// serve owns the ten-way race and hands the winning transport response to the
// protocol-specific writer. A completed wave is fully drained before another
// starts, so retries retain a fixed concurrency and response-body footprint.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, request chatRequest, identity requestIdentity, deliver func(http.ResponseWriter, *attemptResult)) {
	type numberedResult struct {
		index  int
		result attemptResult
	}
	timer := time.NewTimer(p.cfg.WaitTimeout)
	defer timer.Stop()
	retryWait := initialRetryWait
	for {
		sources, err := p.sources(maxAttempts)
		if err != nil || len(sources) != maxAttempts {
			httpx.WriteError(w, http.StatusBadGateway, "connection_error", "Cannot allocate OpenCode source IPv6 addresses")
			return
		}
		waveCtx, cancelWave := context.WithCancel(r.Context())
		cancels := make([]context.CancelFunc, maxAttempts)
		numbered := make(chan numberedResult, maxAttempts)
		for index, source := range sources {
			attemptCtx, attemptCancel := context.WithCancel(waveCtx)
			cancels[index] = attemptCancel
			go func(index int, source net.IP) {
				numbered <- numberedResult{index: index, result: p.attempt(attemptCtx, source, request, identity)}
			}(index, source)
		}

		received := 0
		retryable := false
		for received < maxAttempts {
			var outcome numberedResult
			select {
			case <-r.Context().Done():
				cancelWave()
				for received < maxAttempts {
					pending := <-numbered
					pending.result.cleanup()
					received++
				}
				return
			case <-timer.C:
				cancelWave()
				for received < maxAttempts {
					pending := <-numbered
					pending.result.cleanup()
					received++
				}
				httpx.WriteError(w, http.StatusGatewayTimeout, "timeout_error", "OpenCode race timed out")
				return
			case outcome = <-numbered:
			}
			received++
			result := outcome.result
			status := 0
			if result.response != nil {
				status = result.response.StatusCode
			}
			if p.logger != nil {
				p.logger.Debugf("OpenCode attempt=%d source=%s status=%d failed=%t", outcome.index+1, sources[outcome.index], status, result.err != nil)
			}
			if result.err != nil {
				if result.response != nil && status != http.StatusOK {
					retryable = true
				}
				result.cleanup()
				continue
			}
			for index, stop := range cancels {
				if index != outcome.index {
					stop()
				}
			}
			// Keep exactly one winner readable, but wait for every loser to close
			// its body before returning. This prevents retry waves from stacking
			// goroutines or response buffers.
			for received < maxAttempts {
				pending := <-numbered
				pending.result.cleanup()
				received++
			}
			timer.Stop()
			deliver(w, &result)
			cancelWave()
			result.cleanup()
			return
		}
		cancelWave()
		if !retryable {
			httpx.WriteError(w, http.StatusBadGateway, "upstream_error", "All 10 OpenCode attempts failed without a valid response")
			return
		}

		retryTimer := time.NewTimer(retryWait)
		select {
		case <-r.Context().Done():
			retryTimer.Stop()
			return
		case <-timer.C:
			retryTimer.Stop()
			httpx.WriteError(w, http.StatusGatewayTimeout, "timeout_error", "OpenCode race timed out")
			return
		case <-retryTimer.C:
		}
		if retryWait < maxRetryWait {
			retryWait *= 2
			if retryWait > maxRetryWait {
				retryWait = maxRetryWait
			}
		}
	}
}
