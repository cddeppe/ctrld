#!/bin/sh
# utr-ctrld-setup.sh — install ctrld on the Ubiquiti UTR (UniFi Travel Router),
# firmware BZ.6.6.122, with PROPER persistence via cfgmtd.
#
# THE ROOT CAUSE OF ALL PREVIOUS FAILURES:
#   The UTR rootfs is an initramfs loaded into RAM at boot. Every file in
#   /etc/, /data/, /tmp/ is RAM-only tmpfs. Writing a file and rebooting
#   without running `cfgmtd -w -p /etc/` is the same as writing to /dev/null.
#
# THE FIX:
#   `cfgmtd -w -p /etc/` snapshots /etc/ to the cfg MTD partition (flash).
#   At boot, rc.sysinit runs `cfgmtd -r -p /etc/` to restore it.
#   So: write our stuff to /etc/, run cfgmtd -w -p /etc/, and it survives.
#
# THE SIZE PROBLEM:
#   The cfg partition is small (typically 256KB - 4MB on BZ firmware). The
#   ctrld binary is ~15MB — won't fit. Solution: persist only the small
#   files (config, bootstrap, state file) via cfgmtd. Download the binary
#   to /tmp/ on every boot.
#
#   State file growth is capped by `domain_prefetch_max_graph_size` (default
#   25000 domains ≈ 3MB). If you need more, flash OpenWrt (the UTR has
#   official upstream OpenWrt support as `ubnt_utr`).
#
# INIT HOOK:
#   /etc/rc.local — Ubiquiti's standard user startup hook on BZ firmware.
#   Sourced at the end of the S* rc.d sequence.
#
# USAGE:
#   sh utr-ctrld-setup.sh --resolver-id <id>
#   sh utr-ctrld-setup.sh --resolver-id <id> --version v1.5.9-prefetch.1
#   sh utr-ctrld-setup.sh --uninstall
#
set -eu

REPO="cddeppe/ctrld"
DEFAULT_VERSION="v1.5.9-prefetch.1"
ARCH="arm"

VERSION="$DEFAULT_VERSION"
RESOLVER_ID=""
LISTENER="0.0.0.0:53"
METRICS="127.0.0.1:9101"
UNINSTALL=0
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --version)     VERSION="$2"; shift 2 ;;
        --resolver-id) RESOLVER_ID="$2"; shift 2 ;;
        --listener)    LISTENER="$2"; shift 2 ;;
        --metrics)     METRICS="$2"; shift 2 ;;
        --uninstall)   UNINSTALL=1; shift ;;
        --dry-run)     DRY_RUN=1; shift ;;
        -h|--help) sed -n '3,40p' "$0"; exit 0 ;;
        *) echo "Unknown flag: $1" >&2; exit 1 ;;
    esac
done

CTRLED_DIR="/etc/ctrld"
CTRLD_BIN_RAM="/tmp/ctrld/ctrld"           # RAM-only binary location
CTRLD_CFG="${CTRLED_DIR}/ctrld.toml"
CTRLD_STATE="${CTRLED_DIR}/domain-graph.jsonl"   # cfgmtd-persisted
CTRLD_LOGFILE="/tmp/ctrld/ctrld.log"
CTRLD_PIDFILE="/tmp/ctrld/ctrld.pid"
BOOTSTRAP="${CTRLED_DIR}/ctrld-bootstrap.sh"
RC_LOCAL="/etc/rc.local"
SHUTDOWN_SCRIPT="/etc/init.d/ctrld-save"
SHUTDOWN_LINK="/etc/rc.d/K01ctrld-save"

# --- sanity: are we on a UniFi/Ubiquiti device? ---
if [ ! -f /etc/inittab ] && ! grep -q '::sysinit:' /etc/inittab 2>/dev/null; then
    echo "ERROR: This doesn't look like a Ubiquiti BZ firmware device." >&2
    echo "       No /etc/inittab with ::sysinit: entry." >&2
    exit 1
fi

# --- detect persistence tool ---
PERSIST_CMD=""
if command -v cfgmtd >/dev/null 2>&1; then
    PERSIST_CMD="cfgmtd -w -p /etc/"
elif command -v syswrapper.sh >/dev/null 2>&1; then
    PERSIST_CMD="syswrapper.sh save-config"
