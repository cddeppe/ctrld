#!/bin/sh
# utr-ctrld-setup.sh — one-time setup script for the prefetch-enabled ctrld
# on the Ubiquiti UniFi Travel Router (UTR).
#
# PROBLEM:
#   The UTR runs UniFi OS with a tmpfs root filesystem. Files dropped in
#   /usr/local/bin/, /etc/, /etc/systemd/system/, /etc/init.d/ are LOST on
#   every reboot. Previously-tried paths (/etc/persistent/, /config/,
#   MTD mounts) are EdgeOS-style paths that don't exist on UniFi OS.
#
#   The popular `unifi-utilities/unifios-utilities` on-boot installer
#   explicitly refuses the UTR (only supports UDM/UDR/UX/UCG models), so
#   we can't rely on /data/on_boot.d/ either.
#
# SOLUTION:
#   1. /data/ is the persistent partition on UniFi OS — survives reboots
#      AND firmware upgrades. Put the ctrld binary, config, state file,
#      log, pidfile, AND a bootstrap script under /data/ctrld/.
#   2. Try every known UniFi OS persistence hook (best-effort, non-fatal):
#         a. /data/on_boot.d/ctrld.sh           (if another utility created it)
#         b. systemd unit via /data/systemd/    (UniFi OS 4.x persistence)
#         c. systemd unit via /etc/systemd/system/  (won't survive reboot,
#            but starts ctrld on this boot)
#         d. cron @reboot entry                 (if cron present)
#   3. ALWAYS also drop /data/ctrld/start.sh — a one-liner the user can run
#      after every reboot to (re)start ctrld. This is the guaranteed fallback
#      if no auto-start hook worked.
#
# USAGE:
#   sh utr-ctrld-setup.sh                       # default: install latest prefetch
#   sh utr-ctrld-setup.sh --version v1.5.9-prefetch.1
#   sh utr-ctrld-setup.sh --resolver-id <id>    # use Control-D personal resolver
#   sh utr-ctrld-setup.sh --uninstall           # remove everything
#
# FLAGS:
#   --version <tag>       ctrld release to install (default: v1.5.9-prefetch.1)
#   --resolver-id <id>    Control-D resolver ID for personal DoH upstream
#   --listener <ip:port>  ctrld listener (default: 0.0.0.0:53)
#   --metrics <ip:port>   metrics listener (default: 127.0.0.1:9101)
#   --uninstall           Remove everything this script installed
#   --dry-run             Print plan, do nothing
#   -h, --help            Show this help
set -eu

REPO="cddeppe/ctrld"
DEFAULT_VERSION="v1.5.9-prefetch.1"
ARCH="arm"   # UTR is armv7l (Qualcomm IPQ4018)

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

CTRLED_DIR="/data/ctrld"
ON_BOOT_DIR="/data/on_boot.d"
DATA_SYSTEMD_DIR="/data/systemd"   # UniFi OS 4.x persistent systemd override dir
ETC_SYSTEMD_DIR="/etc/systemd/system"
CTRLED_BIN="${CTRLED_DIR}/ctrld"
CTRLED_CFG="${CTRLED_DIR}/ctrld.toml"
CTRLED_STATE="${CTRLED_DIR}/domain-graph.jsonl"
CTRLED_LOGFILE="${CTRLED_DIR}/ctrld.log"
CTRLED_PIDFILE="${CTRLED_DIR}/ctrld.pid"
BOOTSTRAP="${CTRLED_DIR}/ctrld-bootstrap.sh"   # the canonical bootstrap (persistent)
ON_BOOT_HOOK="${ON_BOOT_DIR}/ctrld.sh"          # best-effort hook (only works if /data/on_boot.d/ exists)
SYSTEMD_UNIT_NAME="ctrld.service"
SYSTEMD_UNIT_FILE="${ETC_SYSTEMD_DIR}/${SYSTEMD_UNIT_NAME}"
SYSTEMD_UNIT_PERSISTENT="${DATA_SYSTEMD_DIR}/${SYSTEMD_UNIT_NAME}"

