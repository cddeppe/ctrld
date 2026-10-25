package cli

import (
        "context"
        "testing"
        "time"

        "github.com/miekg/dns"
        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"

        "github.com/Control-D-Inc/ctrld"
        "github.com/Control-D-Inc/ctrld/internal/dnscache"
        "github.com/Control-D-Inc/ctrld/testhelper"
)

// TestDomainGraph_DisabledByDefault verifies the graph is disabled when
// CacheEnable=false (no point tracking patterns if we can't act on them).
func TestDomainGraph_DisabledByDefault(t *testing.T) {
        t.Run("cache disabled", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = false
                p := &prog{cfg: cfg}
                dg := newDomainGraph(p)
                assert.False(t, dg.enabled, "graph should be disabled when CacheEnable=false")
        })

        t.Run("cache enabled — defaults applied", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                dg := newDomainGraph(p)
                assert.True(t, dg.enabled, "graph should be enabled when CacheEnable=true")
                assert.Equal(t, 60, dg.windowSec, "default windowSec should be 60")
                assert.Equal(t, float64(5), dg.minCoOccur, "default minCoOccur should be 5")
                assert.Equal(t, 5, dg.topK, "default topK should be 5")
                assert.Equal(t, 10000, dg.maxGraphSize, "default maxGraphSize should be 10000")
                assert.Equal(t, 72*time.Hour, dg.halfLife, "default halfLife should be 72h")
        })

        t.Run("explicit disable via min_co_occurrence=0", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                zero := 0
                cfg.Service.DomainPrefetchMinCoOccurrence = &zero
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                dg := newDomainGraph(p)
                assert.False(t, dg.enabled, "graph should be disabled when min_co_occurrence=0")
        })

        t.Run("explicit disable via top_k=0", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                zero := 0
                cfg.Service.DomainPrefetchTopK = &zero
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                dg := newDomainGraph(p)
                assert.False(t, dg.enabled, "graph should be disabled when top_k=0")
        })
}

// TestDomainGraph_RecordQuery_BuildsCoOccurrence verifies that recording
// queries from the same client within the window builds bidirectional
// co-occurrence edges. Two queries A, B → A→B weight 1, B→A weight 1.
// Three queries A, B, C → A→B 1, A→C 1, B→A 1, B→C 1, C→A 1, C→B 1.
func TestDomainGraph_RecordQuery_BuildsCoOccurrence(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        // Three queries from the same client, all within window.
        dg.recordQuery("10.0.0.1", "a.com")
        dg.recordQuery("10.0.0.1", "b.com")
        dg.recordQuery("10.0.0.1", "c.com")

        dg.mu.RLock()
        defer dg.mu.RUnlock()
        // a→b: 1, a→c: 1
        assert.Equal(t, float64(1), dg.coOccur["a.com"]["b.com"])
        assert.Equal(t, float64(1), dg.coOccur["a.com"]["c.com"])
        // b→a: 1, b→c: 1
        assert.Equal(t, float64(1), dg.coOccur["b.com"]["a.com"])
        assert.Equal(t, float64(1), dg.coOccur["b.com"]["c.com"])
        // c→a: 1, c→b: 1
        assert.Equal(t, float64(1), dg.coOccur["c.com"]["a.com"])
        assert.Equal(t, float64(1), dg.coOccur["c.com"]["b.com"])
}

// TestDomainGraph_RecordQuery_SkipsSelf verifies the graph doesn't create
// self-edges when the same domain is queried repeatedly.
func TestDomainGraph_RecordQuery_SkipsSelf(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        // Same domain three times — should not produce any edges.
        dg.recordQuery("10.0.0.1", "a.com")
        dg.recordQuery("10.0.0.1", "a.com")
        dg.recordQuery("10.0.0.1", "a.com")

        dg.mu.RLock()
        defer dg.mu.RUnlock()
        assert.Empty(t, dg.coOccur["a.com"], "no self-edges should be created")
}

// TestDomainGraph_RecordQuery_WindowExpiry verifies that queries older
// than windowSec don't contribute to new co-occurrences.
func TestDomainGraph_RecordQuery_WindowExpiry(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        cfg.Service.DomainPrefetchWindowSec = ptrInt(2) // 2s window
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        dg.recordQuery("10.0.0.1", "a.com")
        // Wait until the window has expired for a.com.
        time.Sleep(2200 * time.Millisecond)
        dg.recordQuery("10.0.0.1", "b.com")

        // a.com is outside the window, so no co-occurrence edges should exist.
        dg.mu.RLock()
        defer dg.mu.RUnlock()
        assert.Empty(t, dg.coOccur["a.com"], "no edges from expired a.com")
        assert.Empty(t, dg.coOccur["b.com"], "no edges to b.com from expired a.com")
}

