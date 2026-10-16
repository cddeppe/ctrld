package cli

import (
        "context"
        "encoding/json"
        "net/http"
        "net/http/httptest"
        "os"
        "testing"
        "time"

        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"

        "github.com/Control-D-Inc/ctrld/internal/dnscache"
        "github.com/Control-D-Inc/ctrld/testhelper"
)

// TestAPIHistoryConfig_DisabledByDefault verifies the config is disabled
// when not explicitly enabled, or when no token / no device ID is set.
func TestAPIHistoryConfig_DisabledByDefault(t *testing.T) {
        t.Run("feature flag off", func(t *testing.T) {
                // Build a prog with domain prefetch enabled but API history off.
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                featureOff := false
                cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOff
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                p.domainGraph = newDomainGraph(p)
                apiCfg := newAPIHistoryConfig(p)
                assert.False(t, apiCfg.enabled, "should be disabled when feature flag is off")
        })

        t.Run("no token", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                featureOn := true
                cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
                // No CONTROLD_API_TOKEN env var set in tests.
                os.Unsetenv("CONTROLD_API_TOKEN")
                os.Unsetenv("CONTROLD_API_TOKEN_READ")
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                p.domainGraph = newDomainGraph(p)
                apiCfg := newAPIHistoryConfig(p)
                assert.False(t, apiCfg.enabled, "should be disabled when no token")
        })

        t.Run("no device ID", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                featureOn := true
                cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
                t.Setenv("CONTROLD_API_TOKEN", "test-token")
                // No device ID set in config, and p.rc is nil.
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                p.domainGraph = newDomainGraph(p)
                // p.rc is nil (no --cd flag), no explicit device ID — should disable.
                apiCfg := newAPIHistoryConfig(p)
                assert.False(t, apiCfg.enabled, "should be disabled when no device ID")
        })

        t.Run("fully configured", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                featureOn := true
                cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
                deviceID := "test-device-123"
                cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
                limit := 5000
                cfg.Service.DomainPrefetchAPIHistoryLimit = &limit
                t.Setenv("CONTROLD_API_TOKEN", "test-token")
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                p.domainGraph = newDomainGraph(p)
                apiCfg := newAPIHistoryConfig(p)
                assert.True(t, apiCfg.enabled, "should be enabled when all config present")
                assert.Equal(t, "test-token", apiCfg.token)
                assert.Equal(t, "test-device-123", apiCfg.deviceID)
                assert.Equal(t, 5000, apiCfg.limit)
        })

        t.Run("token from CONTROLD_API_TOKEN_READ env", func(t *testing.T) {
                cfg := testhelper.SampleConfig(t)
                cfg.Service.CacheEnable = true
                cfg.Service.CacheSize = 64
                featureOn := true
                cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
                deviceID := "test-device"
                cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
                t.Setenv("CONTROLD_API_TOKEN_READ", "read-token")
                os.Unsetenv("CONTROLD_API_TOKEN")
                p := &prog{cfg: cfg}
                cacher, _ := dnscache.NewLRUCache(64)
                p.cache = cacher
                p.um = newUpstreamMonitor(cfg)
                p.domainGraph = newDomainGraph(p)
                apiCfg := newAPIHistoryConfig(p)
                assert.True(t, apiCfg.enabled)
                assert.Equal(t, "read-token", apiCfg.token, "should fall back to CONTROLD_API_TOKEN_READ env var")
        })
}

