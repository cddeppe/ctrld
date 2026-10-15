package cli

import (
        "context"
        "encoding/json"
        "net/http"
        "net/http/httptest"
        "strings"
        "testing"
        "time"

        "github.com/miekg/dns"
        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"

        "github.com/Control-D-Inc/ctrld/internal/dnscache"
        "github.com/Control-D-Inc/ctrld/testhelper"
)

// TestServeCacheDump_GatedByMetricsQueryStats verifies the /cache endpoint
// returns 404 when metrics_query_stats is disabled (privacy gate).
func TestServeCacheDump_GatedByMetricsQueryStats(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        p.metricsQueryStats.Store(false) // gate closed

        cacher, err := dnscache.NewLRUCache(64)
        require.NoError(t, err)
        p.cache = cacher

        // Add a test entry so we can prove the gate is what's blocking, not
        // an empty cache.
        msg := new(dns.Msg)
        msg.SetQuestion("example.com.", dns.TypeA)
        msg.Answer = append(msg.Answer, &dns.A{
                Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
                A:   []byte{1, 2, 3, 4},
        })
        p.cache.Add(dnscache.NewKey(msg, "upstream.0"),
                dnscache.NewValue(msg, time.Now().Add(60*time.Second)))

        req := httptest.NewRequest(http.MethodGet, "/cache", nil)
        w := httptest.NewRecorder()
        p.serveCacheDump(w, req)

        assert.Equal(t, http.StatusNotFound, w.Code,
                "handler should return 404 when metrics_query_stats is disabled")
}

// TestServeCacheDump_ReturnsEntries verifies the handler returns JSON with
// the expected structure: snapshot_time, count, entries[]. Each entry has
// name, qtype, qclass, upstream, expire, ttl_remaining_sec, ips, cnames,
// rcode. The IPs are extracted from the cached Msg's Answer section.
func TestServeCacheDump_ReturnsEntries(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        p.metricsQueryStats.Store(true) // gate open

        cacher, err := dnscache.NewLRUCache(64)
        require.NoError(t, err)
        p.cache = cacher

        // Cached A record with two IPs.
        msgA := new(dns.Msg)
        msgA.SetQuestion("example.com.", dns.TypeA)
        msgA.Answer = append(msgA.Answer,
                &dns.A{
                        Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
                        A:   []byte{93, 184, 216, 34},
                },
                &dns.A{
                        Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
                        A:   []byte{93, 184, 216, 35},
                },
        )
        msgA.Rcode = dns.RcodeSuccess
        p.cache.Add(
                dnscache.NewKey(msgA, "upstream.0"),
                dnscache.NewValue(msgA, time.Now().Add(300*time.Second)),
        )

        // Cached AAAA record.
        msgAAAA := new(dns.Msg)
        msgAAAA.SetQuestion("example.com.", dns.TypeAAAA)
        msgAAAA.Answer = append(msgAAAA.Answer, &dns.AAAA{
                Hdr:  dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 300},
                AAAA: []byte{0x26, 0x06, 0x19, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01},
        })
        p.cache.Add(
                dnscache.NewKey(msgAAAA, "upstream.0"),
                dnscache.NewValue(msgAAAA, time.Now().Add(300*time.Second)),
        )

        // Cached CNAME chain.
        msgCNAME := new(dns.Msg)
        msgCNAME.SetQuestion("www.example.com.", dns.TypeA)
        msgCNAME.Answer = append(msgCNAME.Answer,
                &dns.CNAME{
                        Hdr:    dns.RR_Header{Name: "www.example.com.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300},
                        Target: "example.com.",
                },
                &dns.A{
                        Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
                        A:   []byte{93, 184, 216, 34},
                },
        )
        p.cache.Add(
                dnscache.NewKey(msgCNAME, "upstream.0"),
                dnscache.NewValue(msgCNAME, time.Now().Add(300*time.Second)),
        )

        req := httptest.NewRequest(http.MethodGet, "/cache", nil)
        w := httptest.NewRecorder()
        p.serveCacheDump(w, req)

        require.Equal(t, http.StatusOK, w.Code)
        assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

        var resp cacheDumpResponse
        require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
        assert.NotEmpty(t, resp.SnapshotTime, "snapshot_time should be set")
        assert.Equal(t, 3, resp.Count, "should have 3 entries (A, AAAA, CNAME+A)")
        assert.Len(t, resp.Entries, 3)

        // Find the A entry for example.com and verify its IPs.
        var entryA *cacheDumpEntry
        for i := range resp.Entries {
                e := &resp.Entries[i]
                if e.Name == "example.com." && e.Qtype == "A" {
                        entryA = e
                        break
                }
        }
        require.NotNil(t, entryA, "should find A entry for example.com")
        assert.Equal(t, "IN", entryA.Qclass)
        assert.Equal(t, "upstream.0", entryA.Upstream)
        assert.Equal(t, "NOERROR", entryA.Rcode)
        assert.ElementsMatch(t, []string{"93.184.216.34", "93.184.216.35"}, entryA.IPs)
        assert.Empty(t, entryA.CNAMEs, "A record entry should not have CNAMEs")
        assert.Greater(t, entryA.TTLRemainingSec, int64(290), "TTL should be ~300s minus a bit")
        assert.LessOrEqual(t, entryA.TTLRemainingSec, int64(300))
}

