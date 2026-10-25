package cli

import (
        "context"
        "fmt"
        "math"
        "sync"
        "sync/atomic"
        "time"

        "github.com/miekg/dns"
        "golang.org/x/sync/singleflight"

        "github.com/Control-D-Inc/ctrld"
        "github.com/Control-D-Inc/ctrld/internal/dnscache"
)

// domainGraph tracks co-occurrence patterns of DNS queries per client IP,
// and uses them to speculatively prefetch domains that are likely to be
// queried next.
//
// Design summary:
//
//  1. recordQuery(clientIP, domain) is called after every successful
//     foreground resolve. It appends (now, domain) to the client's recent
//     query list, then for each prior query within windowSec (default 60s),
//     increments the bidirectional co-occurrence weight between domain and
//     the prior domain. Self-edges are skipped.
//
//  2. predictNeighbors(domain, topK) returns the top-K highest-weight
//     neighbors of domain whose weight >= minCoOccurrence. Used by the
//     prefetcher to speculatively fetch those neighbors in the background.
//
//  3. A periodic GC goroutine (every 5min) trims each client's recent
//     list to windowSec and decays all edge weights by decayFactor (0.95).
//     Old patterns fade so the graph adapts to changing client behavior.
//
//  4. The graph is bounded by maxGraphSize domains (default 10K). When
//     the cap is hit, the least-recently-seen domain is evicted with all
//     its edges. This bounds memory to ~5MB worst case.
//
//  5. recordPrefetchFailure(domain, neighbor) demotes the edge by 0.5x.
//     If a prediction turned out wrong (NXDOMAIN upstream), we unlearn it.
//
//  6. All mutations are behind a sync.RWMutex. predictNeighbors takes a
//     read lock; recordQuery takes a write lock briefly.
//
//  7. The graph is in-memory only — patterns don't survive restart.
//     This is intentional for v1: a fresh start lets the graph relearn
//     the current pattern set cleanly. Persistence is a v2 feature.
//
// Concurrency: safe for concurrent use. The lock is held briefly per
// mutation; the heaviest operation (GC) iterates under the read lock
// and only briefly upgrades to write for trimming.
type domainGraph struct {
        enabled bool

        // Configuration.
        windowSec      int           // how recent a prior query counts as "co-occurring"
        minCoOccur     float64       // minimum count to predict a neighbor (lifetime, never decays)
        topK           int           // max neighbors to predict per query
        maxGraphSize   int           // hard cap on distinct domains tracked
        halfLife       time.Duration // ranking half-life: stale patterns lose rank, never qualification
        historyWeight  float64       // multiplier for edges learned from log replay (default 3.0)
        gcInterval     time.Duration // how often GC runs
        statePath      string        // habit-graph persistence path ("" disables)
        stateHour      int           // local hour for the daily save (-1 = disabled)

        // State. All access via mu.
        mu       sync.RWMutex
        coOccur  map[string]map[string]float64 // domain -> neighbor -> lifetime count
        lastSeen map[string]time.Time          // domain -> last recordQuery time, for ranking + LRU
        recent   map[string][]timedQuery        // clientIP -> recent queries (newest last)

        // Background goroutine state.
        gcStop chan struct{}
        gcOnce sync.Once

        // Counters for observability.
        predictions atomic.Int64
        failures     atomic.Int64
        evictions    atomic.Int64

        // Persistence bookkeeping.
        dirty    bool       // set on any graph mutation; guarded by mu
        saveMu   sync.Mutex // serializes saves
        lastSave time.Time  // guarded by saveMu

        // The prefetch worker pool. singleflight coalesces concurrent prefetch
        // attempts for the same (domain, qtype, upstream) tuple.
        g      singleflight.Group
        cache  dnscache.Cacher
        um     *upstreamMonitor
        ctx    context.Context // bound to run()'s ctx so prefetches cancel on reload/stop
}

// timedQuery is one entry in a client's recent query history.
type timedQuery struct {
        at     time.Time
        domain string
}

// domainGraphKey is the context key used to disable recursive prefetching.
// Prefetch calls set this key in their ctx so that the foreground path
// (which they invoke via the prefetcher) doesn't trigger more prefetches.
type domainGraphKey struct{}

// isPrefetchCall reports whether ctx was set by the domain-graph prefetcher.
func isPrefetchCall(ctx context.Context) bool {
        return ctx.Value(domainGraphKey{}) != nil
}