// TestDomainGraph_PredictNeighbors verifies prediction returns the top-K
// highest-weight neighbors above the min threshold. Uses direct edge
// manipulation (addEdgeLocked) for predictable weights, bypassing the
// complex recordQuery semantics which can produce surprising weights
// due to all-priors-in-window co-occurrence.
func TestDomainGraph_PredictNeighbors(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        cfg.Service.DomainPrefetchMinCoOccurrence = ptrInt(3) // require weight >= 3
        cfg.Service.DomainPrefetchTopK = ptrInt(2)             // top 2 only
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        // Set up edges with known weights.
        dg.mu.Lock()
        dg.addEdgeLocked("a.com", "b.com", 5)   // above threshold, weight 5
        dg.addEdgeLocked("a.com", "c.com", 2)   // below threshold 3, excluded
        dg.addEdgeLocked("a.com", "d.com", 10)  // highest weight
        dg.addEdgeLocked("a.com", "e.com", 7)   // second highest
        dg.mu.Unlock()

        neighbors := dg.predictNeighbors("a.com")
        require.Len(t, neighbors, 2, "top-K=2 should return 2 neighbors (b, c, d, e all qualify but only top 2)")
        assert.Equal(t, "d.com", neighbors[0], "highest-weight neighbor should be d.com (weight 10)")
        assert.Equal(t, "e.com", neighbors[1], "second-highest should be e.com (weight 7)")
}

// TestDomainGraph_PredictNeighbors_BelowThreshold verifies that no
// predictions are made when all neighbors are below the min threshold.
func TestDomainGraph_PredictNeighbors_BelowThreshold(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        cfg.Service.DomainPrefetchMinCoOccurrence = ptrInt(100) // require weight >= 100
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        // Build a small graph (weights well below 100).
        for i := 0; i < 5; i++ {
                dg.recordQuery("c1", "a.com")
                dg.recordQuery("c1", "b.com")
        }

        neighbors := dg.predictNeighbors("a.com")
        assert.Empty(t, neighbors, "no neighbors should meet threshold 100")
}

// TestDomainGraph_PredictNeighbors_UnknownDomain verifies that querying
// an unseen domain returns no predictions.
func TestDomainGraph_PredictNeighbors_UnknownDomain(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        neighbors := dg.predictNeighbors("never.seen.com")
        assert.Empty(t, neighbors)
}

// TestDomainGraph_LRU_Eviction verifies that exceeding maxGraphSize
// evicts the least-recently-seen domain.
func TestDomainGraph_LRU_Eviction(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        cfg.Service.DomainPrefetchMaxGraphSize = ptrInt(3) // tiny cap for testing
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        // Add 3 domains in order: a, b, c
        dg.recordQuery("c1", "a.com")
        dg.recordQuery("c1", "b.com")
        dg.recordQuery("c1", "c.com")
        // Touch a.com again so b.com becomes the oldest.
        dg.recordQuery("c1", "a.com")

        // Add d.com — should evict b.com (oldest by lastSeen).
        dg.recordQuery("c1", "d.com")

        dg.mu.RLock()
        defer dg.mu.RUnlock()
        assert.Contains(t, dg.coOccur, "a.com", "a.com should still be present (recently touched)")
        assert.NotContains(t, dg.coOccur, "b.com", "b.com should have been evicted")
        assert.Contains(t, dg.coOccur, "c.com", "c.com should still be present")
        assert.Contains(t, dg.coOccur, "d.com", "d.com should be present (just added)")
}

// TestDomainGraph_RecordPrefetchFailure verifies that failed prefetches
// demote the edge weight by 0.5x in both directions.
func TestDomainGraph_RecordPrefetchFailure(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        // Build an edge with weight 19 (10 alternating (a,b) pairs = 20 calls).
        for i := 0; i < 10; i++ {
                dg.recordQuery("c1", "a.com")
                dg.recordQuery("c1", "b.com")
        }

        // Two failures should demote to 19 * 0.5 * 0.5 = 4.75
        dg.recordPrefetchFailure("a.com", "b.com")
        dg.recordPrefetchFailure("a.com", "b.com")

        dg.mu.RLock()
        defer dg.mu.RUnlock()
        assert.InDelta(t, 4.75, dg.coOccur["a.com"]["b.com"], 0.001, "two failures should demote to 4.75")
        assert.InDelta(t, 4.75, dg.coOccur["b.com"]["a.com"], 0.001, "reverse edge should also be demoted")}

// TestDomainGraph_NormalizeDomain verifies case + trailing-dot normalization.
func TestDomainGraph_NormalizeDomain(t *testing.T) {
        tests := []struct {
                in, want string
        }{
                {"", ""},
                {"a.com", "a.com"},
                {"a.com.", "a.com"},
                {"A.com", "a.com"},
                {"A.COM.", "a.com"},
                {"Example.ORG.", "example.org"},
        }
        for _, tc := range tests {
                assert.Equal(t, tc.want, normalizeDomain(tc.in), "normalizeDomain(%q)", tc.in)
        }
}