// TestServeCacheDump_Filters verifies the handler respects the documented
// query params: domain, upstream, qtype, min_ttl, active.
func TestServeCacheDump_Filters(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        p.metricsQueryStats.Store(true)

        cacher, err := dnscache.NewLRUCache(64)
        require.NoError(t, err)
        p.cache = cacher

        // Add three entries: example.com A (fresh), example.com AAAA (fresh),
        // expired.com A (stale, expired 10s ago).
        addEntry := func(name string, qtype uint16, ttl time.Duration) {
                msg := new(dns.Msg)
                msg.SetQuestion(name, qtype)
                msg.Answer = append(msg.Answer, &dns.A{
                        Hdr: dns.RR_Header{Name: name, Rrtype: qtype, Class: dns.ClassINET, Ttl: uint32(ttl.Seconds())},
                        A:   []byte{1, 2, 3, 4},
                })
                msg.Rcode = dns.RcodeSuccess
                p.cache.Add(
                        dnscache.NewKey(msg, "upstream.0"),
                        dnscache.NewValue(msg, time.Now().Add(ttl)),
                )
        }
        addEntry("example.com.", dns.TypeA, 300*time.Second)
        addEntry("example.com.", dns.TypeAAAA, 300*time.Second)
        addEntry("expired.com.", dns.TypeA, -10*time.Second) // already stale

        tests := []struct {
                name     string
                query    string
                wantNames []string // expected entry names in the response
        }{
                {
                        name:     "no filter — returns all 3 including stale",
                        query:    "",
                        wantNames: []string{"example.com.", "example.com.", "expired.com."},
                },
                {
                        name:     "domain=example.com — exact match",
                        query:    "?domain=example.com",
                        wantNames: []string{"example.com.", "example.com."},
                },
                {
                        name:     "domain_contains=example — substring match (only example.com matches; expired.com does not contain 'example')",
                        query:    "?domain_contains=example",
                        wantNames: []string{"example.com.", "example.com."},
                },
                {
                        name:     "qtype=A — only A records",
                        query:    "?qtype=A",
                        wantNames: []string{"example.com.", "expired.com."},
                },
                {
                        name:     "active=true — only fresh entries",
                        query:    "?active=true",
                        wantNames: []string{"example.com.", "example.com."}, // expired.com excluded
                },
                {
                        name:     "min_ttl=100 — only entries with TTL>=100s",
                        query:    "?min_ttl=100",
                        wantNames: []string{"example.com.", "example.com."}, // expired.com excluded (-10s)
                },
        }

        for _, tc := range tests {
                t.Run(tc.name, func(t *testing.T) {
                        req := httptest.NewRequest(http.MethodGet, "/cache"+tc.query, nil)
                        w := httptest.NewRecorder()
                        p.serveCacheDump(w, req)

                        require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
                        var resp cacheDumpResponse
                        require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
                        gotNames := make([]string, 0, len(resp.Entries))
                        for _, e := range resp.Entries {
                                gotNames = append(gotNames, e.Name)
                        }
                        assert.ElementsMatch(t, tc.wantNames, gotNames)
                })
        }
}