// newDomainGraph constructs a domainGraph from p's config. Returns a
// disabled graph (all recordQuery/predictNeighbors calls are no-ops) if
// caching is off or domain prefetch is disabled.
func newDomainGraph(p *prog) *domainGraph {
        dg := &domainGraph{
                coOccur:      make(map[string]map[string]float64),
                lastSeen:     make(map[string]time.Time),
                recent:       make(map[string][]timedQuery),
                gcStop:       make(chan struct{}),
                windowSec:    60,
                minCoOccur:   5,
                topK:         5,
                maxGraphSize: 10000,
                halfLife:       72 * time.Hour,
                historyWeight:  3.0,
                gcInterval:     5 * time.Minute,
                stateHour:      0,
        }
        if !p.cfg.Service.CacheEnable || p.cache == nil {
                return dg
        }
        dg.enabled = true
        dg.cache = p.cache
        dg.um = p.um

        // Apply user overrides from config.
        if v := p.cfg.Service.DomainPrefetchMinCoOccurrence; v != nil && *v > 0 {
                dg.minCoOccur = float64(*v)
        }
        if v := p.cfg.Service.DomainPrefetchTopK; v != nil && *v > 0 {
                dg.topK = *v
        }
        if v := p.cfg.Service.DomainPrefetchWindowSec; v != nil && *v > 0 {
                dg.windowSec = *v
        }
        if v := p.cfg.Service.DomainPrefetchMaxGraphSize; v != nil && *v > 0 {
                dg.maxGraphSize = *v
        }
        if v := p.cfg.Service.DomainPrefetchHistoryWeight; v != nil && *v > 0 {
                dg.historyWeight = *v
        }
        if v := p.cfg.Service.DomainPrefetchMemoryHalfLifeHours; v != nil && *v > 0 {
                dg.halfLife = time.Duration(*v) * time.Hour
        }
        dg.statePath = "/var/lib/ctrld/domain-graph.jsonl"
        if v := p.cfg.Service.DomainPrefetchStatePath; v != nil {
                dg.statePath = *v
        }
        if v := p.cfg.Service.DomainPrefetchStateSaveHour; v != nil {
                if *v < 0 {
                        dg.stateHour = -1
                } else if *v <= 23 {
                        dg.stateHour = *v
                }
        }
        // Explicit disable via minCoOccurrence=0 OR topK=0.
        if v := p.cfg.Service.DomainPrefetchMinCoOccurrence; v != nil && *v == 0 {
                dg.enabled = false
        }
        if v := p.cfg.Service.DomainPrefetchTopK; v != nil && *v == 0 {
                dg.enabled = false
        }
        return dg
}

// start launches the GC goroutine. Safe to call multiple times.
func (dg *domainGraph) start(ctx context.Context) {
        if !dg.enabled {
                return
        }
        dg.ctx = ctx
        dg.gcOnce.Do(func() {
                go dg.gcLoop(ctx)
        })
}

// recordQuery records a LIVE client query (weight=1.0) — used by the
// proxy() hook when a foreground resolve succeeds.
func (dg *domainGraph) recordQuery(clientIP, domain string) {
        dg.recordQueryAt(clientIP, domain, time.Now(), 1.0)
}

// recordQueryWithWeight records that clientIP queried domain at the current
// time, incrementing co-occurrence edge weights by the given multiplier.
// Used by:
//   - Live queries from proxy() (weight=1.0, via recordQuery wrapper)
//   - API history replay (weight=historyWeight, default 3.0 — historical
//     patterns are based on ~10K queries worth of data, so they should
//     outweigh a few hours of live observation)
//
// For each prior query from the same client within windowSec, increments
// the bidirectional co-occurrence edge between domain and the prior domain
// by `weight`.
//
// O(N) where N = recent queries from this client (bounded by windowSec,
// typically 1-20 entries). Cheap.
func (dg *domainGraph) recordQueryWithWeight(clientIP, domain string, weight float64) {
        dg.recordQueryAt(clientIP, domain, time.Now(), weight)
}

