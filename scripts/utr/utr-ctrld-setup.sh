#!/bin/sh
# utr-ctrld-setup.sh — one-time setup script for the prefetch-enabled ctrld
# on the Ubiquiti UniFi Travel Router (UTR).
#
# PROBLEM:
#   The UTR runs UniFi OS. The root filesystem is tmpfs (rebuilt on every boot
#   from a read-only image), so installing a binary to /usr/local/bin/ctrld
#   DOES NOT persist. Previously-tried paths (/etc/persistent/, /config/,
#   MTD mounts) are EdgeOS-style paths that don't exist on UniFi OS.
#
# SOLUTION:
#   1. /data/ is the persistent partition on UniFi OS — survives reboots AND
#      firmware upgrades. Put the ctrld binary, config, and state file there.
#   2. The unredacted/unifi-on-boot utility installs a systemd unit that
#      executes /data/on_boot.d/*.sh on every boot AND restores itself after
#      firmware upgrades. Drop a bootstrap script there to start ctrld at boot.
#
# RUN FROM: a workstation, with the UTR reachable via SSH.
#   scp this script to the UTR, then ssh in and run it as root.
#   OR run it remotely:
#     ssh root@<utr-ip> 'sh -' < utr-ctrld-setup.sh
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
CTRLED_BIN="${CTRLED_DIR}/ctrld"
CTRLED_CFG="${CTRLED_DIR}/ctrld.toml"
CTRLED_STATE="${CTRLED_DIR}/domain-graph.jsonl"
CTRLED_LOGFILE="${CTRLED_DIR}/ctrld.log"
CTRLED_PIDFILE="${CTRLED_DIR}/ctrld.pid"
BOOTSTRAP="${ON_BOOT_DIR}/ctrld.sh"

# --- sanity: are we even on a UniFi OS device? ---
if [ ! -d /data ] || [ ! -f /etc/os-release ] 2>/dev/null; then
    # /data should always exist on UniFi OS — if not, this is the wrong device.
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
    echo "    removing $CTRLED_BIN"
    rm -f "$CTRLED_BIN"
    echo "    removing $CTRLED_CFG"
    rm -f "$CTRLED_CFG"
    echo "    removing bootstrap $BOOTSTRAP"
    rm -f "$BOOTSTRAP"
    echo "    keeping state file: $CTRLED_STATE (in case you reinstall)"
    echo "    keeping log file:   $CTRLED_LOGFILE"
    echo
    echo "NOTE: unredacted/unifi-on-boot itself was NOT removed."
    echo "      To remove it (and lose all /data/on_boot.d/ scripts):"
    echo "        sh /data/unifi-on-boot/uninstall.sh"
    exit 0
fi

# --- main install ---
echo "==> Installing ctrld $VERSION on UTR"
echo "    persistent dir:  $CTRLED_DIR"
echo "    bootstrap:      $BOOTSTRAP"
echo "    listener:       $LISTENER"
echo "    metrics:        $METRICS"
[ -n "$RESOLVER_ID" ] && echo "    resolver-id:    $RESOLVER_ID"

if [ "$DRY_RUN" = "1" ]; then
    echo "    [dry-run] would install unredacted/unifi-on-boot"
    echo "    [dry-run] would download ctrld $VERSION ($ARCH) -> $CTRLED_BIN"
    echo "    [dry-run] would write $CTRLED_CFG"
    echo "    [dry-run] would deploy bootstrap to $BOOTSTRAP"
    echo "    [dry-run] would start ctrld and verify"
    exit 0
fi

# --- 1. install unredacted/unifi-on-boot (gives us /data/on_boot.d/) ---
if [ ! -d "$ON_BOOT_DIR" ] || [ ! -f /data/unifi-on-boot/installed ]; then
    echo
    echo "==> Installing unredacted/unifi-on-boot (provides /data/on_boot.d/ persistence)"
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
        echo "ERROR: neither curl nor wget found on UTR — cannot install unifi-on-boot." >&2
        exit 1
    fi
    # The unredacted installer is mirrored at the unifi-utilities org.
    INSTALL_URL="https://raw.githubusercontent.com/unredacted/unifi-on-boot/main/install.sh"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$INSTALL_URL" | sh -
    else
        wget -qO- "$INSTALL_URL" | sh -
    fi
    # Verify it actually installed.
    if [ ! -d "$ON_BOOT_DIR" ]; then
        echo "ERROR: unifi-on-boot install did not create $ON_BOOT_DIR" >&2
        echo "       Try running the install.sh manually and check output." >&2
        exit 1
    fi
    echo "    unifi-on-boot installed."