// TestServeCacheDump_MethodNotAllowed verifies non-GET methods are rejected.
func TestServeCacheDump_MethodNotAllowed(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 64
        p := &prog{cfg: cfg}
        p.metricsQueryStats.Store(true)
        cacher, _ := dnscache.NewLRUCache(64)
        p.cache = cacher

        for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
                req := httptest.NewRequest(method, "/cache", strings.NewReader(""))
                w := httptest.NewRecorder()
                p.serveCacheDump(w, req)
                assert.Equal(t, http.StatusMethodNotAllowed, w.Code, "method %s should be rejected", method)
        }
}

// TestServeCacheDump_NoCache verifies the handler returns an empty entries
// array (not nil) when the cache is nil, so the JSON shape is consistent.
func TestServeCacheDump_NoCache(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        p := &prog{cfg: cfg}
        p.metricsQueryStats.Store(true)
        // p.cache is nil

        req := httptest.NewRequest(http.MethodGet, "/cache", nil)
        w := httptest.NewRecorder()
        p.serveCacheDump(w, req)

        require.Equal(t, http.StatusOK, w.Code)
        var resp cacheDumpResponse
        require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
        assert.Equal(t, 0, resp.Count)
        assert.NotNil(t, resp.Entries, "Entries should be an empty slice, not nil, for JSON consistency")
        assert.Empty(t, resp.Entries)
}

// TestProg_RegisterCacheHandler verifies the metricsServer calls
// registerCacheHandler correctly. We can't easily spin up the full server
// in a unit test, so we just verify the handler doesn't blow up when cache
// is nil (graceful no-op).
func TestProg_RegisterCacheHandler_NilCache(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        p := &prog{cfg: cfg}
        // p.cache is nil
        ms := &metricsServer{}
        // Should not panic when cache is nil.
        assert.NotPanics(t, func() {
                ms.registerCacheHandler(p)
        })
}

// TestProg_ServeCacheDump_RaceWithCacheAdd exercises concurrent access:
// one goroutine adds entries to the cache while another walks via the
// HTTP handler. This is a smoke test for the RWMutex semantics of Walk.
func TestProg_ServeCacheDump_RaceWithCacheAdd(t *testing.T) {
        cfg := testhelper.SampleConfig(t)
        cfg.Service.CacheEnable = true
        cfg.Service.CacheSize = 256
        p := &prog{cfg: cfg}
        p.metricsQueryStats.Store(true)
        cacher, _ := dnscache.NewLRUCache(256)
        p.cache = cacher

        ctx, cancel := context.WithCancel(context.Background())
        defer cancel()

        // Writer goroutine: add entries in a loop.
        go func() {
                i := 0
                for {
                        select {
                        case <-ctx.Done():
                                return
                        default:
                                name := "host" + string(rune('a'+(i%26))) + ".test."
                                msg := new(dns.Msg)
                                msg.SetQuestion(name, dns.TypeA)
                                msg.Answer = append(msg.Answer, &dns.A{
                                        Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
                                        A:   []byte{byte(i), 2, 3, 4},
                                })
                                p.cache.Add(
                                        dnscache.NewKey(msg, "upstream.0"),
                                        dnscache.NewValue(msg, time.Now().Add(60*time.Second)),
                                )
                                i++
                        }
                }
        }()

        // Reader goroutine: hit /cache endpoint repeatedly.
        for i := 0; i < 100; i++ {
                req := httptest.NewRequest(http.MethodGet, "/cache", nil)
                w := httptest.NewRecorder()
                p.serveCacheDump(w, req)
                if w.Code != http.StatusOK {
                        t.Errorf("request %d: status %d, body: %s", i, w.Code, w.Body.String())
                        continue
                }
                var resp cacheDumpResponse
                if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
                        t.Errorf("request %d: bad JSON: %v, body: %s", i, err, w.Body.String())
                }
        }
}
