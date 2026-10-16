package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Control-D-Inc/ctrld/internal/dnscache"
	"github.com/Control-D-Inc/ctrld/testhelper"
)

// TestNewAPIHistoryConfig_DeviceIDFromCdFlag verifies that the device ID
// is auto-detected from the --cd flag value (cdUID package-level var)
// when no explicit device_id is configured. This is the most common case
// for users running ctrld with --cd <resolver_id>.
func TestNewAPIHistoryConfig_DeviceIDFromCdFlag(t *testing.T) {
	// Save and restore the package-level cdUID var so the test is isolated.
	savedCdUID := cdUID
	defer func() { cdUID = savedCdUID }()

	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	featureOn := true
	cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
	t.Setenv("CONTROLD_API_TOKEN", "test-token")
	// No explicit domain_prefetch_api_history_device_id set.

	// Set the --cd flag value (simulating CLI parse).
	cdUID = "2gjp83mawy1"

	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	p.domainGraph = newDomainGraph(p)

	apiCfg := newAPIHistoryConfig(p)
	assert.True(t, apiCfg.enabled, "should be enabled when --cd flag provides the device ID")
	assert.Equal(t, "2gjp83mawy1", apiCfg.deviceID,
		"device ID should come from cdUID (--cd flag value)")
	assert.Empty(t, apiCfg.disableReason, "should not have a disable reason when enabled")
}

// TestNewAPIHistoryConfig_DeviceIDExplicitOverridesCdFlag verifies that
// an explicit domain_prefetch_api_history_device_id takes precedence
// over the --cd flag value.
func TestNewAPIHistoryConfig_DeviceIDExplicitOverridesCdFlag(t *testing.T) {
	savedCdUID := cdUID
	defer func() { cdUID = savedCdUID }()

	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	featureOn := true
	cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
	explicitID := "explicit-device-id"
	cfg.Service.DomainPrefetchAPIHistoryDeviceID = &explicitID
	t.Setenv("CONTROLD_API_TOKEN", "test-token")

	cdUID = "cd-flag-value"  // should be ignored in favor of explicit config

	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	p.domainGraph = newDomainGraph(p)

	apiCfg := newAPIHistoryConfig(p)
	assert.True(t, apiCfg.enabled)
	assert.Equal(t, "explicit-device-id", apiCfg.deviceID,
		"explicit config field should take precedence over --cd flag")
}

// TestNewAPIHistoryConfig_DeviceIDFromResolverConfig verifies that
// p.rc.UID is used as a last resort when neither explicit config nor
// --cd flag is set (e.g., when ctrld is configured via config file
// rather than CLI flags).
func TestNewAPIHistoryConfig_DeviceIDFromResolverConfig(t *testing.T) {
	savedCdUID := cdUID
	defer func() { cdUID = savedCdUID }()

	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	featureOn := true
	cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
	t.Setenv("CONTROLD_API_TOKEN", "test-token")

	cdUID = ""  // no --cd flag

	// p.rc is nil in this test (not set up), so we can't fully test the
	// fallback path. But we can verify that with no device ID source,
	// the feature disables with the right reason.
	p := &prog{cfg: cfg}
	cacher, _ := dnscache.NewLRUCache(64)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	p.domainGraph = newDomainGraph(p)

	apiCfg := newAPIHistoryConfig(p)
	assert.False(t, apiCfg.enabled, "should be disabled when no device ID source available")
	assert.NotEmpty(t, apiCfg.disableReason, "should have a specific disable reason")
	assert.Contains(t, apiCfg.disableReason, "device ID",
		"disable reason should mention device ID")
}

// TestNewAPIHistoryConfig_DisableReasons verifies that each disable path
// produces a specific, actionable disableReason message.
func TestNewAPIHistoryConfig_DisableReasons(t *testing.T) {
	t.Run("domain graph disabled (cache_enable=false)", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = false  // disables domain graph entirely
		featureOn := true
		cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
		p := &prog{cfg: cfg}
		p.domainGraph = newDomainGraph(p)

		apiCfg := newAPIHistoryConfig(p)
		assert.False(t, apiCfg.enabled)
		assert.Contains(t, apiCfg.disableReason, "domain graph is disabled",
			"disable reason should explain that the domain graph itself is off")
	})

	t.Run("feature flag not set", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = true
		cfg.Service.CacheSize = 64
		// DomainPrefetchAPIHistoryFetch not set (nil)
		p := &prog{cfg: cfg}
		cacher, _ := dnscache.NewLRUCache(64)
		p.cache = cacher
		p.um = newUpstreamMonitor(cfg)
		p.domainGraph = newDomainGraph(p)

		apiCfg := newAPIHistoryConfig(p)
		assert.False(t, apiCfg.enabled)
		assert.Contains(t, apiCfg.disableReason, "domain_prefetch_api_history_fetch",
			"disable reason should mention the specific config knob to set")
	})

	t.Run("no token", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = true
		cfg.Service.CacheSize = 64
		featureOn := true
		cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
		// No env var, no config field.
		t.Setenv("CONTROLD_API_TOKEN", "")
		t.Setenv("CONTROLD_API_TOKEN_READ", "")

		savedCdUID := cdUID
		defer func() { cdUID = savedCdUID }()
		cdUID = "test-device"  // device ID is set, but no token

		p := &prog{cfg: cfg}
		cacher, _ := dnscache.NewLRUCache(64)
		p.cache = cacher
		p.um = newUpstreamMonitor(cfg)
		p.domainGraph = newDomainGraph(p)

		apiCfg := newAPIHistoryConfig(p)
		assert.False(t, apiCfg.enabled)
		assert.Contains(t, apiCfg.disableReason, "API token",
			"disable reason should mention the missing token")
	})

	t.Run("no device ID", func(t *testing.T) {
		cfg := testhelper.SampleConfig(t)
		cfg.Service.CacheEnable = true
		cfg.Service.CacheSize = 64
		featureOn := true
		cfg.Service.DomainPrefetchAPIHistoryFetch = &featureOn
		t.Setenv("CONTROLD_API_TOKEN", "test-token")

		savedCdUID := cdUID
		defer func() { cdUID = savedCdUID }()
		cdUID = ""  // no --cd flag
		// p.rc is nil in this test

		p := &prog{cfg: cfg}
		cacher, _ := dnscache.NewLRUCache(64)
		p.cache = cacher
		p.um = newUpstreamMonitor(cfg)
		p.domainGraph = newDomainGraph(p)

		apiCfg := newAPIHistoryConfig(p)
		assert.False(t, apiCfg.enabled)
		assert.Contains(t, apiCfg.disableReason, "device ID",
			"disable reason should mention the missing device ID")
	})
}
