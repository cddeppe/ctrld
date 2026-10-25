# ctrld (fork with domain-graph prefetch)

![Latest Release](https://img.shields.io/github/v/release/cddeppe/ctrld?include_prereleases)
![Test](https://github.com/Control-D-Inc/ctrld/actions/workflows/ci.yml/badge.svg)

> **Fork of [Control-D-Inc/ctrld](https://github.com/Control-D-Inc/ctrld)** with a **persistent domain-graph prefetch** that learns which domains are queried together and speculatively prefetches the predicted neighbors — so the next query hits the cache instead of going upstream.

## What this fork adds

A **learning co-occurrence graph** that tracks which domains are queried within 60 seconds of each other (per client IP), then **speculatively prefetches** the predicted neighbors when a trigger domain is queried. This converts the 2nd–Nth queries in a browsing session to cache hits.

### Key features

| Feature | Description |
|---|---|
| **Persistent habit memory** | Co-occurrence counts never decay. A pair seen 5+ times is always predictable, even weeks later. |
| **Ranking formula** | `rank = count × 2^(-age / halfLife)` — fresh patterns rank higher; stale habits keep their relative count order. |
| **State file persistence** | Graph saved to `/var/lib/ctrld/domain-graph.jsonl` once per day at midnight + on clean shutdown. Loaded on startup so week-old patterns survive reboots. |
| **Cache-hit learning** | Every query (hit or miss) feeds the graph. Not just first-time resolves. |
| **Flash-safe** | ~3MB write/day (state file only). Per-query log disabled by default. ~1GB/year — negligible for any storage. |
| **UniFi firmware-update survival** | Set `domain_prefetch_state_path = "/data/ctrld/domain-graph.jsonl"` so habits survive UniFi OS firmware updates. |
| **Self-contained** | No external API dependency. Works for all account types (personal, business, enterprise). |
| **Trigger-domain logging** | After replay, logs the top trigger domains learned: `trigger netflix.com -> [nflximg.com, nflxext.com, ...]` |

### Expected impact

| Workload | Cache hit ratio (stock ctrld) | With domain-graph prefetch |
|---|---|---|
| Home/SMB LAN (5-10 clients) | ~70-75% | **88-93%** |
| Personal VPS | ~60-70% | **80-85%** |
| Heavy CDN/streaming | ~65-75% | **80-88%** |

## Install

```bash
# Auto-detects existing ctrld install, backs up, replaces, restarts:
curl -fsSL -o /tmp/ctrld-install.sh \
  https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/install.sh \
  && sh /tmp/ctrld-install.sh
```

Force a specific arch:
```bash
sh /tmp/ctrld-install.sh --arch amd64   # or arm64
```

Pin a specific version:
```bash
sh /tmp/ctrld-install.sh --version v1.5.8-domain-prefetch.9
```

## Config knobs (all optional — sensible defaults)

```toml
[service]
  cache_enable = true
  cache_size = 16384                              # bigger cache = better hit ratio

  # Domain-graph prefetch (ON BY DEFAULT when cache_enable=true):
  domain_prefetch_min_co_occurrence = 5           # min count to predict a pair
  domain_prefetch_top_k = 5                        # max neighbors to prefetch per query
  domain_prefetch_max_graph_size = 25000           # LRU cap on distinct domains
  domain_prefetch_window_sec = 60                  # co-occurrence time window

  # Habit memory (persistent graph):
  domain_prefetch_memory_half_life_hours = 72      # 3-day ranking freshness
  domain_prefetch_state_save_hour = 0              # midnight save (0-23; -1 disables)
  domain_prefetch_state_path = "/var/lib/ctrld/domain-graph.jsonl"  # default
  # domain_prefetch_state_path = "/data/ctrld/domain-graph.jsonl"   # UniFi

  # Flash-friendly: turn OFF per-query log (state file replaces it):
  domain_prefetch_local_history_enabled = false
```

## Metrics

```bash
curl -s http://127.0.0.1:9101/metrics | grep -E 'domain_prefetch|local_history'
```

| Metric | Type | Description |
|---|---|---|
| `ctrld_domain_prefetch_predictions_total` | Counter | Speculative prefetches scheduled |
| `ctrld_local_history_replays_total` | Counter | Queries replayed from local log |

## How it works

```
DNS query → ctrld resolves + caches → recordQuery(clientIP, domain)
                                              ↓
                                    Domain co-occurrence graph
                                    (domain A ↔ domain B: count N)
                                              ↓
                                    predictNeighbors(domain A)
                                    → top-K by rank = count × 2^(-age/halfLife)
                                              ↓
                                    Background prefetch of neighbors
                                    (singleflight-coalesced, NXDOMAIN-only unlearn)
                                              ↓
                                    Next query for neighbor → CACHE HIT ✓

At midnight + on shutdown:
                                    saveState() → /var/lib/ctrld/domain-graph.jsonl

On startup:
                                    loadState() ← /var/lib/ctrld/domain-graph.jsonl
                                    (week-old patterns restored in milliseconds)
```

## Design philosophy

1. **Counts never decay** — habits are lifetime memory. Netflix-on-Saturday should be predictable on Saturday, not relearned every 15 minutes.
2. **Ranking decays, qualification doesn't** — `count × 2^(-age/halfLife)` separates "does this pattern exist?" (permanent) from "should we prefer it right now?" (time-weighted).
3. **Flash-safe** — one ~3MB write/day at midnight + one on shutdown. Per-query log disabled by default. ~1GB/year.
4. **Self-contained** — no external API, no cloud dependency, no token needed. Works for everyone.
5. **Every query feeds the graph** — cache hits AND misses call `recordQuery`. Without this, the graph only learns from first-time resolves and never accumulates co-occurrences.

## Release history

| Release | Key change |
|---|---|
| **.9** | Cache hits feed the graph + WARN-level logging |
| .8 | Persistent habit memory (no decay, state file, nightly save) |
| .7 | 5 critical bug fixes (timestamps, eviction, memory leak, NXDOMAIN-only, fd leak) |
| .6 | Flush buffer on every write |
| .5 | Self-contained local query log (replaced Control-D API) |
| .4 | Device ID from --cd flag + INFO logging |
| .3 | Weighted history + trigger logging + config token |
| .2 | API history replay (didn't work — 403 for personal accounts) |
| .1 | Learning co-occurrence graph (basic) |

## Rollback

```bash
curl -fsSL -o /tmp/ctrld-rollback.sh \
  https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/rollback.sh \
  && sh /tmp/ctrld-rollback.sh
```

Or downgrade to a specific version:
```bash
sh /tmp/ctrld-install.sh --version v1.5.8-domain-prefetch.9
```

---

# Upstream ctrld (unchanged below)

![ctrld splash image](/docs/ctrldsplash.png)

A highly configurable DNS forwarding proxy with support for:
- Multiple listeners for incoming queries
- Multiple upstreams with fallbacks
- Multiple network policy driven DNS query steering (via network cidr, MAC address or FQDN)
- Policy driven domain based "split horizon" DNS with wildcard support
- Integrations with common router vendors and firmware
- LAN client discovery via DHCP, mDNS, ARP, NDP, hosts file parsing
- Prometheus metrics exporter

## TLDR
Proxy legacy DNS traffic to secure DNS upstreams in highly configurable ways.

All DNS protocols are supported, including:
- `UDP 53`
- `DNS-over-HTTPS`
- `DNS-over-TLS`
- `DNS-over-HTTP/3` (DOH3)
- `DNS-over-QUIC`

# Use Cases
1. Use secure DNS protocols on networks and devices that don't natively support them (legacy routers, legacy OSes, TVs, smart toasters).
2. Create source IP based DNS routing policies with variable secure DNS upstreams. Subnet 1 (admin) uses upstream resolver A, while Subnet 2 (employee) uses upstream resolver B.
3. Create destination IP based DNS routing policies with variable secure DNS upstreams. Listener 1 uses upstream resolver C, while Listener 2 uses upstream resolver D.
4. Create domain level "split horizon" DNS routing policies to send internal domains (*.company.int) to a local DNS server, while everything else goes to another upstream.
5. Deploy on a router and create LAN client specific DNS routing policies from a web GUI (When using ControlD.com).

## OS Support
- Windows (386, amd64, arm)
- Windows Server (386, amd64)
- MacOS (amd64, arm64)
- Linux (386, amd64, arm, mips)
- FreeBSD (386, amd64, arm)
- Common routers (See below)

### Supported Routers
You can run `ctrld` on any supported router. The list of supported routers and firmware includes:
- Asus Merlin
- DD-WRT
- Firewalla
- FreshTomato
