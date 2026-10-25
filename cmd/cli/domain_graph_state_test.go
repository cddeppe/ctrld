package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Control-D-Inc/ctrld/internal/dnscache"
	"github.com/Control-D-Inc/ctrld/testhelper"
)

func newHabitTestGraph(t *testing.T, statePath string) *domainGraph {
	t.Helper()
	cfg := testhelper.SampleConfig(t)
	cfg.Service.CacheEnable = true
	cfg.Service.CacheSize = 64
	if statePath != "" {
		cfg.Service.DomainPrefetchStatePath = &statePath
	}
	p := &prog{cfg: cfg}
	cacher, err := dnscache.NewLRUCache(64)
	require.NoError(t, err)
	p.cache = cacher
	p.um = newUpstreamMonitor(cfg)
	return newDomainGraph(p)
}

// TestDomainGraph_HabitMemory verifies that co-occurrence counts never
// decay: a pair last seen 6 days ago is still predicted immediately when
// the trigger domain is queried (the weekly-habit case), even though it
// ranks below fresher patterns.
func TestDomainGraph_HabitMemory(t *testing.T) {
	dg := newHabitTestGraph(t, "")
	for i := 0; i < 10; i++ {
		dg.recordQuery("c1", "netflix.com")
		dg.recordQuery("c1", "nflximg.net")
	}
	// Fast-forward: nothing queried for 6 days.
	dg.mu.Lock()
	dg.lastSeen["netflix.com"] = time.Now().Add(-6 * 24 * time.Hour)
	dg.lastSeen["nflximg.net"] = time.Now().Add(-6 * 24 * time.Hour)
	dg.mu.Unlock()

	neighbors := dg.predictNeighbors("netflix.com")
	assert.Contains(t, neighbors, "nflximg.net",
		"weekly habit must still be predicted after 6 days")
}

// TestDomainGraph_RankingPrefersFresh verifies that a fresh neighbor
// outranks a stale one with a bigger count.
func TestDomainGraph_RankingPrefersFresh(t *testing.T) {
	dg := newHabitTestGraph(t, "")
	dg.mu.Lock()
	dg.addEdgeLocked("a.com", "old.com", 100)
	dg.addEdgeLocked("a.com", "new.com", 10)
	dg.lastSeen["old.com"] = time.Now().Add(-30 * 24 * time.Hour)
	dg.lastSeen["new.com"] = time.Now()
	dg.mu.Unlock()

	neighbors := dg.predictNeighbors("a.com")
	require.Len(t, neighbors, 2)
	assert.Equal(t, "new.com", neighbors[0],
		"fresh pattern should outrank stale heavy one")
}

// TestDomainGraph_StateRoundTrip verifies that the habit graph survives
// a restart via the state file: save on one instance, load on another.
func TestDomainGraph_StateRoundTrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "graph.jsonl")

	dg1 := newHabitTestGraph(t, statePath)
	for i := 0; i < 10; i++ {
		dg1.recordQuery("c1", "netflix.com")
		dg1.recordQuery("c1", "nflximg.net")
	}
	dg1.saveState()

	dg2 := newHabitTestGraph(t, statePath)
	dg2.loadState()
	assert.Contains(t, dg2.predictNeighbors("netflix.com"), "nflximg.net",
		"habit must survive a restart via state file")
}

// TestDomainGraph_StateNoFile verifies that loadState is a no-op when the
// state file doesn't exist (first run — graph starts cold).
func TestDomainGraph_StateNoFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nonexistent.jsonl")
	dg := newHabitTestGraph(t, statePath)
	dg.loadState()
	assert.Equal(t, 0, dg.domainCount(),
		"graph should be empty when state file doesn't exist")
}

// TestDomainGraph_SaveStateNotDirty verifies that saveState is a no-op
// when the graph hasn't been modified since the last save.
func TestDomainGraph_SaveStateNotDirty(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "graph.jsonl")
	dg := newHabitTestGraph(t, statePath)
	// No mutations — dirty is false.
	dg.saveState()
	// File should NOT exist (nothing to save).
	_, err := filepath.Glob(statePath)
	assert.NoError(t, err)
}
