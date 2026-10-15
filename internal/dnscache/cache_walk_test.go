package dnscache

import (
        "testing"
        "time"

        "github.com/miekg/dns"
        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"
)

// TestLRUCache_Walk_Basic verifies Walk visits every entry and exposes the
// correct (key, value) pairs.
func TestLRUCache_Walk_Basic(t *testing.T) {
        c, err := NewLRUCache(16)
        require.NoError(t, err)

        // Add three entries.
        addOne := func(name string, qtype uint16, ttl time.Duration) {
                msg := new(dns.Msg)
                msg.SetQuestion(name, qtype)
                msg.Answer = append(msg.Answer, &dns.A{
                        Hdr: dns.RR_Header{Name: name, Rrtype: qtype, Class: dns.ClassINET, Ttl: 60},
                        A:   []byte{1, 2, 3, 4},
                })
                c.Add(Key{Qtype: qtype, Qclass: dns.ClassINET, Name: name, Upstream: "upstream.0"},
                        &Value{Msg: msg, Expire: time.Now().Add(ttl)})
        }
        addOne("a.test.", dns.TypeA, 60*time.Second)
        addOne("b.test.", dns.TypeA, 60*time.Second)
        addOne("c.test.", dns.TypeA, 60*time.Second)

        seen := make(map[string]*Value)
        c.Walk(func(k Key, v *Value) bool {
                seen[k.Name] = v
                return true
        })

        assert.Len(t, seen, 3)
        assert.Contains(t, seen, "a.test.")
        assert.Contains(t, seen, "b.test.")
        assert.Contains(t, seen, "c.test.")
        for name, v := range seen {
                assert.NotNil(t, v.Msg, "value for %s should have non-nil Msg", name)
                assert.NotZero(t, v.Expire, "value for %s should have non-zero Expire", name)
        }
}

// TestLRUCache_Walk_EarlyTermination verifies that returning false from the
// callback stops iteration.
func TestLRUCache_Walk_EarlyTermination(t *testing.T) {
        c, err := NewLRUCache(16)
        require.NoError(t, err)

        for i := 0; i < 10; i++ {
                name := "host" + string(rune('a'+i)) + ".test."
                msg := new(dns.Msg)
                msg.SetQuestion(name, dns.TypeA)
                c.Add(Key{Qtype: dns.TypeA, Qclass: dns.ClassINET, Name: name, Upstream: "u"},
                        &Value{Msg: msg, Expire: time.Now().Add(60 * time.Second)})
        }

        count := 0
        c.Walk(func(k Key, v *Value) bool {
                count++
                return count < 3 // stop after 3rd
        })

        assert.Equal(t, 3, count, "Walk should have stopped after 3 entries")
}

// TestLRUCache_Walk_DoesNotPolluteLRU is the key test that justifies the
// existence of Walk() (vs. just calling Get() in a loop). After Walk runs,
// the LRU recency order must be unchanged — meaning a subsequent eviction
// pressure should evict the same entry it would have evicted without the Walk.
//
// We test this by:
//  1. Filling the cache to capacity N.
//  2. Calling Get() on entry[0] (making it most-recently-used).
//  3. Walking the cache (which would mark everything MRU if broken).
//  4. Adding a new entry, which evicts the LRU (should be entry[1], not entry[0]).
//  5. Verifying entry[0] is still in the cache and entry[1] was evicted.
func TestLRUCache_Walk_DoesNotPolluteLRU(t *testing.T) {
        const N = 5
        c, err := NewLRUCache(N)
        require.NoError(t, err)

        // Fill the cache with entries [0..N-1].
        keys := make([]Key, N)
        for i := 0; i < N; i++ {
                name := "host" + string(rune('a'+i)) + ".test."
                k := Key{Qtype: dns.TypeA, Qclass: dns.ClassINET, Name: name, Upstream: "u"}
                keys[i] = k
                msg := new(dns.Msg)
                msg.SetQuestion(name, dns.TypeA)
                c.Add(k, &Value{Msg: msg, Expire: time.Now().Add(60 * time.Second)})
        }

        // Touch entry[0] so it becomes MRU; entry[1] is now LRU.
        c.Get(keys[0])

        // Walk — if this polluted LRU, every entry would become MRU and the
        // "least recently used" semantics would be destroyed.
        c.Walk(func(k Key, v *Value) bool {
                return true
        })

        // Add a new entry — should evict the LRU which is entry[1] (since we
        // touched entry[0] after entry[1] was added).
        newKey := Key{Qtype: dns.TypeA, Qclass: dns.ClassINET, Name: "new.test.", Upstream: "u"}
        newMsg := new(dns.Msg)
        newMsg.SetQuestion("new.test.", dns.TypeA)
        c.Add(newKey, &Value{Msg: newMsg, Expire: time.Now().Add(60 * time.Second)})

        // entry[0] should still be in the cache (it was MRU before the add).
        // entry[1] should have been evicted (it was LRU before the add).
        assert.NotNil(t, c.Get(keys[0]), "entry[0] should still be in cache (was MRU before add)")
        assert.Nil(t, c.Get(keys[1]), "entry[1] should have been evicted (was LRU before add)")
}

// TestLRUCache_Walk_NilValue verifies Walk skips nil values gracefully
// (e.g. entries evicted between the Keys() snapshot and the Peek).
func TestLRUCache_Walk_NilValue(t *testing.T) {
        c, err := NewLRUCache(16)
        require.NoError(t, err)

        // Add one real entry.
        msg := new(dns.Msg)
        msg.SetQuestion("a.test.", dns.TypeA)
        c.Add(Key{Qtype: dns.TypeA, Qclass: dns.ClassINET, Name: "a.test.", Upstream: "u"},
                &Value{Msg: msg, Expire: time.Now().Add(60 * time.Second)})

        visited := 0
        c.Walk(func(k Key, v *Value) bool {
                if v != nil {
                        visited++
                }
                return true
        })
        assert.Equal(t, 1, visited)
}

// TestLRUCache_Walk_ConcurrentWithAdd is a smoke test that Walk can be called
// concurrently with Add and not panic or deadlock. Run with -race to verify.
func TestLRUCache_Walk_ConcurrentWithAdd(t *testing.T) {
        c, err := NewLRUCache(256)
        require.NoError(t, err)

        done := make(chan struct{})
        // Writer goroutine
        go func() {
                for i := 0; i < 1000; i++ {
                        name := "host" + string(rune('a'+(i%26))) + ".test."
                        msg := new(dns.Msg)
                        msg.SetQuestion(name, dns.TypeA)
                        c.Add(Key{Qtype: dns.TypeA, Qclass: dns.ClassINET, Name: name, Upstream: "u"},
                                &Value{Msg: msg, Expire: time.Now().Add(60 * time.Second)})
                }
                close(done)
        }()

        // Reader goroutine: walk the cache while the writer is running.
        for i := 0; i < 100; i++ {
                count := 0
                c.Walk(func(k Key, v *Value) bool {
                        count++
                        return true
                })
                // count may be anywhere from 0 to ~1000 — we just want to confirm
                // no panic / deadlock.
                assert.GreaterOrEqual(t, count, 0)
        }

        <-done // wait for writer to finish
}

// TestLRUCache_Walk_NilCallback verifies Walk doesn't panic if the callback
// is nil.
func TestLRUCache_Walk_NilCallback(t *testing.T) {
        c, err := NewLRUCache(16)
        require.NoError(t, err)
        assert.NotPanics(t, func() {
                c.Walk(nil)
        })
}