// recordQueryAt records that clientIP queried domain at time `at`,
// incrementing co-occurrence edge weights by the given multiplier.
// The `at` parameter is critical for replay: if all replayed queries
// get time.Now(), they ALL fall within the 60s window, creating a
// clique where every domain co-occurs with every other domain.
// Using the original timestamp ensures only queries that were actually
// within 60s of each other in real traffic co-occur.
func (dg *domainGraph) recordQueryAt(clientIP, domain string, at time.Time, weight float64) {
        if !dg.enabled || clientIP == "" || domain == "" || weight <= 0 {
                return
        }
        // Guard against zero/garbage/future timestamps.
        if at.IsZero() || at.After(time.Now()) {
                at = time.Now()
        }
        domain = normalizeDomain(domain)

        dg.mu.Lock()
        defer dg.mu.Unlock()
        dg.dirty = true
        dg.lastSeen[domain] = at

        // Trim client's recent list to windowSec (measured from `at`).
        recent := dg.recent[clientIP]
        cutoff := at.Add(-time.Duration(dg.windowSec) * time.Second)
        cutIdx := 0
        for cutIdx < len(recent) && recent[cutIdx].at.Before(cutoff) {
                cutIdx++
        }
        if cutIdx > 0 {
                recent = recent[cutIdx:]
        }

        // Increment co-occurrence with each UNIQUE prior domain in the window
        // (skip self). Dedupe so if the client queried b.com 5 times in the
        // window, the a→b edge only increments by `weight` when a.com is
        // queried — this makes the weight track "number of times this pair
        // was seen together" rather than "combinatorial count of all (prior,
        // current) pairs", which would grow O(N^2) and saturate the threshold
        // quickly.
        seen := make(map[string]bool, len(recent))
        for _, q := range recent {
                if q.domain == domain {
                        continue
                }
                if seen[q.domain] {
                        continue
                }
                seen[q.domain] = true
                dg.addEdgeLocked(q.domain, domain, weight)
                dg.addEdgeLocked(domain, q.domain, weight)
        }

        // Append this query.
        recent = append(recent, timedQuery{at: at, domain: domain})
        dg.recent[clientIP] = recent

        // Enforce maxGraphSize via LRU eviction.
        dg.enforceCapLocked(at)
}

// addEdgeLocked increments the weight of the domain->neighbor edge by delta.
// Caller must hold dg.mu.
func (dg *domainGraph) addEdgeLocked(domain, neighbor string, delta float64) {
        if dg.coOccur[domain] == nil {
                dg.coOccur[domain] = make(map[string]float64)
        }
        dg.coOccur[domain][neighbor] += delta
}

// enforceCapLocked evicts the least-recently-seen domains until the graph
// is at or below maxGraphSize. Loops (not single-eviction) so a burst of
// new domains above the cap gets resolved in one call. Skips "ghosts"
// (domains in lastSeen but not in coOccur — left over from GC pruning)
// so eviction actually reduces the graph size. Caller must hold dg.mu.
func (dg *domainGraph) enforceCapLocked(now time.Time) {
        for len(dg.coOccur) > dg.maxGraphSize {
                var oldestDomain string
                var oldestTime time.Time
                for d, t := range dg.lastSeen {
                        // Skip ghosts: domains in lastSeen but not in coOccur.
                        if _, ok := dg.coOccur[d]; !ok {
                                continue
                        }
                        if oldestDomain == "" || t.Before(oldestTime) {
                                oldestDomain = d
                                oldestTime = t
                        }
                }
                if oldestDomain == "" {
                        return // no non-ghost domains to evict
                }
                delete(dg.coOccur, oldestDomain)
                for _, neighbors := range dg.coOccur {
                        delete(neighbors, oldestDomain)
                }
                delete(dg.lastSeen, oldestDomain)
                dg.evictions.Add(1)
        }
}

