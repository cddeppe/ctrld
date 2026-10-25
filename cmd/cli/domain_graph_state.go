package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"time"
)

// graphStateNode is one persisted domain node: its co-occurrence counts
// (lifetime habit memory) and the last time it was queried (for ranking).
type graphStateNode struct {
	Domain   string             `json:"d"`
	LastSeen int64              `json:"t"` // unix seconds
	Counts   map[string]float64 `json:"c"`
}

const (
	// maxPersistedNeighbors caps neighbors written per domain. Only the
	// strongest (by count) are kept; weaker ones re-learn quickly.
	maxPersistedNeighbors = 20
	// minPersistedCount is the minimum count worth persisting. Sub-threshold
	// noise (pairs seen once or twice) is re-learnable, not worth flash.
	minPersistedCount = 2
)

// saveState writes the habit graph to dg.statePath as JSONL (one node per
// line). Small on purpose: established edges only, capped per domain —
// typically 2-4MB for a household. Atomic-ish: temp file + rename.
// Serializes via saveMu; safe to call from Stop() and the GC loop.
func (dg *domainGraph) saveState() {
	if !dg.enabled || dg.statePath == "" {
		return
	}
	dg.saveMu.Lock()
	defer dg.saveMu.Unlock()

	dg.mu.Lock()
	if !dg.dirty {
		dg.mu.Unlock()
		return
	}
	// Snapshot under the lock, marshal outside it.
	nodes := make([]graphStateNode, 0, len(dg.coOccur))
	for domain, neighbors := range dg.coOccur {
		type kv struct {
			n string
			c float64
		}
		edges := make([]kv, 0, len(neighbors))
		for n, c := range neighbors {
			if c >= minPersistedCount {
				edges = append(edges, kv{n: n, c: c})
			}
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].c != edges[j].c {
				return edges[i].c > edges[j].c
			}
			return edges[i].n < edges[j].n
		})
		if len(edges) > maxPersistedNeighbors {
			edges = edges[:maxPersistedNeighbors]
		}
		if len(edges) == 0 {
			continue
		}
		counts := make(map[string]float64, len(edges))
		for _, e := range edges {
			counts[e.n] = e.c
		}
		node := graphStateNode{Domain: domain, Counts: counts}
		if t, ok := dg.lastSeen[domain]; ok {
			node.LastSeen = t.Unix()
		}
		nodes = append(nodes, node)
	}
	dg.mu.Unlock()

	tmpPath := dg.statePath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		mainLog.Load().Warn().Err(err).Msg("domain-graph: could not create state file")
		return
	}
	w := bufio.NewWriterSize(f, 64*1024)
	enc := json.NewEncoder(w)
	writeErr := error(nil)
	for i := range nodes {
		if writeErr = enc.Encode(&nodes[i]); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		writeErr = w.Flush()
	}
	if cerr := f.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		os.Remove(tmpPath)
		mainLog.Load().Warn().Err(writeErr).Msg("domain-graph: failed writing state file")
		return
	}
	if err := os.Rename(tmpPath, dg.statePath); err != nil {
		os.Remove(tmpPath)
		mainLog.Load().Warn().Err(err).Msg("domain-graph: failed to replace state file")
		return
	}
	// Only now is the graph considered clean.
	dg.mu.Lock()
	dg.dirty = false
	dg.mu.Unlock()
	dg.lastSave = time.Now()
	mainLog.Load().Warn().Msgf("domain-graph: saved %d domains to %s", len(nodes), dg.statePath)
}

// maybeSaveStateNightly saves the graph once per day at dg.stateHour
// (local time). Checked from the GC loop every gcInterval, so the write
// lands within one tick of the target hour. One small write per day.
func (dg *domainGraph) maybeSaveStateNightly() {
	if !dg.enabled || dg.statePath == "" || dg.stateHour < 0 {
		return
	}
	now := time.Now()
	if now.Hour() != dg.stateHour {
		return
	}
	// Only once per day.
	dg.saveMu.Lock()
	last := dg.lastSave
	dg.saveMu.Unlock()
	if now.Sub(last) < 23*time.Hour {
		return
	}
	dg.saveState()
}

// loadState restores the habit graph from dg.statePath. Called at startup
// BEFORE query-log replay, so week-old patterns survive reboots. A
// missing or corrupt file is a no-op — the graph just starts cold.
func (dg *domainGraph) loadState() {
	if !dg.enabled || dg.statePath == "" {
		return
	}
	f, err := os.Open(dg.statePath)
	if err != nil {
		return // no state yet — first run
	}
	defer f.Close()

	loaded := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var node graphStateNode
		if err := json.Unmarshal(line, &node); err != nil || node.Domain == "" {
			continue
		}
		domain := normalizeDomain(node.Domain)
		dg.mu.Lock()
		if node.LastSeen > 0 {
			dg.lastSeen[domain] = time.Unix(node.LastSeen, 0)
		}
		for n, c := range node.Counts {
			if c <= 0 {
				continue
			}
			if dg.coOccur[domain] == nil {
				dg.coOccur[domain] = make(map[string]float64, len(node.Counts))
			}
			dg.coOccur[domain][normalizeDomain(n)] = c
		}
		loaded++
		dg.mu.Unlock()
	}
	// Enforce the size cap after bulk load.
	dg.mu.Lock()
	dg.enforceCapLocked(time.Now())
	dg.mu.Unlock()

	mainLog.Load().Warn().Msgf("domain-graph: restored %d domains from %s (graph has %d domains)",
		loaded, dg.statePath, dg.domainCount())
}
