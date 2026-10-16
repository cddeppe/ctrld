package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Control-D-Inc/ctrld/internal/dnscache"
	"github.com/Control-D-Inc/ctrld/testhelper"
)

// TestRecordQueryWithWeight_MultiplierApplied verifies that the weight
// multiplier is correctly applied to edge increments. Two queries with
// weight=3.0 should produce edges with weight 3.0 (vs. weight=1.0 from
// default recordQuery).
func TestRecordQueryWithWeight_MultiplierApplied(t *testing.T) {
	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	dg := newDomainGraph(p)
	dg.historyWeight = 3.0

	// Two queries from the same client — first establishes edge, second
	// also adds to it (since the prior is in the window).
	dg.recordQueryWithWeight("10.0.0.1", "a.com", 3.0)
	dg.recordQueryWithWeight("10.0.0.1", "b.com", 3.0)

	// a→b should have weight 3.0 (one co-occurrence × weight 3.0).
	// b→a should also have weight 3.0 (bidirectional).
	dg.mu.RLock()
	defer dg.mu.RUnlock()
	assert.Equal(t, float64(3.0), dg.coOccur["a.com"]["b.com"], "weight should be 3.0 (1 co-occurrence × 3.0 multiplier)")
	assert.Equal(t, float64(3.0), dg.coOccur["b.com"]["a.com"], "reverse edge should also be 3.0")
}

// TestRecordQueryWithWeight_DefaultLiveWeightIsOne verifies that the
// plain recordQuery wrapper still uses weight=1.0 (no regression).
func TestRecordQueryWithWeight_DefaultLiveWeightIsOne(t *testing.T) {
	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	dg := newDomainGraph(p)

	dg.recordQuery("10.0.0.1", "a.com")
	dg.recordQuery("10.0.0.1", "b.com")

	dg.mu.RLock()
	defer dg.mu.RUnlock()
	assert.Equal(t, float64(1.0), dg.coOccur["a.com"]["b.com"], "default recordQuery should use weight 1.0")
}

// TestRecordQueryWithWeight_ZeroOrNegativeIsNoop verifies that weight <= 0
// is a no-op (defensive — avoids negative or zero edges).
func TestRecordQueryWithWeight_ZeroOrNegativeIsNoop(t *testing.T) {
	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	dg := newDomainGraph(p)

	dg.recordQueryWithWeight("10.0.0.1", "a.com", 0)
	dg.recordQueryWithWeight("10.0.0.1", "b.com", -1)

	assert.Equal(t, 0, dg.domainCount(), "no edges should be created with weight <= 0")
}

// TestFetchAndReplayHistory_HistoryWeightApplied verifies that the
// historyWeight multiplier is actually used during replay. After replay,
// edge weights should reflect the multiplier (e.g. weight 3.0 means
// a single co-occurrence produces a 3.0 edge, not 1.0).
func TestFetchAndReplayHistory_HistoryWeightApplied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries := []apiHistoryQuery{
			{Domain: "a.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:00Z", Qtype: "A"},
			{Domain: "b.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:01Z", Qtype: "A"},
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

	// Override historyWeight to 5.0 to verify it's applied.
	hw := 5.0
	cfg.Service.DomainPrefetchHistoryWeight = &hw

	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	p.domainGraph = newDomainGraph(p)

	p.domainGraph.fetchAndReplayHistory(context.Background(), p)

	p.domainGraph.mu.RLock()
	defer p.domainGraph.mu.RUnlock()
	assert.Equal(t, float64(5.0), p.domainGraph.coOccur["a.com"]["b.com"],
		"replay should use historyWeight=5.0 (not default 1.0)")
}

// TestNewAPIHistoryConfig_TokenFromConfigField verifies that the API token
// can come from the config field (not just env vars).
func TestNewAPIHistoryConfig_TokenFromConfigField(t *testing.T) {
	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	featureOn := true
	cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
	deviceID := "test-device"
	cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
	token := "cd_config_field_token"
	cfg.Service.DomainPrefetchAPIToken = &token
	// No env vars set.
	t.Setenv("CONTROLD_API_TOKEN", "")
	t.Setenv("CONTROLD_API_TOKEN_READ", "")

	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	p.domainGraph = newDomainGraph(p)

	apiCfg := newAPIHistoryConfig(p)
	assert.True(t, apiCfg.enabled, "should be enabled when token comes from config field")
	assert.Equal(t, "cd_config_field_token", apiCfg.token, "should read token from config field")
}