else
    echo "ERROR: neither cfgmtd nor syswrapper.sh found on this device." >&2
    echo "       Without one of these, persistence is impossible on stock firmware." >&2
    echo "       Recommendation: flash OpenWrt (UTR has official upstream support)." >&2
    echo "       See: https://github.com/hryamzik/unifi for build scripts." >&2
    exit 1
fi

# --- uninstall path ---
if [ "$UNINSTALL" = "1" ]; then
    echo "==> Uninstalling ctrld from UTR"
    if [ -f "$CTRLD_PIDFILE" ]; then
        PID="$(cat "$CTRLD_PIDFILE" 2>/dev/null || true)"
        if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
            echo "    stopping ctrld (pid=$PID)"
            kill -TERM "$PID" 2>/dev/null || true
            sleep 2
            kill -0 "$PID" 2>/dev/null && kill -KILL "$PID" 2>/dev/null || true
        fi
        rm -f "$CTRLD_PIDFILE"
    fi
    echo "    removing $CTRLED_DIR"
    rm -rf "$CTRLED_DIR"
    echo "    removing $CTRLD_BIN_RAM"
    rm -f "$CTRLD_BIN_RAM"
    # Remove rc.local ctrld lines
    if [ -f "$RC_LOCAL" ]; then
        grep -v 'ctrld' "$RC_LOCAL" > "${RC_LOCAL}.new" && mv "${RC_LOCAL}.new" "$RC_LOCAL"
    fi
    echo "    removing shutdown hook $SHUTDOWN_SCRIPT + $SHUTDOWN_LINK"
    rm -f "$SHUTDOWN_SCRIPT" "$SHUTDOWN_LINK"
    echo "    persisting /etc/ to flash (removing ctrld entries)..."
    $PERSIST_CMD
    echo "    Done. State file may still be in flash as /etc/ctrld/ — gone after next cfgmtd -w."
    exit 0
fi

# --- main install ---
echo "==> Installing ctrld $VERSION on UTR (cfgmtd-based persistence)"
echo "    persistent dir:    $CTRLED_DIR  (cfgmtd-persisted)"
echo "    binary (RAM):     $CTRLD_BIN_RAM  (downloaded every boot)"
echo "    state file:        $CTRLD_STATE  (cfgmtd-persisted, capped by max_graph_size)"
echo "    bootstrap:         $BOOTSTRAP"
echo "    init hook:         $RC_LOCAL"
echo "    shutdown hook:     $SHUTDOWN_SCRIPT  (saves state via cfgmtd on shutdown)"
echo "    persistence tool:  $PERSIST_CMD"
echo "    listener:          $LISTENER"
echo "    metrics:           $METRICS"
[ -n "$RESOLVER_ID" ] && echo "    resolver-id:       $RESOLVER_ID"

if [ "$DRY_RUN" = "1" ]; then
    echo "    [dry-run] would download ctrld binary to $CTRLD_BIN_RAM"
    echo "    [dry-run] would write $CTRLD_CFG"
    echo "    [dry-run] would write bootstrap to $BOOTSTRAP"
    echo "    [dry-run] would patch $RC_LOCAL"
    echo "    [dry-run] would write shutdown hook"
    echo "    [dry-run] would run: $PERSIST_CMD"
    echo "    [dry-run] would start ctrld now"
    exit 0
fi

mkdir -p "$CTRLED_DIR" /tmp/ctrld

# --- 1. download ctrld binary to /tmp/ (RAM, this boot only) ---
BIN_NAME="ctrld-${VERSION#v}-linux-${ARCH}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BIN_NAME}"

echo
echo "==> Downloading ctrld binary to $CTRLD_BIN_RAM"
echo "    url: $DOWNLOAD_URL"

if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$CTRLD_BIN_RAM" "$DOWNLOAD_URL" || { echo "download failed" >&2; exit 1; }
elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$CTRLD_BIN_RAM" "$DOWNLOAD_URL" || { echo "download failed" >&2; exit 1; }
else
    echo "ERROR: neither curl nor wget found." >&2
    exit 1
fi
chmod 0755 "$CTRLD_BIN_RAM"
"$CTRLD_BIN_RAM" --version 2>&1 | head -1 | sed 's/^/    /'

