package cli

import (
        "encoding/json"
        "net/http"
        "sort"
        "strconv"
        "strings"
        "time"

        "github.com/miekg/dns"

        "github.com/Control-D-Inc/ctrld/internal/dnscache"
)

// cacheDumpEntry is the JSON representation of one cached DNS entry, exposed
// via the /cache endpoint on the metrics server. The structure is designed
// to be easy to consume from non-Go programs (e.g. bpftune-swap) — IPs are
// pre-extracted as a flat string slice, TTL is in seconds, expire is RFC3339.
type cacheDumpEntry struct {
        Name             string   `json:"name"`              // FQDN as queried, lowercased
        Qtype            string   `json:"qtype"`             // "A", "AAAA", "CNAME", etc.
        Qclass           string   `json:"qclass"`            // "IN" almost always
        Upstream         string   `json:"upstream"`          // e.g. "upstream.0"
        Expire          string   `json:"expire"`            // RFC3339 timestamp
        TTLRemainingSec int64    `json:"ttl_remaining_sec"` // seconds until expiry; negative if stale
        IPs             []string `json:"ips"`                // resolved IPs from Answer section
        CNAMEs          []string `json:"cnames"`             // CNAME chain targets
        Rcode           string   `json:"rcode"`              // "NOERROR", "NXDOMAIN", etc.
}

// cacheDumpResponse wraps the entries with metadata so consumers can verify
// they're looking at a complete snapshot.
type cacheDumpResponse struct {
        SnapshotTime string           `json:"snapshot_time"` // RFC3339, when the walk started
        Count        int              `json:"count"`         // number of entries returned
        Entries      []cacheDumpEntry `json:"entries"`
}

// serveCacheDump is the HTTP handler for GET /cache on the metrics server.
//
// It walks the in-memory DNS cache (using Walk(), which is non-polluting to
// the LRU policy), parses each cached *dns.Msg for A/AAAA/CNAME records, and
// returns a JSON array of entries. The walk is read-locked per entry, so it
// does not block concurrent DNS queries meaningfully.
//
// Query params:
//
//      ?domain=foo.com.        filter to entries whose Name matches exactly
//      ?domain_contains=foo   filter to entries whose Name contains the substring
//      ?upstream=upstream.0   filter to entries from a specific upstream
//      ?qtype=A               filter to entries of a specific qtype
//      ?min_ttl=60            only entries with TTL remaining >= N seconds
//      ?active=true           only entries with TTL remaining > 0 (default: false, include stale)
//      ?pretty=true           pretty-print JSON (default: compact)
//
// The endpoint is gated behind metrics_query_stats (same as the per-client
// stats endpoint) — if disabled, returns 404.
func (p *prog) serveCacheDump(w http.ResponseWriter, r *http.Request) {
        // Gate behind metrics_query_stats — same privacy posture as the
        // per-client query stats. The cache contents reveal which domains
        // are being queried, which is sensitive.
        if !p.metricsQueryStats.Load() {
                http.NotFound(w, r)
                return
        }
        if r.Method != http.MethodGet && r.Method != http.MethodHead {
                http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
                return
        }

        // Parse query params for filtering. All domain comparisons are done
        // with the trailing dot stripped on both sides (DNS FQDN convention is
        // to keep the dot, but users typically pass "example.com" without one).
        q := r.URL.Query()
        filterDomain := strings.ToLower(strings.TrimSuffix(q.Get("domain"), "."))
        filterDomainContains := strings.ToLower(q.Get("domain_contains"))
        filterUpstream := q.Get("upstream")
        filterQtype := strings.ToUpper(q.Get("qtype"))
        filterMinTTL := int64(0)
        if v := q.Get("min_ttl"); v != "" {
                if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
                        filterMinTTL = n
                }
        }
        filterActive := false
        if v := q.Get("active"); v != "" {
                if b, err := strconv.ParseBool(v); err == nil {
                        filterActive = b
                }
        }
        pretty := false
        if v := q.Get("pretty"); v != "" {
                if b, err := strconv.ParseBool(v); err == nil {
                        pretty = b
                }
        }

        snapshotTime := time.Now()
        entries := make([]cacheDumpEntry, 0, 64)

        if p.cache != nil {
                p.cache.Walk(func(k dnscache.Key, v *dnscache.Value) bool {
                        if v == nil || v.Msg == nil {
                                return true // skip empty entries
                        }
                        ttlRemaining := int64(time.Until(v.Expire).Seconds())
                        if filterActive && ttlRemaining <= 0 {
                                return true
                        }
                        if filterMinTTL > 0 && ttlRemaining < filterMinTTL {
                                return true
                        }
                        // Strip trailing dot from k.Name for comparison so users
                        // can pass ?domain=example.com (no dot) and it still matches
                        // the FQDN form "example.com." that's stored in the cache.
                        nameNoDot := strings.TrimSuffix(k.Name, ".")
                        if filterDomain != "" && nameNoDot != filterDomain {
                                return true
                        }
                        if filterDomainContains != "" && !strings.Contains(nameNoDot, filterDomainContains) {
                                return true
                        }
                        if filterUpstream != "" && k.Upstream != filterUpstream {
                                return true
                        }
                        qtypeName := dns.TypeToString[k.Qtype]
                        if filterQtype != "" && qtypeName != filterQtype {
                                return true
                        }

                        e := cacheDumpEntry{
                                Name:             k.Name,
                                Qtype:            qtypeName,
                                Qclass:           dns.ClassToString[k.Qclass],
                                Upstream:         k.Upstream,
                                Expire:           v.Expire.Format(time.RFC3339),
                                TTLRemainingSec:  ttlRemaining,
                                Rcode:            dns.RcodeToString[v.Msg.Rcode],
                                IPs:              []string{},
                                CNAMEs:           []string{},
                        }

                        // Extract A/AAAA/CNAME records from the Answer section.
                        // We don't include Ns/Extra since those are typically
                        // authority/additional records, not the actual resolved
                        // IPs the client cares about.
                        if len(v.Msg.Answer) > 0 {
                                for _, rr := range v.Msg.Answer {
                                        switch t := rr.(type) {
                                        case *dns.A:
                                                e.IPs = append(e.IPs, t.A.String())
                                        case *dns.AAAA:
                                                e.IPs = append(e.IPs, t.AAAA.String())
                                        case *dns.CNAME:
                                                e.CNAMEs = append(e.CNAMEs, strings.TrimSuffix(t.Target, "."))
                                        }
                                }
                        }

                        entries = append(entries, e)
                        return true
                })
        }

        // Sort by name (alphabetical), then qtype, then upstream — gives stable
        // output that's easy to diff between polls.
        sort.Slice(entries, func(i, j int) bool {
                if entries[i].Name != entries[j].Name {
                        return entries[i].Name < entries[j].Name
                }
                if entries[i].Qtype != entries[j].Qtype {
                        return entries[i].Qtype < entries[j].Qtype
                }
                return entries[i].Upstream < entries[j].Upstream
        })

        resp := cacheDumpResponse{
                SnapshotTime: snapshotTime.Format(time.RFC3339),
                Count:        len(entries),
                Entries:      entries,
        }

        w.Header().Set("Content-Type", "application/json")
        w.Header().Set("Cache-Control", "no-store")
        var b []byte
        if pretty {
                b, _ = json.MarshalIndent(resp, "", "  ")
        } else {
                b, _ = json.Marshal(resp)
        }
        // Intentionally ignore write error — client may have disconnected,
        // nothing we can do about it.
        _, _ = w.Write(b)
}
