package cli

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sync/singleflight"

	"github.com/Control-D-Inc/ctrld"
	"github.com/Control-D-Inc/ctrld/internal/dnscache"
)

// prefetcher warms the DNS cache by re-resolving entries that are about to
// expire, so the next client query hits the cache instead of going upstream.
//
// Design notes (kept here so future readers see the reasoning in one place):
//
//   - Workload-driven, not speculative. A refresh is triggered only when a
//     cache hit is served with little TTL remaining. There is no separate
//     sweeper goroutine and no parallel key index. ctrld only spends upstream
//     queries on domains someone is actually asking about.
//
//   - No change to the dnscache.Cacher interface. The cached Msg's TTL field
//     already equals the original upstream TTL at storage time (set by
//     setCachedAnswerTTL at dns_proxy.go:744), so the prefetcher can derive
//     the original TTL via ttlFromMsg(cachedValue.Msg) without adding a
//     field to dnscache.Value.
//
//   - Coalesced via a per-prefetcher singleflight.Group. A burst of client
//     queries for the same near-expiry entry causes exactly one upstream
//     refresh, not N. The foreground path uses UpstreamConfig.g only for
//     ReBootstrap, so there is no double-coalescing.
//
//   - Jittered start (0..jitter). When many entries from the same upstream
//     expire at the same instant (common when ctrld has just started and
//     filled its cache from one warm upstream), each prefetch goroutine
//     picks a random delay before firing so we don't stampede the upstream
//     with N simultaneous queries.
//
//   - Bound to run()'s ctx. All prefetch goroutines are cancelled on
//     reload/stop, matching how runClientInfoDiscover, runMetricsServer,
//     and checkDnsLoopTicker are scoped.
//
//   - Skips when upstream is marked down by upstreamMonitor. Matches
//     ctrld's failover semantics — no point refreshing against a dead
//     upstream.
//
//   - Respects PTR / CacheFlushDomains exclusions automatically: the hook
//     sits inside proxy()'s cache-hit branch, which is already gated by
//     `req.msg.Question[0].Qtype != dns.TypePTR`. CacheFlushDomains hits
//     p.cache.Purge() earlier in serveDNS, so by the time we reach the
//     cache-hit branch the cache no longer contains those entries.
type prefetcher struct {
	enabled   bool
	threshold float64       // fraction of TTL remaining at which to refresh
	minTTL    time.Duration // skip entries with original TTL below this
	jitter    time.Duration // max random delay before refresh fires

	cache dnscache.Cacher
	um    *upstreamMonitor

	// g coalesces concurrent prefetches for the same (qtype, qclass, qname,
	// upstream) tuple. Key shape matches dnscache.Key so prefetch + a
	// concurrent foreground resolve for the same key don't duplicate work
	// (the foreground doesn't use this group, but if multiple prefetches
	// race, only one runs).
	g singleflight.Group

	// refreshes counts prefetch attempts (including coalesced duplicates that
	// were absorbed by singleflight). For observability via prometheus.
	refreshes atomic.Int64

	// ctx is the lifecycle context from run(); all prefetch goroutines are
	// bound to it so they're cancelled on reload/stop.
	ctx context.Context
}

// prefetcherKey returns the singleflight key for a (msg, upstream) tuple.
// Mirrors dnscache.Key's shape so coalescing aligns with cache identity.
func prefetcherKey(msg *dns.Msg, upstream string) string {
	if len(msg.Question) == 0 {
		return upstream
	}
	q := msg.Question[0]
	return fmt.Sprintf("%d:%d:%s:%s", q.Qtype, q.Qclass, q.Name, upstream)
}

// newPrefetcher constructs a prefetcher from p's config. Returns a disabled
// prefetcher (all maybePrefetch calls are no-ops) if caching is off or the
// cache wasn't initialized. This keeps the call site in dns_proxy.go
// unconditional — no nil checks needed there.
//
// Defaults (applied only when CacheEnable=true):
//   - threshold: 0.25  (refresh when 25% of original TTL remains)
//   - minTTL:    10s   (don't thrash on ultra-short-TTL CDN entries)
//   - jitter:    2s    (spread simultaneous expirations)
//
// All three are overridable via ServiceConfig.
func newPrefetcher(p *prog) *prefetcher {
	pf := &prefetcher{
		cache:  p.cache,
		um:     p.um,
		jitter: 2 * time.Second,
	}
	if !p.cfg.Service.CacheEnable || p.cache == nil {
		return pf
	}
	pf.enabled = true
	pf.minTTL = 10 * time.Second
	pf.threshold = 0.25
	if t := p.cfg.Service.CachePrefetchThreshold; t != nil {
		if *t > 0 && *t < 1 {
			pf.threshold = *t
		} else if *t == 0 {
			// Explicit zero disables prefetch even if CacheEnable is on.
			pf.enabled = false
		}
	}
	if m := p.cfg.Service.CachePrefetchMinTTL; m != nil && *m > 0 {
		pf.minTTL = time.Duration(*m) * time.Second
	}
	return pf
}

