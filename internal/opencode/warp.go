package opencode

import (
	"context"
	"net"

	"aizasy.pages.dev/ai-route/internal/config"
	"aizasy.pages.dev/ai-route/internal/ipv6"
	"aizasy.pages.dev/ai-route/internal/logging"
	"aizasy.pages.dev/ai-route/internal/warp"
)

type warpEgress interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	Release()
	DialNetwork() string
	EgressKey() string
}

type warpPool interface {
	Acquire(context.Context) (warpEgress, error)
	Report(warpEgress, int, []byte)
	Fail(warpEgress, string)
}

type sharedWARPPool struct {
	pool *warp.Pool
}

func (p *Proxy) failWARP(lease warpEgress, reason string) {
	if p.warpPool != nil && lease != nil {
		p.warpPool.Fail(lease, reason)
	}
}

func (p *sharedWARPPool) Acquire(ctx context.Context) (warpEgress, error) {
	lease, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func (p *sharedWARPPool) Report(egress warpEgress, status int, body []byte) {
	lease, ok := egress.(*warp.Lease)
	if ok {
		p.pool.Report(lease, status, body)
	}
}

func (p *sharedWARPPool) Fail(egress warpEgress, reason string) {
	lease, ok := egress.(*warp.Lease)
	if ok {
		p.pool.Fail(lease, reason)
	}
}

func NewWithWARP(cfg config.OpenCode, sources *ipv6.SourcePool, logger *logging.Logger, pool *warp.Pool) *Proxy {
	p := newProxy(cfg, sources, logger)
	if pool != nil {
		p.warpPool = &sharedWARPPool{pool: pool}
	}
	return p
}