// TestAPIHistoryQuery_FieldParsing verifies the defensive field parser
// handles all the documented field name variants.
func TestAPIHistoryQuery_FieldParsing(t *testing.T) {
        t.Run("canonical field names", func(t *testing.T) {
                q := apiHistoryQuery{
                        Domain:    "google.com",
                        Timestamp: "2026-10-15T12:00:00Z",
                        ClientIP:  "192.168.1.1",
                        Qtype:     "A",
                }
                assert.Equal(t, "192.168.1.1", q.parseClientIP())
                assert.Equal(t, "2026-10-15T12:00:00Z", q.parseTimestamp())
                assert.Equal(t, "A", q.parseQtype())
        })

        t.Run("alternative field names", func(t *testing.T) {
                q := apiHistoryQuery{
                        Domain:  "google.com",
                        Time:    "2026-10-15T12:00:00Z",
                        SrcIP:   "10.0.0.1",
                        Type:    "AAAA",
                }
                assert.Equal(t, "10.0.0.1", q.parseClientIP())
                assert.Equal(t, "2026-10-15T12:00:00Z", q.parseTimestamp())
                assert.Equal(t, "AAAA", q.parseQtype())
        })

        t.Run("all fields missing — defaults", func(t *testing.T) {
                q := apiHistoryQuery{Domain: "google.com"}
                assert.Equal(t, "anonymous", q.parseClientIP(), "no client IP → 'anonymous' bucket")
                assert.Equal(t, "", q.parseTimestamp(), "no timestamp → empty (assume pre-sorted)")
                assert.Equal(t, "A", q.parseQtype(), "no qtype → default to A")
        })
}

// TestFetchAndReplayHistory_HappyPath uses a mock HTTP server that returns
// a realistic queries array and verifies the graph gets populated.
func TestFetchAndReplayHistory_HappyPath(t *testing.T) {
        // Mock server returning 5 queries with co-occurrence patterns.
        // Pattern: client A queries [google.com, mail.google.com] in sequence, 3 times.
        // Expected graph state: google.com <-> mail.google.com with weight >= 5 (threshold).
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                // Verify auth header.
                assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
                assert.Equal(t, "test-device-xyz", r.URL.Query().Get("device_id"))
                assert.Equal(t, "10000", r.URL.Query().Get("limit"))

                queries := []apiHistoryQuery{
                        {Domain: "google.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:00Z", Qtype: "A"},
                        {Domain: "mail.google.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:01Z", Qtype: "A"},
                        {Domain: "google.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:01:00Z", Qtype: "A"},
                        {Domain: "mail.google.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:01:01Z", Qtype: "A"},
                        {Domain: "google.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:02:00Z", Qtype: "A"},
                        {Domain: "mail.google.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:02:01Z", Qtype: "A"},
                        // Some skipped queries:
                        {Domain: "", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:03:00Z", Qtype: "A"},            // empty domain, skipped
                        {Domain: "example.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:03:01Z", Qtype: "MX"}, // non-A/AAAA, skipped
                }
                resp := apiHistoryResponse{}
                resp.Body.Queries = queries
                _ = json.NewEncoder(w).Encode(resp)
        }))
        defer server.Close()

        // Build a prog with the feature enabled.
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        featureOn := true
        cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
        deviceID := "test-device-xyz"
        cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
        t.Setenv("CONTROLD_API_TOKEN", "test-token")

        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        p.domainGraph.ctx = context.Background()

        // Override the API URL by replacing fetchAndReplayHistory's URL construction.
        // We do this by setting a test-only override via env var.
        t.Setenv("CONTROLD_API_URL_OVERRIDE", server.URL)

        // Call the fetcher synchronously (not via the goroutine launcher).
        p.domainGraph.fetchAndReplayHistory(context.Background(), p)

        // Verify the graph has the expected edges.
        // After 3 pairs of (google.com, mail.google.com), we should have a co-occurrence
        // edge weight >= 5 (which is minCoOccur default, so predictNeighbors should fire).
        neighbors := p.domainGraph.predictNeighbors("google.com")
        require.NotEmpty(t, neighbors, "should have a predicted neighbor for google.com")
        assert.Contains(t, neighbors, "mail.google.com", "should predict mail.google.com as a neighbor")

        // Verify the counter was incremented.
        assert.Greater(t, apiHistoryReplays.Load(), int64(0), "apiHistoryReplays counter should be > 0")
}

