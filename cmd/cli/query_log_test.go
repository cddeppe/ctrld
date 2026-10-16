package cli

import (
        "context"
        "os"
        "path/filepath"
        "testing"
        "time"

        "github.com/miekg/dns"
        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"

        "github.com/Control-D-Inc/ctrld/internal/dnscache"
        "github.com/Control-D-Inc/ctrld/testhelper"
)

// TestQueryLogger_DisabledByDefault verifies the logger is disabled
// when domain prefetch is off.
func TestQueryLogger_DisabledByDefault(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = false  // disables domain graph entirely
        p := &prog{cfg: cfg}
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        assert.False(t, ql.enabled, "should be disabled when domain prefetch is off")
}

// TestQueryLogger_EnabledByDefault verifies the logger is enabled when
// domain prefetch is on (default behavior — local log is the primary
// cold-start mechanism, works for everyone).
func TestQueryLogger_EnabledByDefault(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        assert.True(t, ql.enabled, "should be enabled by default when domain prefetch is on")
        assert.Equal(t, "/var/lib/ctrld/query-history.jsonl", ql.path)
        assert.Equal(t, int64(10*1024*1024), ql.maxSizeBytes)
}

// TestQueryLogger_ExplicitDisable verifies the explicit disable flag.
func TestQueryLogger_ExplicitDisable(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        disabled := false
        cfg.Service.DomainPrefetchLocalHistoryEnabled = &disabled
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        assert.False(t, ql.enabled, "should respect explicit disable")
}

// TestQueryLogger_AppendAndRead verifies the write + read cycle: write
// several queries, then replay them and confirm the graph gets populated.
func TestQueryLogger_AppendAndRead(t *testing.T) {
        tmpDir := t.TempDir()
        logPath := filepath.Join(tmpDir, "queries.jsonl")

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        logPathConfig := logPath
        cfg.Service.DomainPrefetchLocalHistoryPath = &logPathConfig
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        require.NoError(t, ql.start())
        defer ql.close()

        // Write 6 queries: 3 pairs of (a, b) from the same client.
        for i := 0; i < 3; i++ {
                ql.appendQuery("10.0.0.1", "a.com", dns.TypeA)
                ql.appendQuery("10.0.0.1", "b.com", dns.TypeA)
        }
        ql.flush()

        // Verify the file exists and has 6 lines.
        data, err := os.ReadFile(logPath)
        require.NoError(t, err)
        lines := splitLines(string(data))
        assert.Equal(t, 6, len(lines), "should have written 6 query lines")

        // Now replay into a fresh graph and verify it gets populated.
        ql.replayInto(p.domainGraph, p)

        // Graph should have a.com <-> b.com with weight >= 9 (3 pairs × 3 co-occurrences each × 3.0 weight = 9).
        neighbors := p.domainGraph.predictNeighbors("a.com")
        assert.NotEmpty(t, neighbors, "should have predicted neighbors for a.com after replay")
        assert.Contains(t, neighbors, "b.com", "b.com should be a predicted neighbor of a.com")
}

// TestQueryLogger_AppendQuery_FiltersNonAAAA verifies only A and AAAA
// queries are logged (CNAME, MX, etc. are skipped).
func TestQueryLogger_AppendQuery_FiltersNonAAAA(t *testing.T) {
        tmpDir := t.TempDir()
        logPath := filepath.Join(tmpDir, "queries.jsonl")

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        logPathConfig := logPath
        cfg.Service.DomainPrefetchLocalHistoryPath = &logPathConfig
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        require.NoError(t, ql.start())
        defer ql.close()

        // Write mixed query types.
        ql.appendQuery("10.0.0.1", "a.com", dns.TypeA)
        ql.appendQuery("10.0.0.1", "b.com", dns.TypeAAAA)
        ql.appendQuery("10.0.0.1", "c.com", dns.TypeCNAME)  // should be skipped
        ql.appendQuery("10.0.0.1", "d.com", dns.TypeMX)      // should be skipped
        ql.appendQuery("10.0.0.1", "e.com", dns.TypeTXT)     // should be skipped
        ql.flush()

        data, _ := os.ReadFile(logPath)
        lines := splitLines(string(data))
        assert.Equal(t, 2, len(lines), "only A and AAAA queries should be logged")
}

// TestQueryLogger_ReplayInto_FileDoesNotExist verifies that replaying
// from a non-existent file is a no-op (no error, no panic, graph stays empty).
func TestQueryLogger_ReplayInto_FileDoesNotExist(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        nonexistentPath := "/tmp/nonexistent-query-log-" + time.Now().Format("150405") + ".jsonl"
        cfg.Service.DomainPrefetchLocalHistoryPath = &nonexistentPath
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)

        // Should not panic, should not error visibly.
        assert.NotPanics(t, func() {
                ql.replayInto(p.domainGraph, p)
        })
        // Graph should be empty.
        assert.Equal(t, 0, p.domainGraph.domainCount(), "graph should be empty when log file doesn't exist")
}