# --- 2. write ctrld.toml (cfgmtd-persisted) ---
write_config() {
    UPSTREAM_URL="https://freedns.controld.com/p2"
    UPSTREAM_NAME="controld-public"
    if [ -n "$RESOLVER_ID" ]; then
        UPSTREAM_URL="https://dns.controld.com/${RESOLVER_ID}"
        UPSTREAM_NAME="controld-personal"
    fi

    cat > "$CTRLD_CFG" <<EOF
# ctrld.toml — UTR travel-router config (auto-generated by utr-ctrld-setup.sh)
# State file is in $CTRLED_DIR/ — cfgmtd-persisted across reboots.
# Capped at 25000 domains (~3MB) to fit in the cfg partition.

[service]
  log_level = "info"
  cache_enable = true
  cache_size = 16384
  cache_serve_stale = true

  # Prefetch (workload-driven): refresh cache entries when 25% of TTL remains.
  cache_prefetch_threshold = 0.25
  cache_prefetch_min_ttl = 10

  # Domain-graph prefetch: learn domain co-occurrence patterns.
  domain_prefetch_min_co_occurrence = 5
  domain_prefetch_top_k = 5
  domain_prefetch_max_graph_size = 25000          # capped for cfg partition fit
  domain_prefetch_memory_half_life_hours = 72
  domain_prefetch_state_path = "$CTRLD_STATE"     # /etc/ — cfgmtd-persisted
  domain_prefetch_local_history_enabled = false

  # Metrics — verify with: curl http://127.0.0.1:9101/metrics | grep ctrld_
  metrics_listener = "$METRICS"
  metrics_query_stats = true

[listeners]
  [listeners.l0]
  ip = "${LISTENER%%:*}"
  port = "${LISTENER##*:}"

[upstreams]
  [upstreams.u0]
  type = "doh"
  endpoint = "$UPSTREAM_URL"
  name = "$UPSTREAM_NAME"
EOF
}

if [ -f "$CTRLD_CFG" ]; then
    echo
    echo "==> Existing config at $CTRLD_CFG — preserving (delete it to regenerate)"
    echo "    current contents:"
    sed 's/^/    /' "$CTRLD_CFG"
else
    echo
    echo "==> Writing $CTRLD_CFG"
    write_config
    echo "    upstream: $(grep '^  endpoint' "$CTRLD_CFG" | head -1 | tr -s ' ')"
fi

# --- 3. write the bootstrap script (cfgmtd-persisted) ---
# This runs on every boot via /etc/rc.local.
# It downloads the binary if missing, waits for default route, starts ctrld.
echo
echo "==> Writing bootstrap to $BOOTSTRAP"

cat > "$BOOTSTRAP" <<EOF
#!/bin/sh
# ctrld-bootstrap.sh — runs on every boot via /etc/rc.local.
# Auto-deployed by utr-ctrld-setup.sh. Idempotent — safe to call multiple times.
set -eu

CTRLD_BIN="$CTRLD_BIN_RAM"
CTRLD_CFG="$CTRLD_CFG"
CTRLD_PIDFILE="$CTRLD_PIDFILE"
CTRLD_LOGFILE="$CTRLD_LOGFILE"
CTRLD_DIR_TMP="/tmp/ctrld"

# Already running?
if [ -f "\$CTRLD_PIDFILE" ]; then
    PID="\$(cat "\$CTRLD_PIDFILE" 2>/dev/null || true)"
    if [ -n "\$PID" ] && kill -0 "\$PID" 2>/dev/null; then
        CMDLINE="\$(cat /proc/\$PID/cmdline 2>/dev/null | tr '\\0' ' ' || true)"
        case "\$CMDLINE" in
            *ctrld*run*) echo "[ctrld-bootstrap] already running (pid=\$PID)"; exit 0 ;;
        esac
    fi
    rm -f "\$CTRLD_PIDFILE"
fi

mkdir -p "\$CTRLD_DIR_TMP"

# Rotate log if >2MB.
if [ -f "\$CTRLD_LOGFILE" ]; then
    LOGSIZE="\$(wc -c < "\$CTRLD_LOGFILE" 2>/dev/null || echo 0)"
    [ "\$LOGSIZE" -gt 2097152 ] && mv "\$CTRLD_LOGFILE" "\${CTRLD_LOGFILE}.1"