// maybePrefetch schedules a background refresh of (msg, upstream) if all of:
//   - prefetcher is enabled;
//   - msg has a question and is not a PTR query (PTR is never cached);
//   - the cached entry's original TTL >= minTTL (avoid thrashing on 1-5s
//     CDN TTLs);
//   - the cached entry's remaining TTL is at or below threshold * originalTTL;
//   - the upstream is not currently marked down by upstreamMonitor.
//
// The refresh runs in its own goroutine; this function returns immediately
// (the foreground cache hit is unaffected). Concurrent calls for the same
// key are coalesced by singleflight, so even if 100 client queries for the
// same near-expiry entry arrive simultaneously, only one upstream refresh
// fires.
//
// Returns true if a refresh was scheduled (or coalesced into an in-flight
// one). The caller may use this for metrics, but currently just ignores it.
func (pf *prefetcher) maybePrefetch(
	p *prog,
	msg *dns.Msg,
	upstream string,
	upstreamConfig *ctrld.UpstreamConfig,
	cachedExpire time.Time,
	originalTTL uint32,
) bool {
	if pf == nil || !pf.enabled {
		return false
	}
	if len(msg.Question) == 0 {
		return false
	}
	if msg.Question[0].Qtype == dns.TypePTR {
		return false
	}
	ttl := time.Duration(originalTTL) * time.Second
	if ttl < pf.minTTL {
		return false
	}
	now := time.Now()
	remaining := cachedExpire.Sub(now)
	// Refresh only if remaining TTL is at or below the threshold fraction.
	if remaining > time.Duration(float64(ttl)*pf.threshold) {
		return false
	}
	if pf.um != nil && pf.um.isDown(upstream) {
		return false
	}
	if pf.ctx == nil {
		return false
	}

	key := prefetcherKey(msg, upstream)
	pf.refreshes.Add(1)
	statsCachePrefetchRefreshes.Inc()

	// Non-blocking: the foreground cache hit has already returned; we don't
	// want the client to wait for the refresh. singleflight still coalesces
	// concurrent prefetches for the same key.
	go func() {
		_, _, _ = pf.g.Do(key, func() (any, error) {
			// Jitter the start so a burst of co-located expiries (e.g.
			// 50 entries from one upstream all expiring at the same instant)
			// doesn't fire 50 simultaneous upstream queries. Different keys
			// are independent, so the jitter only matters within a burst.
			delay := time.Duration(0)
			if remaining > 0 && pf.jitter > 0 {
				delay = time.Duration(rand.Int64N(int64(pf.jitter)))
			}
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-pf.ctx.Done():
					timer.Stop()
					return nil, nil
				case <-timer.C:
				}
			}
			pf.refresh(p, msg, upstream, upstreamConfig)
			return nil, nil
		})
	}()
	return true
}

// refresh re-resolves msg via upstreamConfig and re-Adds the result to the
// cache. Mirrors the foreground write path at dns_proxy.go:596-605 so the
// cached entry looks identical to a freshly-resolved one (same TTL handling,
// same CacheTTLOverride semantics, same setCachedAnswerTTL mutation).
//
// On any failure (resolver creation, Resolve error, non-NOERROR rcode, zero
// TTL), the refresh is silently dropped — the cache retains the stale entry
// which will be served stale per the existing CacheServeStale semantics.
func (pf *prefetcher) refresh(
	p *prog,
	msg *dns.Msg,
	upstream string,
	upstreamConfig *ctrld.UpstreamConfig,
) {
	if pf.ctx == nil || upstreamConfig == nil {
		return
	}
	ctx := pf.ctx

	dnsResolver, err := ctrld.NewResolver(upstreamConfig)
	if err != nil {
		mainLog.Load().Debug().Err(err).
			Msgf("prefetch: failed to create resolver for %s", upstream)
		return
	}
	resolveCtx, cancel := upstreamConfig.Context(ctx)
	defer cancel()

	// Build a fresh query message so we don't mutate the caller's msg
	// (the foreground may still be using it for setCachedAnswerTTL etc.).
	// We carry over Question + EDNS0 if present, nothing else — prefetch
	// queries are intentionally minimal.
	q := new(dns.Msg)
	q.SetQuestion(msg.Question[0].Name, msg.Question[0].Qtype)
	q.Question[0].Qclass = msg.Question[0].Qclass
	if opt := msg.IsEdns0(); opt != nil {
		q.SetEdns0(opt.UDPSize(), true)
	}

	answer, err := dnsResolver.Resolve(resolveCtx, q)
	if err != nil || answer == nil {
		mainLog.Load().Debug().Err(err).
			Msgf("prefetch: failed to refresh %s via %s",
				msg.Question[0].Name, upstream)
		return
	}
	if answer.Rcode != dns.RcodeSuccess {
		return
	}
	ttl := ttlFromMsg(answer)
	if ttl == 0 {
		return // nothing to cache
	}
	now := time.Now()
	expired := now.Add(time.Duration(ttl) * time.Second)
	if cachedTTL := p.cfg.Service.CacheTTLOverride; cachedTTL > 0 {
		expired = now.Add(time.Duration(cachedTTL) * time.Second)
	}
	setCachedAnswerTTL(answer, now, expired)
	pf.cache.Add(dnscache.NewKey(msg, upstream), dnscache.NewValue(answer, expired))

	mainLog.Load().Debug().
		Str("query", msg.Question[0].Name).
		Str("upstream", upstream).
		Uint32("ttl", ttl).
		Msg("prefetch: refreshed cache entry")
}
