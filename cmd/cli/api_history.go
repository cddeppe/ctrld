package cli

import (
        "context"
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "os"
        "strings"
        "sync/atomic"
        "time"
)

// apiHistoryConfig holds the runtime configuration for fetching query history
// from Control-D's API to pre-seed the domain graph. Built once at startup
// from ServiceConfig + env vars.
type apiHistoryConfig struct {
        enabled bool
        token   string // Bearer token, read from CONTROLD_API_TOKEN env var
        deviceID string // Control-D resolver/device ID, from the --cd flag value
        limit   int    // how many queries to fetch
}

// apiHistoryResponse is the (defensive) shape of the JSON response from
// https://api.controld.com/queries?device_id=X&limit=N. Control-D doesn't
// publish an official schema for this endpoint, so we parse defensively:
// every field is optional except `queries` itself, and unknown fields are
// ignored. Field name aliases cover the variants we've seen in different
// docs (e.g. `client_ip` vs `src_ip` vs `source_ip`).
type apiHistoryResponse struct {
        Body struct {
                Queries []apiHistoryQuery `json:"queries"`
        } `json:"body"`
}

// apiHistoryQuery is one row in the queries array. Fields are intentionally
// all optional/loose-typed so a non-standard response doesn't break parsing.
type apiHistoryQuery struct {
        // Domain is required for replay — without it the query is useless.
        Domain string `json:"domain"`

        // Time is used to sort queries chronologically before replay. If
        // missing, we assume the API already returned them in order.
        Timestamp string `json:"timestamp"`
        Time       string `json:"time"`
        TS         string `json:"ts"`

        // ClientIP groups queries per-client for co-occurrence. If missing,
        // we use a single "anonymous" bucket — patterns will be global
        // rather than per-client, but still useful.
        ClientIP    string `json:"client_ip"`
        SrcIP       string `json:"src_ip"`
        SourceIP    string `json:"source_ip"`
        Source      string `json:"source"`

        // Qtype filters to A/AAAA only (same as the live hook). If missing,
        // we assume A/AAAA (the most common case).
        Qtype string `json:"qtype"`
        Type  string `json:"type"`
}

// parseClientIP returns the first non-empty client IP field, or "anonymous"
// if none are present. We normalize to a string bucket so the domain graph
// can group queries per-client.
func (q apiHistoryQuery) parseClientIP() string {
        if q.ClientIP != "" {
                return q.ClientIP
        }
        if q.SrcIP != "" {
                return q.SrcIP
        }
        if q.SourceIP != "" {
                return q.SourceIP
        }
        if q.Source != "" {
                return q.Source
        }
        return "anonymous"
}

// parseTimestamp returns the timestamp string (whichever field has it),
// or empty string if none present.
func (q apiHistoryQuery) parseTimestamp() string {
        if q.Timestamp != "" {
                return q.Timestamp
        }
        if q.Time != "" {
                return q.Time
        }
        return q.TS
}

// parseQtype returns the qtype, normalized to uppercase. If missing,
// returns "A" (the most common query type).
func (q apiHistoryQuery) parseQtype() string {
        if q.Qtype != "" {
                return q.Qtype
        }
        if q.Type != "" {
                return q.Type
        }
        return "A"
}

// newAPIHistoryConfig builds the runtime config from p's settings.
// Returns a disabled config if any of: domain prefetch is off, the
// api history fetch is disabled, no token, or no device ID.
func newAPIHistoryConfig(p *prog) apiHistoryConfig {
        cfg := apiHistoryConfig{
                limit: 10000,
        }
        if p.domainGraph == nil || !p.domainGraph.enabled {
                return cfg
        }
        if v := p.cfg.Service.DomainPrefetchAPIHistoryFetch; v == nil || !*v {
                return cfg
        }
        cfg.enabled = true

        // Token resolution order:
        //   1. Env var CONTROLD_API_TOKEN (preferred — keeps secret out of config)
        //   2. Env var CONTROLD_API_TOKEN_READ (mcp-controld naming convention)
        //   3. Config field DomainPrefetchAPIToken (works via Control-D CustomConfig
        //      overlay — convenient since it auto-propagates to all devices using
        //      the same resolver ID; just remember any dashboard admin can see it)
        cfg.token = os.Getenv("CONTROLD_API_TOKEN")
        if cfg.token == "" {
                cfg.token = os.Getenv("CONTROLD_API_TOKEN_READ")
        }
        if cfg.token == "" && p.cfg.Service.DomainPrefetchAPIToken != nil {
                cfg.token = *p.cfg.Service.DomainPrefetchAPIToken
        }
        if cfg.token == "" {
                // Disabled — no token, can't call the API.
                cfg.enabled = false
                return cfg
        }

        // Device ID: prefer explicit config, fall back to --cd flag value.
        // The --cd flag is the resolver UID, which is the same as device_id
        // in the API's terms.
        if v := p.cfg.Service.DomainPrefetchAPIHistoryDeviceID; v != nil && *v != "" {
                cfg.deviceID = *v
        } else if p.rc != nil && p.rc.UID != "" {
                cfg.deviceID = p.rc.UID
        }
        if cfg.deviceID == "" {
                // Can't fetch without a device ID — disable with a warning.
                cfg.enabled = false
                return cfg
        }

        if v := p.cfg.Service.DomainPrefetchAPIHistoryLimit; v != nil && *v > 0 {
                cfg.limit = *v
        }
        return cfg
}