fi

# Download binary if missing.
if [ ! -x "\$CTRLD_BIN" ]; then
    echo "[ctrld-bootstrap] binary missing, downloading \$DOWNLOAD_URL"
    TRIES=0
    while [ "\$TRIES" -lt 60 ]; do
        # Wait for default route (DNS + internet needed).
        ip route show default 2>/dev/null | grep -q '^default' && break
        sleep 2; TRIES=\$((TRIES + 1))
    done
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "\$CTRLD_BIN" "$DOWNLOAD_URL" || { echo "[ctrld-bootstrap] download failed" >&2; exit 1; }
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "\$CTRLD_BIN" "$DOWNLOAD_URL" || { echo "[ctrld-bootstrap] download failed" >&2; exit 1; }
    else
        echo "[ctrld-bootstrap] no curl/wget" >&2; exit 1
    fi
    chmod 0755 "\$CTRLD_BIN"
fi

# Wait for default route (upstream DoH needs internet).
TRIES=0
while [ "\$TRIES" -lt 30 ]; do
    ip route show default 2>/dev/null | grep -q '^default' && break
    sleep 1; TRIES=\$((TRIES + 1))
done

echo "[ctrld-bootstrap] starting ctrld at \$(date -Iseconds 2>/dev/null || date)"
nohup setsid "\$CTRLD_BIN" run --config "\$CTRLD_CFG" >>"\$CTRLD_LOGFILE" 2>&1 &
CTRLED_PID=\$!
echo "\$CTRLED_PID" > "\$CTRLD_PIDFILE"
sleep 2
if kill -0 "\$CTRLED_PID" 2>/dev/null; then
    echo "[ctrld-bootstrap] ctrld started (pid=\$CTRLED_PID)"
else
    echo "[ctrld-bootstrap] WARNING: ctrld exited — check \$CTRLD_LOGFILE" >&2
    tail -n 20 "\$CTRLD_LOGFILE" >&2 2>/dev/null || true
    rm -f "\$CTRLD_PIDFILE"
    exit 1
fi
[ -f "$CTRLD_STATE" ] && echo "[ctrld-bootstrap] state file: $CTRLD_STATE (\$(wc -l < "$CTRLD_STATE" 2>/dev/null || echo 0) lines)"
exit 0
EOF

chmod +x "$BOOTSTRAP"
echo "    bootstrap installed."

# --- 4. patch /etc/rc.local to call bootstrap (cfgmtd-persisted) ---
echo
echo "==> Patching $RC_LOCAL"

if [ -f "$RC_LOCAL" ]; then
    # Backup before modifying.
    cp -p "$RC_LOCAL" "${RC_LOCAL}.orig" 2>/dev/null || true
    # Remove any prior ctrld entry (idempotent).
    grep -v 'ctrld-bootstrap' "$RC_LOCAL" > "${RC_LOCAL}.new" || true
    mv "${RC_LOCAL}.new" "$RC_LOCAL"
else
    : > "$RC_LOCAL"
fi

# Append our bootstrap call (only if not already present).
if ! grep -q 'ctrld-bootstrap' "$RC_LOCAL" 2>/dev/null; then
    cat >> "$RC_LOCAL" <<EOF

# === ctrld DNS proxy (cddeppe/ctrld fork with domain-graph prefetch) ===
sh $BOOTSTRAP
EOF
fi
chmod +x "$RC_LOCAL"
echo "    $RC_LOCAL patched:"
sed 's/^/    /' "$RC_LOCAL"

# --- 5. write shutdown hook (saves /etc/ to flash on shutdown) ---
echo
echo "==> Writing shutdown hook $SHUTDOWN_SCRIPT"
cat > "$SHUTDOWN_SCRIPT" <<EOF
#!/bin/sh
# ctrld-save — runs at shutdown (K01) via /etc/rc.d/rcS K.
# Commits /etc/ (including state file changes) to flash before reboot.
set -eu