// predictNeighbors returns up to topK domains that have co-occurred with
// domain at least minCoOccur times. Qualification is by LIFETIME count
// (co-occurrence counts never decay), so a habit used once a week is
// still predicted immediately when its trigger domain is queried.
//
// Ranking uses an effective weight:
//
//      rank = count * 2^(-age / halfLife)
//
// where age is how long ago the neighbor was last queried. Hot, recent
// patterns take the top-K slots; equally-stale habit neighbors keep
// their relative order by count.
func (dg *domainGraph) predictNeighbors(domain string) []string {
        if !dg.enabled || domain == "" {
                return nil
        }
        domain = normalizeDomain(domain)

        dg.mu.RLock()
        defer dg.mu.RUnlock()
        neighbors := dg.coOccur[domain]
        if len(neighbors) == 0 {
                return nil
        }
        now := time.Now()
        type weighted struct {
                domain string
                rank   float64
        }
        candidates := make([]weighted, 0, len(neighbors))
        for n, count := range neighbors {
                if count < dg.minCoOccur {
                        continue
                }
                age := 30 * 24 * time.Hour // unknown lastSeen: rank as very stale
                if seen, ok := dg.lastSeen[n]; ok && !seen.IsZero() {
                        age = now.Sub(seen)
                        if age < 0 {
                                age = 0
                        }
                }
                rank := count * math.Pow(2, -float64(age)/float64(dg.halfLife))
                candidates = append(candidates, weighted{domain: n, rank: rank})
        }
        if len(candidates) == 0 {
                return nil
        }
        // Sort by rank descending; ties broken alphabetically. Insertion
        // sort — N is small (typically topK=5, max ~50).
        for i := 1; i < len(candidates); i++ {
                j := i
                for j > 0 {
                        if candidates[j-1].rank < candidates[j].rank ||
                                (candidates[j-1].rank == candidates[j].rank && candidates[j-1].domain > candidates[j].domain) {
                                candidates[j-1], candidates[j] = candidates[j], candidates[j-1]
                                j--
                        } else {
                                break
                        }
                }
        }
        k := dg.topK
        if k > len(candidates) {
                k = len(candidates)
        }
        out := make([]string, 0, k)
        for i := 0; i < k; i++ {
                out = append(out, candidates[i].domain)
        }
        dg.predictions.Add(int64(len(out)))
        return out
}

// logTopTriggers walks the graph and logs the top-N "trigger" domains —
// domains that have multiple qualifying neighbors (weight >= minCoOccur).
// These are the high-confidence patterns the graph has learned, e.g.:
//
//      domain-graph: trigger netflix.com -> [nflximg.com, nflxext.com, ...]
//      domain-graph: trigger youtube.com -> [googlevideo.com, i.ytimg.com, ...]
//
// Pure observability — no behavior change. Called after API history replay
// so you can see what patterns were learned from your historical traffic.
//
// domainCount returns the number of distinct domains currently in the
// co-occurrence graph (i.e., domains that have at least one edge).
// Used for logging the graph state after replay. Read-locked.
func (dg *domainGraph) domainCount() int {
        dg.mu.RLock()
        defer dg.mu.RUnlock()
        return len(dg.coOccur)
}

// Sort order: by total weight of qualifying edges (descending). Ties broken
// by domain name alphabetically for deterministic output.
func (dg *domainGraph) logTopTriggers() {
        if !dg.enabled {
                return
        }

        dg.mu.RLock()
        defer dg.mu.RUnlock()

        type trigger struct {
                domain     string
                neighbors  []string
                totalWeight float64
        }

        triggers := make([]trigger, 0)
        for domain, neighbors := range dg.coOccur {
                qualifying := make([]string, 0, len(neighbors))
                totalW := 0.0
                for n, w := range neighbors {
                        if w >= dg.minCoOccur {
                                qualifying = append(qualifying, n)
                                totalW += w
                        }
                }
                // A "trigger" has at least 2 qualifying neighbors — i.e. it's a
                // domain that, when queried, is followed by a meaningful set of
                // other domains. Single-neighbor entries are common (every domain
                // has at least one co-occurrence) but not interesting to log.
                if len(qualifying) >= 2 {
                        triggers = append(triggers, trigger{
                                domain:      domain,
                                neighbors:   qualifying,
                                totalWeight: totalW,
                        })
                }
        }

        // Sort by total weight desc; ties by domain name asc.
        for i := 1; i < len(triggers); i++ {
                j := i
                for j > 0 {
                        a, b := triggers[j-1], triggers[j]
                        if a.totalWeight < b.totalWeight ||
                                (a.totalWeight == b.totalWeight && a.domain > b.domain) {
                                triggers[j-1], triggers[j] = triggers[j], triggers[j-1]
                                j--
                        } else {
                                break
                        }
                }
        }

        // Log top 10 (or fewer if we don't have that many).
        count := len(triggers)
        if count > 10 {
                count = 10
        }
        if count == 0 {
                mainLog.Load().Info().Msg("domain-graph: no trigger domains learned yet (need >= 2 neighbors above threshold)")
                return
        }
        mainLog.Load().Info().Msgf("domain-graph: %d trigger domains learned (showing top %d):", len(triggers), count)
        for i := 0; i < count; i++ {
                t := triggers[i]
                // Cap the neighbor list display at 8 to keep logs readable.
                display := t.neighbors
                if len(display) > 8 {
                        display = display[:8]
                }
                mainLog.Load().Info().Msgf("  trigger %s (total_weight=%.1f) -> %v%s",
                        t.domain, t.totalWeight, display,
                        func() string {
                                if len(t.neighbors) > 8 {
                                        return " ..."
                                }
                                return ""
                        }())
        }
}

