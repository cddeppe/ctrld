package cli

import (
        "context"
        "testing"
        "time"

        "github.com/miekg/dns"
        "github.com/stretchr/testify/assert"

        "github.com/Control-D-Inc/ctrld"
        "github.com/Control-D-Inc/ctrld/internal/dnscache"
        "github.com/Control-D-Inc/ctrld/testhelper"
)

// TestPrefetcherGating verifies the decision tree in maybePrefetch without
// requiring a real upstream. The refreshes counter is incremented
// synchronously before the background goroutine fires, so we can check the
// gating decision by reading the counter immediately after the call.
func TestPrefetcherGating(t *testing.T) {
        // Build a base msg for A queries.
        msgA := new(dns.Msg)
        msgA.SetQuestion("example.com.", dns.TypeA)

        // PTR is excluded (caller already gates via Qtype != dns.TypePTR, but
        // maybePrefetch also defends against it for safety).
        msgPTR := new(dns.Msg)
        msgPTR.SetQuestion("1.0.0.10.in-addr.arpa.", dns.TypePTR)

        // Set up a real-ish UpstreamConfig. NewResolver succeeds for "legacy"
        // type with any endpoint. Resolve will fail (no real server), but that's
        // fine — we only test the gating decision, not the refresh outcome.
        cfg := testhelper.SampleConfig(t)
        uc := &ctrld.UpstreamConfig{
                Name:     "test",
                Type:     "legacy",
                Endpoint: "127.0.0.1:5353",
                Timeout:  1,
        }
        // Force UID to be set so singleflight key is stable.
        _ = uc.UID()

        p := &prog{cfg: cfg}
        cacher, err := dnscache.NewLRUCache(64)
        if err != nil {
                t.Fatal(err)
        }
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)

        tests := []struct {
                name          string
                enabled       bool
                threshold     float64
                minTTL        time.Duration
                msg           *dns.Msg
                originalTTL   uint32
                expireOffset  time.Duration // cachedExpire = now + offset; negative = expired
                upstream      string
                upstreamDown  bool
                wantScheduled bool
        }{
                {
                        name: "disabled by config",
                        enabled: false, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 60, expireOffset: 5 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: false,
                },
                {
                        name: "fresh hit well above threshold (50% remaining, threshold 25%)",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 60, expireOffset: 30 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: false,
                },
                {
                        name: "fresh hit within threshold (16% remaining, threshold 25%)",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 60, expireOffset: 10 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: true,
                },
                {
                        name: "exactly at threshold (25% remaining)",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 60, expireOffset: 15 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: true, // remaining (15s) is NOT > threshold*TTL (15s), so trigger
                },
                {
                        name: "expired entry (negative remaining)",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 60, expireOffset: -5 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: true,
                },
                {
                        name: "TTL below minTTL (TTL=5, minTTL=10)",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 5, expireOffset: 1 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: false,
                },
                {
                        name: "PTR query — excluded",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgPTR, originalTTL: 60, expireOffset: 1 * time.Second,
                        upstream:      "upstream.0",
                        wantScheduled: false,
                },
                {
                        name: "upstream marked down — skipped",
                        enabled: true, threshold: 0.25, minTTL: 10 * time.Second,
                        msg: msgA, originalTTL: 60, expireOffset: 1 * time.Second,
                        upstream: "upstream.0", upstreamDown: true,
                        wantScheduled: false,
                },
        }

        for _, tc := range tests {
                t.Run(tc.name, func(t *testing.T) {
                        // Build a fresh prefetcher for each case.
                        pf := &prefetcher{
                                enabled:   tc.enabled,
                                threshold: tc.threshold,
                                minTTL:    tc.minTTL,
                                jitter:    0, // no jitter so the goroutine fires immediately
                                cache:     p.cache,
                                um:        p.um,
                                ctx:       context.Background(),
                        }
                        // Reset the per-prefetcher counter to isolate this case.
                        // statsCachePrefetchRefreshes is a package-level prometheus.Counter
                        // (no Reset method), so we can't reset it — we use the per-instance
                        // counter (pf.refreshes) for assertions instead.
                        pf.refreshes.Store(0)

                        if tc.upstreamDown {
                                pf.um.mu.Lock()
                                pf.um.down[tc.upstream] = true
                                pf.um.mu.Unlock()
                                defer func() {
                                        pf.um.mu.Lock()
                                        pf.um.down[tc.upstream] = false
                                        pf.um.mu.Unlock()
                                }()
                        }

                        cachedExpire := time.Now().Add(tc.expireOffset)
                        got := pf.maybePrefetch(p, tc.msg, tc.upstream, uc, cachedExpire, tc.originalTTL)

                        // maybePrefetch returns the same value as "was a refresh scheduled".
                        // The internal refreshes counter is the source of truth.
                        gotCounter := pf.refreshes.Load()

                        if got != tc.wantScheduled {
                                t.Errorf("maybePrefetch returned %v, want %v", got, tc.wantScheduled)
                        }
                        if gotCounter != pf.refreshes.Load() {
                                // sanity: counter shouldn't change between calls
                        }
                        if (gotCounter > 0) != tc.wantScheduled {
                                t.Errorf("refreshes counter = %d, want scheduled=%v", gotCounter, tc.wantScheduled)
                        }

                        // If a refresh was scheduled, give the goroutine a moment to
                        // run (it will fail to resolve and log a debug message; that's
                        // fine — we only care that the gating decision was correct).
                        if tc.wantScheduled {
                                time.Sleep(50 * time.Millisecond)
                        }
                })
        }
}

// TestPrefetcherDisabledByDefault verifies that newPrefetcher returns a
// disabled prefetcher when CacheEnable is false, and an enabled one (with
// default threshold=0.25, minTTL=10s) when CacheEnable is true with no
// explicit prefetch config.
func TestPrefetcherDisabledByDefault(t *testing.T) {
        t.Run("cache disabled", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = false
                p := &prog{cfg: cfg}
                pf := newPrefetcher(p)
                assert.False(t, pf.enabled, "prefetcher should be disabled when CacheEnable is false")
        })

        t.Run("cache enabled, no explicit prefetch config — defaults applied", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                p := &prog{cfg: cfg}
                cacher, err := dnscache.NewLRUCache(64)
                if err != nil {
                        t.Fatal(err)
                }
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                pf := newPrefetcher(p)
                assert.True(t, pf.enabled, "prefetcher should be enabled when CacheEnable is true")
                assert.Equal(t, 0.25, pf.threshold, "default threshold should be 0.25")
                assert.Equal(t, 10*time.Second, pf.minTTL, "default minTTL should be 10s")
        })

        t.Run("explicit threshold=0 disables prefetch", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                zero := 0.0
                cfg.Service.CachePrefetchThreshold = &zero
                p := &prog{cfg: cfg}
                cacher, err := dnscache.NewLRUCache(64)
                if err != nil {
                        t.Fatal(err)
                }
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                pf := newPrefetcher(p)
                assert.False(t, pf.enabled, "prefetcher should be disabled when threshold is explicitly 0")
        })

        t.Run("custom threshold and minTTL", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                thresh := 0.5
                cfg.Service.CachePrefetchThreshold = &thresh
                minTTL := 30
                cfg.Service.CachePrefetchMinTTL = &minTTL
                p := &prog{cfg: cfg}
                cacher, err := dnscache.NewLRUCache(64)
                if err != nil {
                        t.Fatal(err)
                }
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                pf := newPrefetcher(p)
                assert.True(t, pf.enabled)
                assert.Equal(t, 0.5, pf.threshold)
                assert.Equal(t, 30*time.Second, pf.minTTL)
        })
}