// fetchAndReplayHistory fetches the device's recent query history from
// the Control-D API and replays it through the domain graph to pre-seed
// the co-occurrence patterns. Designed to run in a background goroutine
// at startup — does NOT block ctrld from serving queries.
//
// On any error (network, auth, parse, rate-limit), the function logs and
// returns without modifying the graph. ctrld continues to operate normally
// with an empty graph, learning from live queries as they arrive.
//
// The replay is straightforward: sort queries by timestamp (oldest first,
// if timestamps are present), filter to A/AAAA only (matching the live
// hook's behavior), and call dg.recordQuery(clientIP, domain) for each.
// The existing recordQuery logic handles dedup, edge weighting, and LRU
// eviction — same code path as live queries, so the graph is consistent.
//
// Rate-limit handling: on HTTP 429, wait 60s and retry once. If still
// rate-limited, give up (will retry on next ctrld restart).
//
// Auth errors (401/403) are treated as permanent — log the error and
// return. No retry. (User likely has a bad token; retrying would just
// spam the API.)
func (dg *domainGraph) fetchAndReplayHistory(ctx context.Context, p *prog) {
        cfg := newAPIHistoryConfig(p)
        if !cfg.enabled {
                if p.domainGraph.enabled {
                        mainLog.Load().Debug().Msg("domain-graph: API history fetch disabled (no token, no device ID, or feature off)")
                }
                return
        }

        url := fmt.Sprintf("https://api.controld.com/queries?device_id=%s&limit=%d",
                cfg.deviceID, cfg.limit)

        // Test-only override: if CONTROLD_API_URL_OVERRIDE is set, use that
        // as the base URL instead of api.controld.com. Used by api_history_test.go
        // to point at a mock httptest.Server. Not intended for production use
        // (the env var should never be set in real deployments).
        if override := os.Getenv("CONTROLD_API_URL_OVERRIDE"); override != "" {
                url = fmt.Sprintf("%s/queries?device_id=%s&limit=%d",
                        strings.TrimRight(override, "/"), cfg.deviceID, cfg.limit)
        }

        mainLog.Load().Info().Msgf("domain-graph: fetching %d queries from API for device %s",
                cfg.limit, cfg.deviceID)

        // HTTP client with a generous timeout (the response can be large —
        // 10000 queries * ~200 bytes each = ~2MB).
        client := &http.Client{Timeout: 60 * time.Second}

        req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
        if err != nil {
                mainLog.Load().Warn().Err(err).Msg("domain-graph: failed to create API request")
                return
        }
        req.Header.Set("Authorization", "Bearer "+cfg.token)
        req.Header.Set("Accept", "application/json")
        req.Header.Set("User-Agent", "ctrld/"+curVersion())

        resp, err := client.Do(req)
        if err != nil {
                // Network error — transient. Will retry on next restart.
                mainLog.Load().Warn().Err(err).Msg("domain-graph: API history fetch failed (network error)")
                return
        }
        defer resp.Body.Close()

        // Rate-limited — wait 60s and retry once.
        if resp.StatusCode == http.StatusTooManyRequests {
                mainLog.Load().Info().Msg("domain-graph: API rate-limited, waiting 60s and retrying once")
                select {
                case <-ctx.Done():
                        return
                case <-time.After(60 * time.Second):
                }
                resp.Body.Close()
                resp, err = client.Do(req)
                if err != nil {
                        mainLog.Load().Warn().Err(err).Msg("domain-graph: API retry failed")
                        return
                }
                defer resp.Body.Close()
        }

        if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
                mainLog.Load().Error().Int("status", resp.StatusCode).
                        Msg("domain-graph: API auth failed (check CONTROLD_API_TOKEN — needs read scope)")
                return
        }
        if resp.StatusCode != http.StatusOK {
                body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
                mainLog.Load().Warn().Int("status", resp.StatusCode).
                        Str("body", string(body)).
                        Msg("domain-graph: API returned non-200 status")
                return
        }

        var apiResp apiHistoryResponse
        if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
                mainLog.Load().Warn().Err(err).
                        Msg("domain-graph: failed to parse API response")
                return
        }
        queries := apiResp.Body.Queries
        if len(queries) == 0 {
                mainLog.Load().Info().Msg("domain-graph: API returned no queries (device may not have Full Analytics enabled)")
                return
        }

        // Sort by timestamp if timestamps are present. Otherwise assume the
        // API returned them in order (most recent last is typical for query
        // logs — we want oldest first for replay so recent patterns dominate
        // via the LRU + decay mechanism).
        if queries[0].parseTimestamp() != "" {
                sortByTimestampOldestFirst(queries)
        }

        // Replay: filter to A/AAAA, call recordQuery for each. The existing
        // recordQuery handles dedup, edge weighting, etc.
        replayed := 0
        skipped := 0
        for _, q := range queries {
                if q.Domain == "" {
                        skipped++
                        continue
                }
                qtype := q.parseQtype()
                // Match the live hook: only A/AAAA queries contribute to the
                // co-occurrence graph (CNAME/MX/TXT/SRV are singletons).
                qtypeUpper := qtype
                if len(qtype) >= 1 {
                        // Cheap uppercase for ASCII.
                        b := []byte(qtype)
                        for i := range b {
                                if 'a' <= b[i] && b[i] <= 'z' {
                                        b[i] = b[i] - 'a' + 'A'
                                }
                        }
                        qtypeUpper = string(b)
                }
                if qtypeUpper != "A" && qtypeUpper != "AAAA" {
                        skipped++
                        continue
                }
                // Replay with the history-weight multiplier — historical patterns
                // are based on ~10K queries worth of data, so they should
                // outweigh a few hours of live observation. Weight defaults to
                // 3.0; configurable via domain_prefetch_history_weight.
                dg.recordQueryWithWeight(q.parseClientIP(), q.Domain, dg.historyWeight)
                replayed++
        }

        mainLog.Load().Info().Msgf("domain-graph: replayed %d queries (skipped %d non-A/AAAA/empty) with weight=%.1fx, graph now has %d domains",
                replayed, skipped, dg.historyWeight, dg.domainCount())

        // Log the top trigger domains so the user can SEE what patterns were
        // learned from history (e.g. "netflix.com -> [nflximg.com, ...]").
        // Pure observability — no behavior change.
        dg.logTopTriggers()

        // Count for observability.
        apiHistoryReplays.Add(int64(replayed))
}