# Stop ctrld cleanly first (so it flushes its state file).
if [ -f "$CTRLD_PIDFILE" ]; then
    PID="\$(cat "$CTRLD_PIDFILE" 2>/dev/null || true)"
    if [ -n "\$PID" ] && kill -0 "\$PID" 2>/dev/null; then
        kill -TERM "\$PID" 2>/dev/null || true
        sleep 2
        kill -0 "\$PID" 2>/dev/null && kill -KILL "\$PID" 2>/dev/null || true
    fi
    rm -f "$CTRLD_PIDFILE"
fi

# Commit /etc/ to flash (saves ctrld state file).
$PERSIST_CMD >/dev/null 2>&1 || true
exit 0
EOF
chmod +x "$SHUTDOWN_SCRIPT"

# Create K* symlink in /etc/rc.d/ so it runs at shutdown.
ln -sf "../init.d/ctrld-save" "$SHUTDOWN_LINK"
echo "    shutdown hook installed at $SHUTDOWN_LINK"

# --- 6. persist /etc/ to flash ---
echo
echo "==> Persisting /etc/ to flash via: $PERSIST_CMD"
echo "    (this is the step that was missing in every previous attempt)"
$PERSIST_CMD
echo "    persisted."

# --- 7. start ctrld now (run the bootstrap) ---
echo
echo "==> Starting ctrld now"
sh "$BOOTSTRAP" || {
    echo "ERROR: bootstrap failed. Check $CTRLD_LOGFILE" >&2
    exit 1
}

# --- 8. verify via metrics ---
sleep 2
echo
echo "==> Verifying via metrics endpoint"
METRICS_HOST="${METRICS%%:*}"
METRICS_PORT="${METRICS##*:}"
if command -v curl >/dev/null 2>&1; then
    BUILD_INFO="$(curl -fsS "http://${METRICS_HOST}:${METRICS_PORT}/metrics" 2>/dev/null | grep '^ctrld_build_info' | head -1 || true)"
    if [ -n "$BUILD_INFO" ]; then
        echo "    $BUILD_INFO"
    else
        echo "    (metrics endpoint not responding yet — give ctrld ~10s)"
    fi
fi

# --- 9. summary ---
echo
echo "==> DONE — ctrld is running, persistence wired up via cfgmtd"
echo
echo "WHAT SURVIVES REBOOT (cfgmtd-persisted in /etc/):"
echo "  $CTRLD_CFG         — config (with your resolver ID)"
echo "  $CTRLD_STATE       — domain-graph state (capped at 25000 domains)"
echo "  $BOOTSTRAP         — bootstrap script (downloads binary at boot)"
echo "  $RC_LOCAL          — patched to call bootstrap on boot"
echo "  $SHUTDOWN_SCRIPT   — saves state via cfgmtd at shutdown"
echo
echo "WHAT DOES NOT SURVIVE REBOOT:"
echo "  $CTRLD_BIN_RAM  — binary (RAM-only, re-downloaded at boot)"
echo "  $CTRLD_LOGFILE  — log (rotated, doesn't matter)"
echo
echo "VERIFY PERSISTENCE — DO THIS NOW:"
echo "  1. Reboot:        reboot"
echo "  2. Wait ~60s for the UTR to come back, then SSH in."
echo "  3. Check ctrld is running:"
echo "       cat $CTRLD_PIDFILE && kill -0 \$(cat $CTRLD_PIDFILE) && echo OK"
echo "  4. Check state file survived:"
echo "       wc -l $CTRLD_STATE"
echo "  5. Check metrics:"
echo "       curl -s http://${METRICS_HOST}:${METRICS_PORT}/metrics | grep ctrld_build_info"
echo
echo "IF REBOOT TEST FAILS (ctrld not running after reboot):"
echo "  - First check rc.local actually ran:  cat /tmp/ctrld_boot_marker (if you add one)"
echo "  - Check the bootstrap log:           cat $CTRLD_LOGFILE"
echo "  - Manually start:                     sh $BOOTSTRAP"
echo
echo "LIMITATION:"
echo "  State file is capped at 25000 domains (~3MB) to fit in the cfg partition."
echo "  If you need more, flash OpenWrt (UTR has official upstream support as 'ubnt_utr')."
echo "  Build scripts: https://github.com/hryamzik/unifi"
echo
echo "To uninstall:"
echo "  sh $0 --uninstall"