// recordPrefetchFailure demotes the edge between domain and neighbor by 0.5x,
// indicating that the prediction was wrong (e.g. neighbor returned NXDOMAIN
// when prefetch). Unlearning bad patterns keeps the graph accurate.
func (dg *domainGraph) recordPrefetchFailure(domain, neighbor string) {
        if !dg.enabled || domain == "" || neighbor == "" {
                return
        }
        domain = normalizeDomain(domain)
        neighbor = normalizeDomain(neighbor)

        dg.mu.Lock()
        defer dg.mu.Unlock()
        dg.dirty = true

        if neighbors, ok := dg.coOccur[domain]; ok {
                if w, ok := neighbors[neighbor]; ok {
                        neighbors[neighbor] = w * 0.5
                }
        }
        if neighbors, ok := dg.coOccur[neighbor]; ok {
                if w, ok := neighbors[domain]; ok {
                        neighbors[domain] = w * 0.5
                }
        }
        dg.failures.Add(1)
}

// gcLoop runs the periodic decay + trim pass.
func (dg *domainGraph) gcLoop(ctx context.Context) {
        ticker := time.NewTicker(dg.gcInterval)
        defer ticker.Stop()
        for {
                select {
                case <-ctx.Done():
                        return
                case <-dg.gcStop:
                        return
                case <-ticker.C:
                        dg.gcPass()
                        dg.maybeSaveStateNightly()
                }
        }
}

// gcPass trims expired entries from each client's recent list and prunes
// ghost lastSeen entries. Edge counts are NOT decayed: co-occurrence
// counts are lifetime "habit memory" and only lose RANKING priority over
// time (see predictNeighbors). Memory is bounded by the maxGraphSize LRU.
func (dg *domainGraph) gcPass() {
        if !dg.enabled {
                return
        }
        now := time.Now()
        cutoff := now.Add(-time.Duration(dg.windowSec) * time.Second)

        dg.mu.Lock()
        defer dg.mu.Unlock()

        // Trim recent per client.
        for clientIP, recent := range dg.recent {
                cutIdx := 0
                for cutIdx < len(recent) && recent[cutIdx].at.Before(cutoff) {
                        cutIdx++
                }
                if cutIdx > 0 {
                        recent = recent[cutIdx:]
                }
                if len(recent) == 0 {
                        delete(dg.recent, clientIP)
                } else {
                        dg.recent[clientIP] = recent
                }
        }

        // Prune lastSeen entries for domains that have no edges and have
        // not been queried within the window (ghosts; keeps the LRU scan
        // honest and bounds memory).
        for d, t := range dg.lastSeen {
                if t.Before(cutoff) {
                        if neighbors, ok := dg.coOccur[d]; !ok || len(neighbors) == 0 {
                                delete(dg.lastSeen, d)
                        }
                }
        }
}

// normalizeDomain lowercases and strips the trailing dot, so "Google.com."
// and "google.com" are treated identically.
func normalizeDomain(d string) string {
        if d == "" {
                return ""
        }
        if d[len(d)-1] == '.' {
                d = d[:len(d)-1]
        }
        b := []byte(d)
        for i := 0; i < len(b); i++ {
                if 'A' <= b[i] && b[i] <= 'Z' {
                        b[i] += 'a' - 'A'
                }
        }
        return string(b)
}

// domainPrefetchKey returns the singleflight key for a (domain, qtype, upstream) tuple.
func domainPrefetchKey(domain string, qtype uint16, upstream string) string {
        return fmt.Sprintf("%s:%d:%s", domain, qtype, upstream)
}

