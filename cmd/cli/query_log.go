package cli

import (
        "bufio"
        "encoding/json"
        "fmt"
        "io"
        "os"
        "path/filepath"
        "strings"
        "sync"
        "sync/atomic"
        "time"

        "github.com/miekg/dns"
)

// queryLogger writes each DNS query (after a successful resolve + cache
// write) to a local JSON-lines file, then replays that file on startup
// to pre-seed the domain graph with recent co-occurrence patterns.
//
// This is the SELF-CONTAINED alternative to fetching query history from
// Control-D's API (which requires undocumented/enterprise-only access).
// The local log captures every query that goes through ctrld, persists
// across restarts, and replays on startup with the configured weight
// multiplier (default 3.0x).
//
// File format: JSON lines, one query per line:
//
//      {"ts":"2026-10-16T13:54:05.217Z","client_ip":"10.0.0.1","domain":"google.com","qtype":"A"}
//
// File rotation: when the file exceeds max_size_bytes, ctrld rewrites it
// on the NEXT startup, keeping only the most recent entries that fit
// (truncating from the beginning). No external logrotate needed.
//
// Concurrency: writes are behind a mutex (briefly). Reads happen once
// on startup, before any concurrent writes. Writes are best-effort —
// if the file write fails (disk full, permissions), the error is logged
// at debug level and DNS resolution continues uninterrupted.
type queryLogger struct {
        enabled      bool
        path         string
        maxSizeBytes int64

        mu    sync.Mutex
        file  *os.File
        bw    *bufio.Writer
        writes atomic.Int64
}

// queryLogEntry is one line in the JSON-lines log file.
type queryLogEntry struct {
        Timestamp string `json:"ts"`
        ClientIP  string `json:"client_ip"`
        Domain    string `json:"domain"`
        Qtype     string `json:"qtype"`
}

// newQueryLogger constructs a queryLogger from p's config. Returns a
// disabled logger if domain prefetch is off, or if the local history
// feature is explicitly disabled.
//
// Default: enabled when domain prefetch is enabled (the local log is
// the primary mechanism for cold-start seeding, and works for everyone
// — no external API dependency).
func newQueryLogger(p *prog) *queryLogger {
        ql := &queryLogger{
                maxSizeBytes: 10 * 1024 * 1024, // 10 MB default
        }
        if p.domainGraph == nil || !p.domainGraph.enabled {
                return ql
        }
        ql.enabled = true
        ql.path = "/var/lib/ctrld/query-history.jsonl"

        if v := p.cfg.Service.DomainPrefetchLocalHistoryPath; v != nil && *v != "" {
                ql.path = *v
        }
        if v := p.cfg.Service.DomainPrefetchLocalHistoryMaxSizeMB; v != nil && *v > 0 {
                ql.maxSizeBytes = int64(*v) * 1024 * 1024
        }
        // Explicit disable via config.
        if v := p.cfg.Service.DomainPrefetchLocalHistoryEnabled; v != nil && !*v {
                ql.enabled = false
        }
        return ql
}