else
    echo "    unifi-on-boot already installed (skipping)."
fi

mkdir -p "$CTRLED_DIR" "$ON_BOOT_DIR"

# --- 2. download ctrld binary to /data/ctrld/ ---
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

# --- 3. write ctrld.toml (preserve existing if present) ---
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

# --- 4. deploy the bootstrap script ---
echo
echo "==> Deploying bootstrap to $BOOTSTRAP"

# The bootstrap script is shipped alongside this installer in the same
# repo/directory. We try to find it; if not present, we write an inline copy.
BOOTSTRAP_SRC="$(dirname "$0")/ctrld-bootstrap.sh"
if [ -f "$BOOTSTRAP_SRC" ]; then
    cp "$BOOTSTRAP_SRC" "$BOOTSTRAP"
else
    # Inline copy — keeps the installer self-contained when piped over SSH.
    # Keep this in sync with scripts/utr/ctrld-bootstrap.sh.
    cat > "$BOOTSTRAP" <<'BOOTSTRAP_EOF'
#!/bin/sh
# ctrld-bootstrap.sh — on_boot.d script for UniFi Travel Router (UTR).
# Auto-deployed by utr-ctrld-setup.sh. See that script for full docs.
set -eu

CTRLED_DIR="/data/ctrld"
CTRLED_BIN="${CTRLED_DIR}/ctrld"
CTRLED_CFG="${CTRLED_DIR}/ctrld.toml"
CTRLED_PIDFILE="${CTRLED_DIR}/ctrld.pid"
CTRLED_LOGFILE="${CTRLED_DIR}/ctrld.log"
CTRLED_STATE="${CTRLED_DIR}/domain-graph.jsonl"

[ -x "$CTRLED_BIN" ] || { echo "[ctrld-bootstrap] binary missing: $CTRLED_BIN" >&2; exit 1; }
[ -f "$CTRLED_CFG" ] || { echo "[ctrld-bootstrap] config missing: $CTRLED_CFG" >&2; exit 1; }

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

if [ -f "$CTRLED_LOGFILE" ]; then
    LOGSIZE="$(wc -c < "$CTRLED_LOGFILE" 2>/dev/null || echo 0)"
    [ "$LOGSIZE" -gt 2097152 ] && mv "$CTRLED_LOGFILE" "${CTRLED_LOGFILE}.1"
fi

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
fi

chmod +x "$BOOTSTRAP"
echo "    bootstrap installed."

# --- 5. run the bootstrap now to start ctrld immediately ---
echo
echo "==> Starting ctrld now (also auto-starts on every future boot)"
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
echo "    binary:        $CTRLED_BIN  (persistent)"
echo "    config:        $CTRLED_CFG  (persistent)"
echo "    state file:    $CTRLED_STATE  (persistent, grows over time)"
echo "    log:           $CTRLED_LOGFILE"
echo "    bootstrap:     $BOOTSTRAP  (runs on every boot via unifi-on-boot)"
echo
echo "Next steps:"
echo "  1. Reboot the UTR to confirm ctrld auto-starts:"
echo "       reboot"
echo "  2. After reboot, verify ctrld is running:"
echo "       cat $CTRLED_PIDFILE && kill -0 \$(cat $CTRLED_PIDFILE) && echo OK"
echo "  3. Check prefetch is learning (after ~5 min of DNS traffic):"
echo "       wc -l $CTRLED_STATE"
echo "  4. Watch metrics:"
echo "       curl -s http://${METRICS_HOST}:${METRICS_PORT}/metrics | grep ctrld_"
echo
echo "To uninstall later:"
echo "  sh $0 --uninstall"