// maybePrefetchNeighbors is the hook called from proxy() after a successful
// foreground resolve and cache write. It records the query in the graph,
// then speculatively prefetches the predicted neighbors in the background.
//
// Prefetches are passed a ctx with the domainGraphKey set, so any further
// cache writes triggered by them won't recurse into more prefetching.
//
// Returns the number of prefetches scheduled (for metrics).
func (dg *domainGraph) maybePrefetchNeighbors(
        ctx context.Context,
        p *prog,
        clientIP string,
        queriedDomain string,
        queriedQtype uint16,
        upstream string,
        upstreamConfig *ctrld.UpstreamConfig,
) int {
        if !dg.enabled {
                return 0
        }
        if isPrefetchCall(ctx) {
                return 0 // don't recurse
        }
        // Only speculate on A and AAAA queries — these are the queries that
        // resource-loading patterns generate. CNAME, MX, TXT, SRV, etc. are
        // typically singletons (one-shot lookups, not part of a graph).
        if queriedQtype != dns.TypeA && queriedQtype != dns.TypeAAAA {
                return 0
        }

        // Record the query first (this is what builds the graph over time).
        dg.recordQuery(clientIP, queriedDomain)

        // Get predictions.
        neighbors := dg.predictNeighbors(queriedDomain)
        if len(neighbors) == 0 {
                return 0
        }

        // Skip if upstream is down — no point speculating against a dead upstream.
        if dg.um != nil && dg.um.isDown(upstream) {
                return 0
        }

        // Spawn a background prefetch goroutine per neighbor, coalesced by
        // singleflight so concurrent identical predictions collapse to one
        // upstream query.
        scheduled := 0
        for _, neighbor := range neighbors {
                neighbor := neighbor
                // Don't prefetch if neighbor is already cached and fresh.
                neighborMsg := new(dns.Msg)
                neighborMsg.SetQuestion(dns.Fqdn(neighbor), queriedQtype)
                if dg.cache != nil {
                        if v := dg.cache.Get(dnscache.NewKey(neighborMsg, upstream)); v != nil && v.Expire.After(time.Now()) {
                                continue
                        }
                }
                key := domainPrefetchKey(neighbor, queriedQtype, upstream)
                go func() {
                        _, _, _ = dg.g.Do(key, func() (any, error) {
                                // Use a ctx with the prefetch marker so the foreground
                                // path doesn't trigger more prefetching.
                                prefetchCtx := context.WithValue(ctx, domainGraphKey{}, true)
                                answer, err := resolveOne(prefetchCtx, upstreamConfig, neighborMsg)
                                // Only NXDOMAIN unlearns — transient errors (timeout,
                                // shutdown cancel, network) should NOT demote good patterns.
                                if answer != nil && answer.Rcode == dns.RcodeNameError {
                                        dg.recordPrefetchFailure(queriedDomain, neighbor)
                                        return nil, nil
                                }
                                if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess {
                                        return nil, nil // transient error, don't unlearn
                                }
                                // Cache the result — mirrors the foreground write path.
                                now := time.Now()
                                ttl := ttlFromMsg(answer)
                                if ttl == 0 {
                                        return nil, nil
                                }
                                expired := now.Add(time.Duration(ttl) * time.Second)
                                if cachedTTL := p.cfg.Service.CacheTTLOverride; cachedTTL > 0 {
                                        expired = now.Add(time.Duration(cachedTTL) * time.Second)
                                }
                                setCachedAnswerTTL(answer, now, expired)
                                dg.cache.Add(dnscache.NewKey(neighborMsg, upstream), dnscache.NewValue(answer, expired))
                                return nil, nil
                        })
                }()
                scheduled++
        }
        return scheduled
}

// resolveOne is a small helper that resolves msg via upstreamConfig and
// returns the response. Mirrors the resolve1 closure in dns_proxy.go but
// extracted here so the domain-graph prefetcher can call it directly.
func resolveOne(ctx context.Context, uc *ctrld.UpstreamConfig, msg *dns.Msg) (*dns.Msg, error) {
        if uc == nil {
                return nil, fmt.Errorf("upstream config is nil")
        }
        dnsResolver, err := ctrld.NewResolver(uc)
        if err != nil {
                return nil, err
        }
        resolveCtx, cancel := uc.Context(ctx)
        defer cancel()
        return dnsResolver.Resolve(resolveCtx, msg)
}