# --- sanity: are we even on a UniFi OS device? ---
if [ ! -d /data ] || [ ! -f /etc/os-release ] 2>/dev/null; then
    if ! grep -qi unifi /etc/os-release 2>/dev/null \
        && ! grep -qi ubios /etc/os-release 2>/dev/null \
        && [ ! -d /data ]; then
        echo "ERROR: /data not found and this doesn't look like a UniFi OS device." >&2
        echo "       Detected OS:" >&2
        cat /etc/os-release 2>/dev/null | head -5 >&2
        echo "       This script targets the Ubiquiti UTR (UniFi OS)." >&2
        exit 1
    fi
fi

# --- uninstall path ---
if [ "$UNINSTALL" = "1" ]; then
    echo "==> Uninstalling ctrld from UTR"
    if [ -f "$CTRLED_PIDFILE" ]; then
        PID="$(cat "$CTRLED_PIDFILE" 2>/dev/null || true)"
        if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
            echo "    stopping ctrld (pid=$PID)"
            kill -TERM "$PID" 2>/dev/null || true
            sleep 2
            kill -0 "$PID" 2>/dev/null && kill -KILL "$PID" 2>/dev/null || true
        fi
        rm -f "$CTRLED_PIDFILE"
    fi
    # Stop systemd unit if active
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop "$SYSTEMD_UNIT_NAME" 2>/dev/null || true
        systemctl disable "$SYSTEMD_UNIT_NAME" 2>/dev/null || true
    fi
    echo "    removing $CTRLED_BIN"
    rm -f "$CTRLED_BIN"
    echo "    removing $CTRLED_CFG"
    rm -f "$CTRLED_CFG"
    echo "    removing bootstrap $BOOTSTRAP"
    rm -f "$BOOTSTRAP"
    echo "    removing on-boot hook $ON_BOOT_HOOK"
    rm -f "$ON_BOOT_HOOK"
    echo "    removing systemd unit"
    rm -f "$SYSTEMD_UNIT_FILE" "$SYSTEMD_UNIT_PERSISTENT"
    # Remove cron entry if present
    if command -v crontab >/dev/null 2>&1; then
        crontab -l 2>/dev/null | grep -v 'ctrld-bootstrap.sh' | crontab - 2>/dev/null || true
    fi
    echo "    keeping state file: $CTRLED_STATE (in case you reinstall)"
    echo "    keeping log file:   $CTRLED_LOGFILE"
    exit 0
fi

# --- main install ---
echo "==> Installing ctrld $VERSION on UTR"
echo "    persistent dir:  $CTRLED_DIR"
echo "    bootstrap:       $BOOTSTRAP"
echo "    listener:        $LISTENER"
echo "    metrics:         $METRICS"
[ -n "$RESOLVER_ID" ] && echo "    resolver-id:     $RESOLVER_ID"

if [ "$DRY_RUN" = "1" ]; then
    echo "    [dry-run] would download ctrld $VERSION ($ARCH) -> $CTRLED_BIN"
    echo "    [dry-run] would write $CTRLED_CFG"
    echo "    [dry-run] would deploy bootstrap to $BOOTSTRAP"
    echo "    [dry-run] would try on-boot hooks (best-effort, non-fatal):"
    echo "              - $ON_BOOT_HOOK"
    echo "              - $SYSTEMD_UNIT_PERSISTENT"
    echo "              - $SYSTEMD_UNIT_FILE (this boot only)"
    echo "              - cron @reboot"
    echo "    [dry-run] would start ctrld and verify"
    exit 0
fi

mkdir -p "$CTRLED_DIR"

# --- 1. download ctrld binary to /data/ctrld/ ---
BIN_NAME="ctrld-${VERSION#v}-linux-${ARCH}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BIN_NAME}"
SHA256_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.txt"

echo
echo "==> Downloading $DOWNLOAD_URL"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
TMP_BIN="$TMPDIR/$BIN_NAME"
TMP_SHA="$TMPDIR/SHA256SUMS.txt"

if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$TMP_BIN" "$DOWNLOAD_URL" || { echo "download failed" >&2; exit 1; }
    curl -fsSL -o "$TMP_SHA"  "$SHA256_URL"  || echo "    (could not fetch SHA256SUMS, skipping verification)"
elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$TMP_BIN" "$DOWNLOAD_URL" || { echo "download failed" >&2; exit 1; }
    wget -q -O "$TMP_SHA"  "$SHA256_URL"  || echo "    (could not fetch SHA256SUMS, skipping verification)"
else
    echo "ERROR: neither curl nor wget found on UTR." >&2
    exit 1