// domainCount returns the number of distinct domains in the co-occurrence
// graph, for logging. Read-locked.
func (dg *domainGraph) domainCount() int {
        dg.mu.RLock()
        defer dg.mu.RUnlock()
        return len(dg.coOccur)
}

// sortByTimestampOldestFirst sorts queries by timestamp ascending. Uses
// insertion sort for stability on small inputs (the API caps at ~10K
// queries, and Go's sort.Slice is plenty fast for that). We use a
// straightforward lexicographic comparison on the timestamp string —
// RFC3339 timestamps sort correctly as strings.
func sortByTimestampOldestFirst(qs []apiHistoryQuery) {
        // Use Go's sort.Slice — it's O(N log N) and well-tested.
        // Custom comparator: empty timestamps go to the end.
        for i := 1; i < len(qs); i++ {
                j := i
                for j > 0 {
                        tsA := qs[j-1].parseTimestamp()
                        tsB := qs[j].parseTimestamp()
                        if tsA == "" {
                                // Empty A goes after B (to end of slice).
                                break
                        }
                        if tsB != "" && tsA > tsB {
                                qs[j-1], qs[j] = qs[j], qs[j-1]
                                j--
                        } else {
                                break
                        }
                }
        }
}

// startAPIHistoryFetcher is the public hook called from prog.run() to
// kick off the background fetch + replay. Safe to call multiple times
// (gcOnce-style); only runs once per domainGraph instance.
//
// The fetch happens in a goroutine bound to ctx, so it's cancelled on
// reload/stop. ctrld continues serving queries while the fetch is in
// progress — the graph fills in within seconds of startup.
func (dg *domainGraph) startAPIHistoryFetcher(ctx context.Context, p *prog) {
        if !dg.enabled {
                return
        }
        go dg.fetchAndReplayHistory(ctx, p)
}

// apiHistoryReplays is a prometheus counter for observability — total
// queries replayed from API history into the graph across all restarts.
// Defined here for locality; the metric itself is registered in
// prometheus.go alongside the other domain-prefetch counters.
var apiHistoryReplays atomic.Int64