// TestQueryLogger_ReplayInto_LargeFileTruncation verifies that when the
// log file exceeds max_size_bytes, only the recent entries are replayed,
// and the file is rewritten to fit.
func TestQueryLogger_ReplayInto_LargeFileTruncation(t *testing.T) {
        tmpDir := t.TempDir()
        logPath := filepath.Join(tmpDir, "queries.jsonl")

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        logPathConfig := logPath
        cfg.Service.DomainPrefetchLocalHistoryPath = &logPathConfig
        // Set a tiny max size — 1KB — so we can easily exceed it.
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        ql.maxSizeBytes = 200  // 200 bytes — easily exceeded

        require.NoError(t, ql.start())

        // Write many queries to exceed 200 bytes. Alternate two domains so
        // the recent entries (after truncation) have co-occurrences.
        for i := 0; i < 50; i++ {
                ql.appendQuery("10.0.0.1", "a.com", dns.TypeA)
                ql.appendQuery("10.0.0.1", "b.com", dns.TypeA)
        }
        ql.flush()
        ql.close()

        // Verify file is large.
        stat, _ := os.Stat(logPath)
        assert.Greater(t, stat.Size(), ql.maxSizeBytes, "file should exceed max size before replay")

        // Replay — should only replay recent entries and truncate the file.
        ql.replayInto(p.domainGraph, p)

        // After truncation, file should be <= max_size_bytes.
        stat, _ = os.Stat(logPath)
        assert.LessOrEqual(t, stat.Size(), ql.maxSizeBytes,
                "file should be truncated to <= max size after replay")

        // Graph should have at least some domains (from the recent entries).
        assert.Greater(t, p.domainGraph.domainCount(), 0,
                "graph should have domains after replaying recent entries")
}

// TestQueryLogger_ReplayInto_PreservesOrder verifies that queries are
// replayed in file order (oldest first), which produces correct
// co-occurrence edges (the recordQueryWithWeight logic depends on
// chronological order).
func TestQueryLogger_ReplayInto_PreservesOrder(t *testing.T) {
        tmpDir := t.TempDir()
        logPath := filepath.Join(tmpDir, "queries.jsonl")

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        logPathConfig := logPath
        cfg.Service.DomainPrefetchLocalHistoryPath = &logPathConfig
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        require.NoError(t, ql.start())

        // Write queries in a specific order: a, b, c, d.
        // The graph should reflect a->b, b->c, c->d co-occurrences (in order).
        ql.appendQuery("10.0.0.1", "a.com", dns.TypeA)
        ql.appendQuery("10.0.0.1", "b.com", dns.TypeA)
        ql.appendQuery("10.0.0.1", "c.com", dns.TypeA)
        ql.appendQuery("10.0.0.1", "d.com", dns.TypeA)
        ql.flush()
        ql.close()

        ql.replayInto(p.domainGraph, p)

        // All 4 domains should be in the graph (each co-occurs with at least one other).
        p.domainGraph.mu.RLock()
        defer p.domainGraph.mu.RUnlock()
        for _, domain := range []string{"a.com", "b.com", "c.com", "d.com"} {
                _, ok := p.domainGraph.coOccur[domain]
                assert.True(t, ok, "%s should be in the graph after replay", domain)
        }
}

// TestQueryLogger_ConcurrentAppends verifies that concurrent writes
// don't cause races or panics. Run with -race to verify.
func TestQueryLogger_ConcurrentAppends(t *testing.T) {
        tmpDir := t.TempDir()
        logPath := filepath.Join(tmpDir, "queries.jsonl")

        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        logPathConfig := logPath
        cfg.Service.DomainPrefetchLocalHistoryPath = &logPathConfig
        p := &prog{cfg: cfg}
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher
        p.um = newUpstreamMonitor(cfg)
        p.domainGraph = newDomainGraph(p)
        ql := newQueryLogger(p)
        require.NoError(t, ql.start())
        defer ql.close()

        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        // Spawn 10 goroutines, each writing 100 queries.
        done := make(chan struct{})
        for i := 0; i < 10; i++ {
                go func(n int) {
                        for j := 0; j < 100; j++ {
                                ql.appendQuery("10.0.0.1", "host.com", dns.TypeA)
                        }
                        done <- struct{}{}
                }(i)
        }

        // Also have a reader goroutine that calls flush periodically.
        go func() {
                for {
                        select {
                        case <-ctx.Done():
                                return
                        default:
                                ql.flush()
                                time.Sleep(time.Millisecond)
                        }
                }
        }()

        // Wait for all writers.
        for i := 0; i < 10; i++ {
                <-done
        }
        ql.flush()

        // Verify the file has all 1000 queries.
        data, err := os.ReadFile(logPath)
        require.NoError(t, err)
        lines := splitLines(string(data))
        assert.Equal(t, 1000, len(lines), "should have 1000 query lines (10 goroutines × 100 each)")
}

// splitLines is a small helper that splits a string by newlines and
// returns non-empty lines.
func splitLines(s string) []string {
        var out []string
        start := 0
        for i := 0; i < len(s); i++ {
                if s[i] == '\n' {
                        if i > start {
                                out = append(out, s[start:i])
                        }
                        start = i + 1
                }
        }
        if start < len(s) {
                out = append(out, s[start:])
        }
        return out
}