// TestNewAPIHistoryConfig_EnvVarOverridesConfigField verifies that the
// env var takes precedence over the config field when both are set.
// This is the documented behavior — env vars are preferred for secrets.
func TestNewAPIHistoryConfig_EnvVarOverridesConfigField(t *testing.T) {
	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	featureOn := true
	cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
	deviceID := "test-device"
	cfg.Service.DomainPrefetchAPIHistoryDeviceID = &deviceID
	token := "config-field-token"
	cfg.Service.DomainPrefetchAPIToken = &token
	t.Setenv("CONTROLD_API_TOKEN", "env-var-token")

	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	p.domainGraph = newDomainGraph(p)

	apiCfg := newAPIHistoryConfig(p)
	assert.True(t, apiCfg.enabled)
	assert.Equal(t, "env-var-token", apiCfg.token, "env var should take precedence over config field")
}

// TestLogTopTriggers verifies that the trigger-domain logging works
// correctly. We can't easily capture log output, so we verify the
// method doesn't panic and completes — the log output is observable
// via ctrld's actual logs in production.
func TestLogTopTriggers(t *testing.T) {
	t.Run("empty graph — no panic", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = true
		cfg.Service.CacheSize = 64
		p := &prog{cfg: cfg}
		cacher, _ := dnscache.NewLRUCache(64)
		p.cache = cacher
		p.um = newUpstreamMonitor(cfg)
		dg := newDomainGraph(p)

		assert.NotPanics(t, func() {
			dg.logTopTriggers()
		})
	})

	t.Run("graph with triggers — no panic", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = true
		cfg.Service.CacheSize = 64
		p := &prog{cfg: cfg}
		cacher, _ := dnscache.NewLRUCache(64)
		p.cache = cacher
		p.um = newUpstreamMonitor(cfg)
		dg := newDomainGraph(p)

		// Build a graph with multiple trigger domains.
		// a.com co-occurs with b.com, c.com, d.com (3 neighbors — qualifies as trigger).
		dg.mu.Lock()
		dg.addEdgeLocked("a.com", "b.com", 10)
		dg.addEdgeLocked("a.com", "c.com", 8)
		dg.addEdgeLocked("a.com", "d.com", 6)
		dg.addEdgeLocked("b.com", "a.com", 10)
		dg.addEdgeLocked("c.com", "a.com", 8)
		dg.addEdgeLocked("d.com", "a.com", 6)
		// e.com co-occurs with only f.com — doesn't qualify as trigger (needs >= 2).
		dg.addEdgeLocked("e.com", "f.com", 5)
		dg.addEdgeLocked("f.com", "e.com", 5)
		dg.mu.Unlock()

		assert.NotPanics(t, func() {
			dg.logTopTriggers()
		})
	})

	t.Run("disabled graph — no panic", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = false // disables domain graph entirely
		p := &prog{cfg: cfg}
		dg := newDomainGraph(p)

		assert.NotPanics(t, func() {
			dg.logTopTriggers()
		})
	})
}

// TestFetchAndReplayHistory_LogsTriggers verifies that logTopTriggers is
// called at the end of a successful replay. We can't capture logs in unit
// tests, but we can verify the call doesn't error out and the graph state
// is correct.
func TestFetchAndReplayHistory_LogsTriggers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Build a pattern that should produce a trigger:
		// client queries [a, b, c, d] in tight succession, multiple times.
		queries := []apiHistoryQuery{}
		for i := 0; i < 10; i++ {
			queries = append(queries,
				apiHistoryQuery{Domain: "a.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:0" + string(rune('0'+i)) + "Z", Qtype: "A"},
				apiHistoryQuery{Domain: "b.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:1" + string(rune('0'+i)) + "Z", Qtype: "A"},
				apiHistoryQuery{Domain: "c.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:2" + string(rune('0'+i)) + "Z", Qtype: "A"},
				apiHistoryQuery{Domain: "d.com", ClientIP: "10.0.0.1", Timestamp: "2026-10-15T12:00:3" + string(rune('0'+i)) + "Z", Qtype: "A"},
			)
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

	// Should not panic, and the graph should have a.com as a trigger
	// (it co-occurs with b.com, c.com, d.com — each above threshold 5
	// given historyWeight=3.0 × multiple replays).
	assert.NotPanics(t, func() {
		p.domainGraph.fetchAndReplayHistory(context.Background(), p)
	})

	neighbors := p.domainGraph.predictNeighbors("a.com")
	require.NotEmpty(t, neighbors, "a.com should have predicted neighbors (it's a trigger)")
}