// TestFetchAndReplayHistory_AuthFailure verifies that a 401 response
// doesn't crash and logs an error.
func TestFetchAndReplayHistory_AuthFailure(t *testing.T) {
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.WriteHeader(http.StatusUnauthorized)
                _, _ = w.Write([]byte(`{"error": "invalid token"}`))
        }))
        defer server.Close()

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        featureOn := true
        cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
        deviceID := "test-device"
        cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
        t.Setenv("CONTROLD_API_TOKEN", "bad-token")
        t.Setenv("CONTROLD_API_URL_OVERRIDE", server.URL)

        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)

        before := apiHistoryReplays.Load()
        // Should not panic, should not modify the graph.
        assert.NotPanics(t, func() {
                p.domainGraph.fetchAndReplayHistory(context.Background(), p)
        })
        // Counter should NOT have changed (no replays on auth failure).
        assert.Equal(t, before, apiHistoryReplays.Load())
        // Graph should be empty (no replays happened).
        assert.Equal(t, 0, p.domainGraph.domainCount())
}

// TestFetchAndReplayHistory_RateLimited verifies that a 429 response
// triggers a retry, and if the retry succeeds, the graph gets populated.
// Skipped in short mode because it takes 60+ seconds (the retry delay).
func TestFetchAndReplayHistory_RateLimited(t *testing.T) {
        if testing.Short() {
                t.Skip("skipping rate-limit test in short mode (takes 60+s for retry delay)")
        }
        callCount := 0
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                callCount++
                if callCount == 1 {
                        w.WriteHeader(http.StatusTooManyRequests)
                        return
                }
                // On retry, return a minimal valid response.
                resp := apiHistoryResponse{}
                resp.Body.Queries = []apiHistoryQuery{
                        {Domain: "example.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:00Z", Qtype: "A"},
                        {Domain: "test.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:01Z", Qtype: "A"},
                }
                _ = json.NewEncoder(w).Encode(resp)
        }))
        defer server.Close()

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        featureOn := true
        cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
        deviceID := "test-device"
        cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
        t.Setenv("CONTROLD_API_TOKEN", "test-token")
        t.Setenv("CONTROLD_API_URL_OVERRIDE", server.URL)

        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)

        // Use a very short retry delay so the test doesn't take 60s.
        // We can't easily override the constant, so we accept the test will
        // be slow OR we monkey-patch differently. For now, skip if we can't
        // shorten — set a deadline.
        ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
        defer cancel()

        p.domainGraph.fetchAndReplayHistory(ctx, p)

        // Should have called the server twice (initial 429, then retry success).
        assert.Equal(t, 2, callCount, "should have retried after 429")
        // Graph should be populated (1 replay since only 2 queries with one pair).
        assert.Equal(t, 2, p.domainGraph.domainCount(), "graph should have 2 domains (example.com + test.com)")
}

// TestFetchAndReplayHistory_NetworkError verifies a network failure
// doesn't crash and leaves the graph empty.
func TestFetchAndReplayHistory_NetworkError(t *testing.T) {
        // Point at a port that's not listening.
        t.Setenv("CONTROLD_API_URL_OVERRIDE", "http://127.0.0.1:1")

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        featureOn := true
        cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
        deviceID := "test-device"
        cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
        t.Setenv("CONTROLD_API_TOKEN", "test-token")

        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)

        assert.NotPanics(t, func() {
                p.domainGraph.fetchAndReplayHistory(context.Background(), p)
        })
        assert.Equal(t, 0, p.domainGraph.domainCount(), "graph should be empty on network error")
}

// TestSortByTimestampOldestFirst verifies queries are sorted chronologically.
func TestSortByTimestampOldestFirst(t *testing.T) {
        queries := []apiHistoryQuery{
                {Domain: "c.com", Timestamp: "2026-10-15T12:03:00Z"},
                {Domain: "a.com", Timestamp: "2026-10-15T12:00:00Z"},
                {Domain: "b.com", Timestamp: "2026-10-15T12:01:00Z"},
                {Domain: "d.com", Timestamp: ""}, // empty goes to end
        }
        sortByTimestampOldestFirst(queries)

        assert.Equal(t, "a.com", queries[0].Domain)
        assert.Equal(t, "b.com", queries[1].Domain)
        assert.Equal(t, "c.com", queries[2].Domain)
        assert.Equal(t, "d.com", queries[3].Domain, "empty timestamp should sort to end")
}

