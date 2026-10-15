package dnscache

import (
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/miekg/dns"
)

// Cacher is the interface for caching DNS response.
type Cacher interface {
	Get(Key) *Value
	Add(Key, *Value)
	Purge()
	// Walk iterates over all cache entries without updating LRU recency.
	// The callback receives each (key, value) pair; returning false stops
	// iteration. Walk uses Peek() internally, so it does NOT pollute the
	// eviction order — entries walked are not marked as recently-used.
	// Safe for concurrent use; holds only the read lock briefly per entry.
	Walk(func(Key, *Value) bool)
}

// Key is the caching key for DNS message.
type Key struct {
	Qtype    uint16
	Qclass   uint16
	Name     string
	Upstream string
}

type Value struct {
	Expire time.Time
	Msg    *dns.Msg
}

var _ Cacher = (*LRUCache)(nil)

// LRUCache implements Cacher interface.
type LRUCache struct {
	cacher *lru.ARCCache[Key, *Value]
}

// Get looks up key's value from cache.
func (l *LRUCache) Get(key Key) *Value {
	v, _ := l.cacher.Get(key)
	return v
}

// Add adds a value to cache.
func (l *LRUCache) Add(key Key, value *Value) {
	l.cacher.Add(key, value)
}

// Purge clears the cache.
func (l *LRUCache) Purge() {
	l.cacher.Purge()
}

// Walk iterates over all cache entries without updating LRU recency.
// The callback receives each (key, value) pair; returning false stops
// iteration. Uses Peek() under the hood so eviction order is preserved
// (i.e. walking the cache does NOT mark entries as recently-used, which
// would otherwise defeat the LRU policy).
//
// Snapshot semantics: the key list is obtained once at the start; entries
// added or evicted during the walk will not be visited. Values are Peek'd
// per-key, so a value that's evicted between the key list snapshot and the
// Peek call will be skipped (Peek returns ok=false).
//
// Safe for concurrent use with Get/Add/Purge — those operations take the
// write lock briefly; Walk takes the read lock per Peek. Worst-case
// contention is microsecond-level per entry.
func (l *LRUCache) Walk(fn func(Key, *Value) bool) {
	if l == nil || l.cacher == nil || fn == nil {
		return
	}
	// Keys() returns a snapshot slice — takes RLock briefly, then releases.
	// Iterating the snapshot does not hold any lock.
	keys := l.cacher.Keys()
	for _, k := range keys {
		// Peek takes RLock briefly per call. Returns (value, ok) — if ok
		// is false, the entry was evicted between the snapshot and now.
		v, ok := l.cacher.Peek(k)
		if !ok || v == nil {
			continue
		}
		if !fn(k, v) {
			return
		}
	}
}

// NewLRUCache creates a new LRUCache instance with given size.
func NewLRUCache(size int) (*LRUCache, error) {
	cacher, err := lru.NewARC[Key, *Value](size)
	return &LRUCache{cacher: cacher}, err
}

// NewKey creates a new cache key for given DNS message.
func NewKey(msg *dns.Msg, upstream string) Key {
	q := msg.Question[0]
	return Key{Qtype: q.Qtype, Qclass: q.Qclass, Name: normalizeQname(q.Name), Upstream: upstream}
}

// NewValue creates a new cache value for given DNS message.
func NewValue(msg *dns.Msg, expire time.Time) *Value {
	return &Value{
		Expire: expire,
		Msg:    msg,
	}
}

func normalizeQname(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}