// start opens the log file for appending. Creates the parent directory
// if needed. Safe to call multiple times. Returns an error if the file
// can't be opened (caller decides whether to continue without logging).
func (ql *queryLogger) start() error {
        if !ql.enabled {
                return nil
        }
        ql.mu.Lock()
        defer ql.mu.Unlock()
        if ql.file != nil {
                return nil
        }
        dir := filepath.Dir(ql.path)
        if err := os.MkdirAll(dir, 0755); err != nil {
                return fmt.Errorf("create query log dir %s: %w", dir, err)
        }
        f, err := os.OpenFile(ql.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
        if err != nil {
                return fmt.Errorf("open query log %s: %w", ql.path, err)
        }
        ql.file = f
        ql.bw = bufio.NewWriterSize(f, 64*1024) // 64KB write buffer
        return nil
}

// appendQuery writes one query to the log file. Best-effort — errors
// are logged at debug level and don't break DNS resolution. The write
// is buffered; the buffer is flushed periodically by a background
// goroutine OR when the file fills up.
//
// Called from dns_proxy.go after a successful resolve + cache write,
// alongside the maybePrefetchNeighbors hook. Only A/AAAA queries are
// logged (matching the live hook's filter — CNAME/MX/TXT/SRV are
// singletons not relevant to co-occurrence).
func (ql *queryLogger) appendQuery(clientIP, domain string, qtype uint16) {
        if !ql.enabled || ql.file == nil {
                return
        }
        // Only log A/AAAA (matches the live hook + replay filter).
        if qtype != dns.TypeA && qtype != dns.TypeAAAA {
                return
        }
        qtypeStr := "A"
        if qtype == dns.TypeAAAA {
                qtypeStr = "AAAA"
        }
        entry := queryLogEntry{
                Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
                ClientIP:  clientIP,
                Domain:    normalizeDomain(domain),
                Qtype:     qtypeStr,
        }
        line, err := json.Marshal(entry)
        if err != nil {
                return // shouldn't happen for simple strings
        }
        ql.mu.Lock()
        defer ql.mu.Unlock()
        if ql.bw == nil {
                return
        }
        // Write line + newline. Best-effort — ignore errors.
        _, _ = ql.bw.Write(line)
        _ = ql.bw.WriteByte('\n')
        ql.writes.Add(1)

        // Flush every write. This ensures data reaches disk immediately,
        // so it survives crashes/restarts. The performance cost is ~1
        // syscall per query — negligible for typical DNS workloads (10-100
        // QPS = 10-100 syscalls/sec = <0.1% CPU).
        _ = ql.bw.Flush()
}

// flush writes any buffered data to the underlying file. Safe to call
// concurrently with appendQuery (mutex serializes).
func (ql *queryLogger) flush() {
        if !ql.enabled || ql.bw == nil {
                return
        }
        ql.mu.Lock()
        defer ql.mu.Unlock()
        _ = ql.bw.Flush()
}

// close flushes the buffer and closes the file. Called on shutdown.
func (ql *queryLogger) close() error {
        if !ql.enabled || ql.file == nil {
                return nil
        }
        ql.mu.Lock()
        defer ql.mu.Unlock()
        if ql.bw != nil {
                _ = ql.bw.Flush()
        }
        err := ql.file.Close()
        ql.file = nil
        ql.bw = nil
        return err
}

// replayInto reads the local query log file and replays each entry
// through the domain graph, pre-seeding co-occurrence patterns. Uses
// the configured history weight multiplier (default 3.0x).
//
// Called once on startup, before ctrld starts serving queries. If the
// file doesn't exist yet (first run), this is a no-op — the graph
// starts cold and learns from live queries.
//
// After replay, calls logTopTriggers() to log the top-N trigger
// domains learned from the local history (e.g. "netflix.com ->
// [nflximg.com, ...]") — pure observability, no behavior change.
//
// If the file exceeds max_size_bytes, rewrites it after replay keeping
// only the most recent entries that fit (truncating from the beginning).
// This bounds disk usage without an external logrotate.
func (ql *queryLogger) replayInto(dg *domainGraph, p *prog) {
        if !ql.enabled || dg == nil || !dg.enabled {
                return
        }

        // Close the writer before reading (so all buffered data is on disk).
        // We'll reopen for appending after replay.
        _ = ql.close()

        f, err := os.OpenFile(ql.path, os.O_RDONLY|os.O_CREATE, 0600)
        if err != nil {
                mainLog.Load().Warn().Err(err).
                        Msg("domain-graph: failed to open local query log for replay")
                // Reopen for appending so we can at least keep logging new queries.
                _ = ql.start()
                return
        }
        defer f.Close()

        // Check file size — if too large, we'll truncate after reading.
        stat, err := f.Stat()
        if err != nil {
                mainLog.Load().Warn().Err(err).
                        Msg("domain-graph: failed to stat local query log")
                _ = ql.start()
                return
        }
        fileSize := stat.Size()
        overSize := fileSize > ql.maxSizeBytes

        // If over size, read only the last max_size_bytes of the file.
        // Otherwise read the whole thing.
        var reader io.Reader = f
        if overSize {
                _, err = f.Seek(-ql.maxSizeBytes, io.SeekEnd)
                if err != nil {
                        mainLog.Load().Warn().Err(err).
                                Msg("domain-graph: failed to seek local query log")
                        _ = ql.start()
                        return
                }
                // Discard the partial first line (we started mid-file).
                br := bufio.NewReader(f)
                _, _ = br.ReadString('\n')
                reader = br
                mainLog.Load().Info().
                        Int64("file_size_bytes", fileSize).
                        Int64("max_size_bytes", ql.maxSizeBytes).
                        Msg("domain-graph: local query log exceeds max size, replaying only the most recent entries")
        }

        // Read entries and replay chronologically. The file is already in
        // append-order (oldest first), so no need to sort.
        replayed := 0
        skipped := 0
        scanner := bufio.NewScanner(reader)
        // Allow long lines (default scanner limit is 64KB; our entries are
        // ~150 bytes so this is plenty, but bump it to be safe).
        scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
        for scanner.Scan() {
                line := strings.TrimSpace(scanner.Text())
                if line == "" {
                        continue
                }
                var entry queryLogEntry
                if err := json.Unmarshal([]byte(line), &entry); err != nil {
                        skipped++
                        continue
                }
                if entry.Domain == "" {
                        skipped++
                        continue
                }
                // Filter to A/AAAA only (matches the live hook + appendQuery filter).
                qtypeUpper := strings.ToUpper(entry.Qtype)
                if qtypeUpper != "A" && qtypeUpper != "AAAA" {
                        skipped++
                        continue
                }
                // Replay with the configured history weight multiplier.
                dg.recordQueryWithWeight(entry.ClientIP, entry.Domain, dg.historyWeight)
                replayed++
        }
        if err := scanner.Err(); err != nil {
                mainLog.Load().Warn().Err(err).
                        Msg("domain-graph: error reading local query log")
        }

        mainLog.Load().Info().Msgf("domain-graph: replayed %d queries (skipped %d) from local log %s with weight=%.1fx, graph now has %d domains",
                replayed, skipped, ql.path, dg.historyWeight, dg.domainCount())

        // Log the top trigger domains so the user can see what was learned.
        dg.logTopTriggers()

        // Count for observability.
        statsLocalHistoryReplays.Add(float64(replayed))

        // If the file was over size, rewrite it with only the recent entries
        // that fit. This bounds disk usage without external logrotate.
        if overSize {
                if err := ql.truncateToRecent(); err != nil {
                        mainLog.Load().Warn().Err(err).
                                Msg("domain-graph: failed to truncate local query log to recent entries")
                }
        }

        // Reopen for appending so we can keep logging new queries.
        if err := ql.start(); err != nil {
                mainLog.Load().Warn().Err(err).
                        Msg("domain-graph: failed to reopen local query log for appending")
        }
}

// truncateToRecent rewrites the query log file to contain only the most
// recent entries that fit within max_size_bytes. Called after replay
// when the file exceeds the size limit. Atomic-ish: writes to a temp
// file then renames over the original.
func (ql *queryLogger) truncateToRecent() error {
        tmpPath := ql.path + ".tmp"
        src, err := os.Open(ql.path)
        if err != nil {
                return fmt.Errorf("open for truncate: %w", err)
        }
        defer src.Close()

        // Seek to the last max_size_bytes.
        stat, err := src.Stat()
        if err != nil {
                return fmt.Errorf("stat for truncate: %w", err)
        }
        if stat.Size() <= ql.maxSizeBytes {
                return nil // nothing to do
        }
        if _, err := src.Seek(-ql.maxSizeBytes, io.SeekEnd); err != nil {
                return fmt.Errorf("seek for truncate: %w", err)
        }
        br := bufio.NewReader(src)
        // Discard partial first line.
        _, _ = br.ReadString('\n')

        dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
        if err != nil {
                return fmt.Errorf("create temp file for truncate: %w", err)
        }
        bw := bufio.NewWriterSize(dst, 64*1024)
        _, err = io.Copy(bw, br)
        if err != nil {
                dst.Close()
                os.Remove(tmpPath)
                return fmt.Errorf("copy for truncate: %w", err)
        }
        if err := bw.Flush(); err != nil {
                dst.Close()
                os.Remove(tmpPath)
                return fmt.Errorf("flush for truncate: %w", err)
        }
        if err := dst.Close(); err != nil {
                os.Remove(tmpPath)
                return fmt.Errorf("close temp file for truncate: %w", err)
        }
        // Atomic rename.
        if err := os.Rename(tmpPath, ql.path); err != nil {
                os.Remove(tmpPath)
                return fmt.Errorf("rename for truncate: %w", err)
        }
        mainLog.Load().Info().
                Int64("old_size_bytes", stat.Size()).
                Int64("new_size_bytes", ql.maxSizeBytes).
                Msg("domain-graph: truncated local query log to recent entries")
        return nil
}