// TestFetchAndReplayHistory_EmptyResponse verifies that an empty queries
// array is handled gracefully (no replays, no crash).
func TestFetchAndReplayHistory_EmptyResponse(t *testing.T) {
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                _ = json.NewEncoder(w).Encode(apiHistoryResponse{})
        }))
        defer server.Close()

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        featureOn := true
        cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
        deviceID := "test-device"
        cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
        t.Setenv("CONTROLD_API_TOKEN", "test-token")
        t.Setenv("CONTROLD_API_URL_OVERRIDE", server.URL)

        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)

        assert.NotPanics(t, func() {
                p.domainGraph.fetchAndReplayHistory(context.Background(), p)
        })
        assert.Equal(t, 0, p.domainGraph.domainCount())
}

// TestFetchAndReplayHistory_NonGETSkipped verifies that non-A/AAAA query
// types (CNAME, MX, TXT, SRV) are filtered out during replay — same as
// the live hook's behavior.
func TestFetchAndReplayHistory_NonGETSkipped(t *testing.T) {
        server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                // 2 A queries (a.com + a2.com) + 4 non-A/AAAA queries (CNAME/MX/TXT/SRV).
                // After replay, only a.com <-> a2.com should have a co-occurrence edge.
                queries := []apiHistoryQuery{
                        {Domain: "a.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:00Z", Qtype: "A"},
                        {Domain: "b.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:01Z", Qtype: "CNAME"},
                        {Domain: "c.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:02Z", Qtype: "MX"},
                        {Domain: "d.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:03Z", Qtype: "TXT"},
                        {Domain: "e.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:04Z", Qtype: "SRV"},
                        {Domain: "a2.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:05Z", Qtype: "A"},
                }
                resp := apiHistoryResponse{}
                resp.Body.Queries = queries
                _ = json.NewEncoder(w).Encode(resp)
        }))
        defer server.Close()

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        featureOn := true
        cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
        deviceID := "test-device"
        cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
        t.Setenv("CONTROLD_API_TOKEN", "test-token")
        t.Setenv("CONTROLD_API_URL_OVERRIDE", server.URL)

        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)

        p.domainGraph.fetchAndReplayHistory(context.Background(), p)

        // Only a.com and a2.com (both A) should be in the graph with edges.
        // b.com (CNAME), c.com (MX), d.com (TXT), e.com (SRV) all skipped.
        assert.Equal(t, 2, p.domainGraph.domainCount(), "only A queries should be replayed (2 A queries → 2 domains with edges)")

        p.domainGraph.mu.RLock()
        defer p.domainGraph.mu.RUnlock()
        _, okA := p.domainGraph.coOccur["a.com"]
        assert.True(t, okA, "a.com should be in the graph")
        _, okA2 := p.domainGraph.coOccur["a2.com"]
        assert.True(t, okA2, "a2.com should be in the graph")
        // Verify a.com <-> a2.com edge exists.
        assert.Contains(t, p.domainGraph.coOccur["a.com"], "a2.com", "a.com should have edge to a2.com")
        assert.Contains(t, p.domainGraph.coOccur["a2.com"], "a.com", "a2.com should have edge to a.com (bidirectional)")
        // Verify skipped queries are NOT in the graph.
        for _, skipped := range []string{"b.com", "c.com", "d.com", "e.com"} {
                _, ok := p.domainGraph.coOccur[skipped]
                assert.False(t, ok, "%s should NOT be in the graph (skipped)", skipped)
        }
}

// stub: ensure unused imports are referenced
var _ = require.Equal