// TestDomainGraph_IsPrefetchCall verifies the context marker is set and
// detected correctly.
func TestDomainGraph_IsPrefetchCall(t *testing.T) {
        // Plain context — not a prefetch call.
        assert.False(t, isPrefetchCall(context.Background()))

        // Context with marker — is a prefetch call.
        ctx := context.WithValue(context.Background(), domainGraphKey{}, true)
        assert.True(t, isPrefetchCall(ctx))
}

// TestDomainGraph_GoroutineSafety is a smoke test that recordQuery and
// predictNeighbors can be called concurrently without panicking. Run
// with -race to verify no data races.
func TestDomainGraph_GoroutineSafety(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 256
        cfg.Service.DomainPrefetchMaxGraphSize = ptrInt(1000)
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(256)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)

        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()
        dg.start(ctx)

        done := make(chan struct{})

        // Writer 1: queries from client1.
        go func() {
                for i := 0; i < 500; i++ {
                        dg.recordQuery("client1", "host"+string(rune('a'+(i%10)))+".com")
                }
                close(done)
        }()

        // Writer 2: queries from client2.
        go func() {
                for i := 0; i < 500; i++ {
                        dg.recordQuery("client2", "host"+string(rune('a'+(i%10)))+".com")
                }
        }()

        // Reader: predict neighbors repeatedly.
        for i := 0; i < 200; i++ {
                dg.predictNeighbors("hosta.com")
        }

        <-done

        // Run a gcPass to make sure decay works under load.
        dg.gcPass()
}

// ptrInt helper for creating *int from int literal.
func ptrInt(v int) *int { return &v }

// fakeUpstreamConfig is a minimal UpstreamConfig for the prefetch test below.
func fakeUpstreamConfig() *ctrld.UpstreamConfig {
        return &ctrld.UpstreamConfig{
                Name:     "test",
                Type:     "legacy",
                Endpoint: "127.0.0.1:5353",
                Timeout:  1,
        }
}

// TestDomainGraph_MaybePrefetchNeighbors_DisabledGraph verifies the hook
// is a no-op when the graph is disabled.
func TestDomainGraph_MaybePrefetchNeighbors_DisabledGraph(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = false // disables domain graph too
        p := &prog{cfg: cfg}
        dg := newDomainGraph(p)

        // Build a minimal msg.
        msg := new(dns.Msg)
        msg.SetQuestion("a.com.", dns.TypeA)

        n := dg.maybePrefetchNeighbors(context.Background(), p, "10.0.0.1", "a.com.", dns.TypeA, "upstream.0", fakeUpstreamConfig())
        assert.Equal(t, 0, n, "disabled graph should not schedule any prefetches")
}

// TestDomainGraph_MaybePrefetchNeighbors_RecursionBlocked verifies that
// calling the hook with a context marked as a prefetch call is a no-op.
func TestDomainGraph_MaybePrefetchNeighbors_RecursionBlocked(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)
        dg.ctx = context.Background()

        // Build enough co-occurrences to trigger prediction under normal ctx.
        for i := 0; i < 10; i++ {
                dg.recordQuery("c1", "a.com")
                dg.recordQuery("c1", "b.com")
        }

        // Plain ctx — should schedule prefetches (the actual upstream will fail
        // since we're using a fake config, but the scheduling should happen).
        plainN := dg.maybePrefetchNeighbors(
                context.Background(), p, "c1", "a.com.", dns.TypeA,
                "upstream.0", fakeUpstreamConfig(),
        )
        assert.Greater(t, plainN, 0, "plain ctx should schedule prefetches")

        // Prefetch ctx — should be a no-op.
        prefetchCtx := context.WithValue(context.Background(), domainGraphKey{}, true)
        prefetchN := dg.maybePrefetchNeighbors(
                prefetchCtx, p, "c1", "a.com.", dns.TypeA,
                "upstream.0", fakeUpstreamConfig(),
        )
        assert.Equal(t, 0, prefetchN, "prefetch ctx should not schedule more prefetches")
}

// TestDomainGraph_MaybePrefetchNeighbors_NonAQuerySkipped verifies that
// non-A/AAAA queries don't trigger prefetching (CNAME, MX, etc. are
// typically singletons).
func TestDomainGraph_MaybePrefetchNeighbors_NonAQuerySkipped(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        dg := newDomainGraph(p)
        dg.ctx = context.Background()

        // Build co-occurrences.
        for i := 0; i < 10; i++ {
                dg.recordQuery("c1", "a.com")
                dg.recordQuery("c1", "b.com")
        }

        // Query with CNAME — should be a no-op.
        n := dg.maybePrefetchNeighbors(
                context.Background(), p, "c1", "a.com.", dns.TypeCNAME,
                "upstream.0", fakeUpstreamConfig(),
        )
        assert.Equal(t, 0, n, "CNAME queries should not trigger prefetching")
}