fi

# Verify SHA256 if checksums were fetched.
if [ -s "$TMP_SHA" ]; then
    EXPECTED="$(grep "  ${BIN_NAME}\$" "$TMP_SHA" | awk '{print $1}')"
    if [ -n "$EXPECTED" ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            ACTUAL="$(sha256sum "$TMP_BIN" | awk '{print $1}')"
        elif command -v shasum >/dev/null 2>&1; then
            ACTUAL="$(shasum -a 256 "$TMP_BIN" | awk '{print $1}')"
        else
            ACTUAL=""
        fi
        if [ -n "$ACTUAL" ] && [ "$ACTUAL" != "$EXPECTED" ]; then
            echo "SHA256 mismatch!" >&2
            echo "  expected: $EXPECTED" >&2
            echo "  actual:   $ACTUAL"   >&2
            exit 1
        elif [ -n "$ACTUAL" ]; then
            echo "    sha256 verified: $ACTUAL"
        fi
    fi
fi

chmod +x "$TMP_BIN"
cp "$TMP_BIN" "$CTRLED_BIN"
chmod 0755 "$CTRLED_BIN"
echo "    installed: $CTRLED_BIN"
"$CTRLED_BIN" --version 2>&1 | head -1 | sed 's/^/    /'

# --- 2. write ctrld.toml (preserve existing if present) ---
write_config() {
    # Upstream: Control-D personal resolver (DoH) if --resolver-id was given,
    # else the public Control-D DNS for Families endpoint.
    UPSTREAM_URL="https://freedns.controld.com/p2"
    UPSTREAM_NAME="controld-public"
    if [ -n "$RESOLVER_ID" ]; then
        UPSTREAM_URL="https://dns.controld.com/${RESOLVER_ID}"
        UPSTREAM_NAME="controld-personal"
    fi

    cat > "$CTRLED_CFG" <<EOF
# ctrld.toml — UTR travel-router config (auto-generated by utr-ctrld-setup.sh)
# All persistent artifacts live in /data/ctrld/ which survives reboots
# AND firmware upgrades on UniFi OS.

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
  domain_prefetch_memory_half_life_hours = 72
  domain_prefetch_state_path = "$CTRLED_STATE"
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

if [ -f "$CTRLED_CFG" ]; then
    echo
    echo "==> Existing config at $CTRLED_CFG — preserving (delete it to regenerate)"
    echo "    current contents:"
    sed 's/^/    /' "$CTRLED_CFG"
else
    echo
    echo "==> Writing $CTRLED_CFG"
    write_config
    echo "    upstream: $(grep '^  endpoint' "$CTRLED_CFG" | head -1 | tr -s ' ')"
fi

# --- 3. write the bootstrap script to /data/ctrld/ (persistent) ---
echo
echo "==> Writing bootstrap to $BOOTSTRAP"

cat > "$BOOTSTRAP" <<'BOOTSTRAP_EOF'
#!/bin/sh
# ctrld-bootstrap.sh — start ctrld on the UTR.
# Auto-deployed by utr-ctrld-setup.sh. Safe to call multiple times —
# detects an already-running ctrld and exits cleanly.
#
# This script is the canonical entry point. Other startup hooks
# (/data/on_boot.d/, systemd, cron @reboot) all just exec this script.
set -eu

CTRLED_DIR="/data/ctrld"
CTRLED_BIN="${CTRLED_DIR}/ctrld"
CTRLED_CFG="${CTRLED_DIR}/ctrld.toml"
CTRLED_PIDFILE="${CTRLED_DIR}/ctrld.pid"
CTRLED_LOGFILE="${CTRLED_DIR}/ctrld.log"
CTRLED_STATE="${CTRLED_DIR}/domain-graph.jsonl"

[ -x "$CTRLED_BIN" ] || { echo "[ctrld-bootstrap] binary missing: $CTRLED_BIN" >&2; exit 1; }
[ -f "$CTRLED_CFG" ] || { echo "[ctrld-bootstrap] config missing: $CTRLED_CFG" >&2; exit 1; }

# Already running?
if [ -f "$CTRLED_PIDFILE" ]; then
    PID="$(cat "$CTRLED_PIDFILE" 2>/dev/null || true)"
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        CMDLINE="$(cat /proc/"$PID"/cmdline 2>/dev/null | tr '\0' ' ' || true)"
        case "$CMDLINE" in
            *ctrld*run*) echo "[ctrld-bootstrap] already running (pid=$PID)"; exit 0 ;;
        esac
    fi
    rm -f "$CTRLED_PIDFILE"
fi

# Rotate log if >2MB.
if [ -f "$CTRLED_LOGFILE" ]; then
    LOGSIZE="$(wc -c < "$CTRLED_LOGFILE" 2>/dev/null || echo 0)"
    [ "$LOGSIZE" -gt 2097152 ] && mv "$CTRLED_LOGFILE" "${CTRLED_LOGFILE}.1"
fi

# Wait for default route (upstream DoH needs internet).
TRIES=0
while [ "$TRIES" -lt 30 ]; do
    ip route show default 2>/dev/null | grep -q '^default' && break
    sleep 1; TRIES=$((TRIES + 1))
done

echo "[ctrld-bootstrap] starting ctrld at $(date -Iseconds 2>/dev/null || date)"
nohup setsid "$CTRLED_BIN" run --config "$CTRLED_CFG" >>"$CTRLED_LOGFILE" 2>&1 &
CTRLED_PID=$!
echo "$CTRLED_PID" > "$CTRLED_PIDFILE"
sleep 2
if kill -0 "$CTRLED_PID" 2>/dev/null; then
    echo "[ctrld-bootstrap] ctrld started (pid=$CTRLED_PID)"
else
    echo "[ctrld-bootstrap] WARNING: ctrld exited immediately — check $CTRLED_LOGFILE" >&2
    tail -n 20 "$CTRLED_LOGFILE" >&2 2>/dev/null || true
    rm -f "$CTRLED_PIDFILE"
    exit 1
fi
[ -f "$CTRLED_STATE" ] && echo "[ctrld-bootstrap] state file: $CTRLED_STATE ($(wc -l < "$CTRLED_STATE" 2>/dev/null || echo 0) lines)"
exit 0
BOOTSTRAP_EOF

chmod +x "$BOOTSTRAP"
echo "    bootstrap installed."

# Also write a tiny /data/ctrld/start.sh wrapper for users to call after reboot
# if the auto-start hooks below didn't work. One-liner: `sh /data/ctrld/start.sh`.
START_WRAPPER="${CTRLED_DIR}/start.sh"
cat > "$START_WRAPPER" <<EOF
#!/bin/sh
# One-liner to (re)start ctrld on the UTR.
# Run this after a reboot if ctrld didn't auto-start.
exec sh "$BOOTSTRAP" "\$@"
EOF
chmod +x "$START_WRAPPER"

# --- 4. try every persistence hook we know about (best-effort, non-fatal) ---
echo
echo "==> Trying auto-start hooks (best-effort, non-fatal)"

# 4a. /data/on_boot.d/ — if it exists (some other utility may have created it),
# drop our hook there.
if [ -d "$ON_BOOT_DIR" ]; then
    echo "    found $ON_BOOT_DIR — installing hook"
    cat > "$ON_BOOT_HOOK" <<EOF
#!/bin/sh
exec sh "$BOOTSTRAP"
EOF
    chmod +x "$ON_BOOT_HOOK"
    echo "    hook installed: $ON_BOOT_HOOK"
else
    echo "    $ON_BOOT_DIR does not exist — skipping (no unifi-on-boot detected)"
fi

# 4b. systemd unit — try to install at /data/systemd/ (persistent on
# UniFi OS 4.x if it exists) AND at /etc/systemd/system/ (works for this
# boot even though it's tmpfs).
HAVE_SYSTEMD=0
if command -v systemctl >/dev/null 2>&1; then
    HAVE_SYSTEMD=1
fi

if [ "$HAVE_SYSTEMD" = "1" ]; then
    echo "    systemd detected — writing $SYSTEMD_UNIT_NAME"

    SYSTEMD_UNIT_BODY="[Unit]
Description=ctrld DNS proxy (cddeppe/ctrld fork with domain-graph prefetch)
After=network-online.target
Wants=network-online.target
After=network.target

[Service]
Type=simple
ExecStart=$CTRLED_BIN run --config $CTRLED_CFG
Restart=on-failure
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=10
StandardOutput=append:$CTRLED_LOGFILE
StandardError=append:$CTRLED_LOGFILE
PIDFile=$CTRLED_PIDFILE

[Install]
WantedBy=multi-user.target"

    # Try /data/systemd/ first (persistent on UniFi OS 4.x).
    if [ -d "$DATA_SYSTEMD_DIR" ]; then
        echo "    /data/systemd exists — writing persistent unit there"
        echo "$SYSTEMD_UNIT_BODY" > "$SYSTEMD_UNIT_PERSISTENT"
        # Symlink into /etc/systemd/system/ so systemd loads it this boot.
        ln -sf "$SYSTEMD_UNIT_PERSISTENT" "$SYSTEMD_UNIT_FILE"
    else
        # /data/systemd doesn't exist on this firmware. Write the unit to
        # /etc/systemd/system/ — it WILL be lost on reboot, but starting
        # it now at least gets ctrld running for this session.
        echo "    /data/systemd does NOT exist — writing to $SYSTEMD_UNIT_FILE (tmpfs, lost on reboot)"
        echo "    You will need to run 'sh $START_WRAPPER' after each reboot, OR"
        echo "    tell us which path is persistent on your firmware (see below)."
        mkdir -p "$ETC_SYSTEMD_DIR"
        echo "$SYSTEMD_UNIT_BODY" > "$SYSTEMD_UNIT_FILE"
    fi

    systemctl daemon-reload 2>/dev/null || true
    systemctl enable "$SYSTEMD_UNIT_NAME" 2>/dev/null || true
else
    echo "    no systemd detected — skipping systemd unit"
fi

# 4c. cron @reboot — if cron is present, add a @reboot line.
if command -v crontab >/dev/null 2>&1; then
    echo "    cron detected — adding @reboot entry"
    # Add our entry if not already present.
    if ! crontab -l 2>/dev/null | grep -q 'ctrld-bootstrap.sh'; then
        (crontab -l 2>/dev/null; echo "@reboot sh $BOOTSTRAP # ctrld-bootstrap") | crontab -
        echo "    cron @reboot entry added"
    else
        echo "    cron @reboot entry already present"
    fi
else
    echo "    no cron detected — skipping cron @reboot"
fi

# --- 5. start ctrld now (via the bootstrap, so the path is exercised) ---
echo
echo "==> Starting ctrld now"
sh "$BOOTSTRAP" || {
    echo "ERROR: bootstrap failed. Check $CTRLED_LOGFILE" >&2
    exit 1
}

# --- 6. verify via metrics ---
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

# --- 7. summary ---
echo
echo "==> DONE"
echo "    binary:         $CTRLED_BIN  (persistent)"
echo "    config:         $CTRLED_CFG  (persistent)"
echo "    state file:     $CTRLED_STATE  (persistent — habits accumulate across reboots)"
echo "    log:            $CTRLED_LOGFILE"
echo "    bootstrap:      $BOOTSTRAP  (persistent)"
echo "    start wrapper:  $START_WRAPPER  (persistent — call after reboot if needed)"
echo
echo "AUTO-START HOOKS TRIED (best-effort):"
[ -f "$ON_BOOT_HOOK" ] && echo "  [x] /data/on_boot.d/ctrld.sh  (if unifi-on-boot was installed elsewhere)"
[ -f "$SYSTEMD_UNIT_FILE" ] && echo "  [x] systemd unit (this boot only — /etc/systemd/system/ is tmpfs)"
[ -f "$SYSTEMD_UNIT_PERSISTENT" ] && echo "  [x] systemd unit (/data/systemd/ — persistent)"
command -v crontab >/dev/null 2>&1 && echo "  [x] cron @reboot entry"
echo
echo "GUARANTEED FALLBACK (always works):"
echo "  After every reboot, if ctrld is not running, just run:"
echo "    sh $START_WRAPPER"
echo
echo "VERIFY AFTER REBOOT:"
echo "  1. cat $CTRLED_PIDFILE && kill -0 \$(cat $CTRLED_PIDFILE) && echo OK"
echo "  2. curl -s http://${METRICS_HOST}:${METRICS_PORT}/metrics | grep ctrld_build_info"
echo "  3. wc -l $CTRLED_STATE   # after ~5 min of DNS traffic"
echo
echo "To uninstall later:"
echo "  sh $0 --uninstall"
