#!/bin/sh
# install.sh — installer for ctrld v1.5.8-prefetch (cddeppe/ctrld fork).
#
# Detects the existing ctrld installation (running process or systemd unit),
# backs up the old binary, replaces it in place, and restarts the service.
# If no existing ctrld is found, falls back to /usr/local/bin/ctrld.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/install.sh | sh -
#   curl -fsSL .../install.sh | sh - -- --arch arm64
#   curl -fsSL .../install.sh | sh - -- --prefix /usr/local/bin   # force install dir
#   curl -fsSL .../install.sh | sh - -- --no-restart               # don't restart service
#   curl -fsSL .../install.sh | sh - -- --dry-run                 # print plan, do nothing
#
# Flags:
#   --arch <amd64|arm64>    Force architecture (default: auto-detect via uname -m)
#   --os <linux|darwin>     Force OS (default: auto-detect via uname -s)
#   --prefix <dir>           Force install dir (default: detect existing ctrld or /usr/local/bin)
#   --version <tag>          Release tag to install (default: v1.5.8-prefetch)
#   --no-restart             Don't stop/replace/restart — just install the binary
#   --no-backup              Don't back up the old binary before replacing
#   --dry-run                Print what would happen, don't actually do anything
#   -h, --help               Show this help
set -eu

REPO="cddeppe/ctrld"
DEFAULT_VERSION="v1.5.9-prefetch.1"

# --- arg parsing ---
ARCH=""
OS=""
PREFIX=""
VERSION="$DEFAULT_VERSION"
DRY_RUN=0
NO_RESTART=0
NO_BACKUP=0

while [ $# -gt 0 ]; do
    case "$1" in
        --arch)       ARCH="$2"; shift 2 ;;
        --os)         OS="$2"; shift 2 ;;
        --prefix)     PREFIX="$2"; shift 2 ;;
        --version)    VERSION="$2"; shift 2 ;;
        --no-restart) NO_RESTART=1; shift ;;
        --no-backup)  NO_BACKUP=1; shift ;;
        --dry-run)    DRY_RUN=1; shift ;;
        -h|--help)
            sed -n '3,22p' "$0"
            exit 0
            ;;
        --) shift; continue ;;
        *) echo "Unknown flag: $1" >&2; exit 1 ;;
    esac
done

# --- detect OS / arch if not forced ---
if [ -z "$OS" ]; then
    case "$(uname -s)" in
        Linux*)  OS="linux" ;;
        Darwin*) OS="darwin" ;;
        *) echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
    esac
fi
if [ -z "$ARCH" ]; then
    case "$(uname -m)" in
        x86_64|amd64)  ARCH="amd64" ;;
        arm64|aarch64) ARCH="arm64" ;;
        *) echo "Unsupported arch: $(uname -m)" >&2; exit 1 ;;
    esac
fi

# Only linux/darwin + amd64/arm64 are built for this release.
if [ "$OS" != "linux" ] && [ "$OS" != "darwin" ]; then
    echo "No prebuilt binary for OS=$OS. Build from source:" >&2
    echo "  git clone https://github.com/$REPO && cd ctrld && git checkout $VERSION && go build ./cmd/ctrld" >&2
    exit 1
fi
if [ "$ARCH" != "amd64" ] && [ "$ARCH" != "arm64" ]; then
    echo "No prebuilt binary for ARCH=$ARCH. Build from source:" >&2
    echo "  git clone https://github.com/$REPO && cd ctrld && git checkout $VERSION && go build ./cmd/ctrld" >&2
    exit 1
fi

# --- detect existing ctrld installation (running process > systemd unit > none) ---
# 1. Look at the running ctrld process's binary path via /proc/<pid>/exe (Linux only).
# 2. Fall back to parsing the systemd unit's ExecStart= line.
# 3. Fall back to nothing — install to /usr/local/bin/ctrld.
EXISTING_CTRLD=""
EXISTING_CTRLD_VIA=""   # "running-process" | "systemd-unit" | ""
SERVICE_MGR=""          # "systemd" | "sysv-init" | ""

# Try running process detection (Linux /proc).
CTRLD_PID="$(pgrep -f '/ctrld run' 2>/dev/null | head -1 || true)"
if [ -z "$CTRLD_PID" ]; then
    # broader match — any ctrld process running with "run" arg
    CTRLD_PID="$(pgrep -x ctrld 2>/dev/null | head -1 || true)"
fi
if [ -n "$CTRLD_PID" ] && [ -r "/proc/$CTRLD_PID/exe" ]; then
    EXISTING_CTRLD="$(readlink -f "/proc/$CTRLD_PID/exe" 2>/dev/null || true)"
    if [ -n "$EXISTING_CTRLD" ] && [ -x "$EXISTING_CTRLD" ]; then
        EXISTING_CTRLD_VIA="running-process (pid=$CTRLD_PID)"
    else
        EXISTING_CTRLD=""
    fi
fi

# Try systemd unit detection if no running process found.
if [ -z "$EXISTING_CTRLD" ] && command -v systemctl >/dev/null 2>&1; then
    if systemctl list-unit-files 2>/dev/null | grep -q '^ctrld\.service'; then
        SERVICE_MGR="systemd"
        # Parse ExecStart= line — the first token after ExecStart= is the binary path.
        EXEC_LINE="$(systemctl cat ctrld 2>/dev/null | grep -E '^ExecStart=' | head -1 || true)"
        if [ -n "$EXEC_LINE" ]; then
            # Strip the "ExecStart=" prefix and the surrounding quotes; take first token.
            EXEC_PATH="$(echo "$EXEC_LINE" | sed -E 's/^ExecStart=//; s/^"//; s/"$//; s/\s.*//' | head -1)"
            if [ -n "$EXEC_PATH" ] && [ -x "$EXEC_PATH" ]; then
                EXISTING_CTRLD="$EXEC_PATH"
                EXISTING_CTRLD_VIA="systemd unit (ctrld.service)"
            fi
        fi
    fi
fi

# Try SysV init script if no systemd unit found.
if [ -z "$EXISTING_CTRLD" ] && [ -x "/etc/init.d/ctrld" ]; then
    SERVICE_MGR="sysv-init"
    # Parse the init script for the binary path — common patterns.
    for pattern in 's/.*BIN=\([^"]*\).*/\1/p' 's/.*daemon\s\([^"]*\).*/\1/p'; do
        EXEC_PATH="$(sed -n "$pattern" /etc/init.d/ctrld 2>/dev/null | head -1 || true)"
        if [ -n "$EXEC_PATH" ] && [ -x "$EXEC_PATH" ]; then
            EXISTING_CTRLD="$EXEC_PATH"
            EXISTING_CTRLD_VIA="sysv-init script (/etc/init.d/ctrld)"
            break
        fi
    done
fi

# If a running process was found, also note the service manager (so we can restart it).
if [ -n "$CTRLD_PID" ] && [ -z "$SERVICE_MGR" ]; then
    if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q '^ctrld\.service'; then
        SERVICE_MGR="systemd"
    elif [ -x "/etc/init.d/ctrld" ]; then
        SERVICE_MGR="sysv-init"
    fi
fi

# --- resolve install target ---
USER_FORCED_PREFIX=1
if [ -z "$PREFIX" ]; then
    USER_FORCED_PREFIX=0
    if [ -n "$EXISTING_CTRLD" ]; then
        # Install to the same path as the existing ctrld binary.
        PREFIX="$(dirname "$EXISTING_CTRLD")"
    elif [ -w "/usr/local/bin" ] || command -v sudo >/dev/null 2>&1; then
        PREFIX="/usr/local/bin"
    else
        PREFIX="$HOME/.local/bin"
    fi
fi
TARGET_BIN="$PREFIX/ctrld"

# On Linux, if the target equals EXISTING_CTRLD, we'll need to stop the service
# before we can overwrite the file (can't mv over a running binary on Linux).
WILL_REPLACE_RUNNING=0
if [ -n "$EXISTING_CTRLD" ] && [ "$EXISTING_CTRLD" = "$TARGET_BIN" ] && [ "$NO_RESTART" = "0" ]; then
    WILL_REPLACE_RUNNING=1
fi

BIN_NAME="ctrld-${VERSION#v}-${OS}-${ARCH}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BIN_NAME}"
SHA256_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.txt"

# --- print plan ---
echo "==> Installing ctrld $VERSION ($OS/$ARCH)"
echo "    target binary:       $TARGET_BIN"
if [ -n "$EXISTING_CTRLD_VIA" ]; then
    echo "    detected existing:  $EXISTING_CTRLD"
    echo "    detected via:       $EXISTING_CTRLD_VIA"
else
    echo "    detected existing:  (none — fresh install)"
fi
if [ "$WILL_REPLACE_RUNNING" = "1" ]; then
    echo "    will stop service:  yes (to replace running binary)"
    echo "    service manager:    ${SERVICE_MGR:-unknown}"
    if [ "$NO_BACKUP" = "0" ]; then
        echo "    will back up to:    ${EXISTING_CTRLD}.bak"
    fi
fi

if [ "$DRY_RUN" = "1" ]; then
    echo "    [dry-run] would download: $DOWNLOAD_URL"
    echo "    [dry-run] would verify with: $SHA256_URL"
    [ "$WILL_REPLACE_RUNNING" = "1" ] && echo "    [dry-run] would stop service, replace binary, restart service"
    exit 0
fi

# --- download binary to temp file (BEFORE stopping service, so DNS still works) ---
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
TMP_BIN="$TMPDIR/$BIN_NAME"
TMP_SHA="$TMPDIR/SHA256SUMS.txt"

echo "    downloading $DOWNLOAD_URL"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$TMP_BIN" "$DOWNLOAD_URL" || { echo "download failed" >&2; exit 1; }
    curl -fsSL -o "$TMP_SHA"  "$SHA256_URL"  || echo "    (could not fetch SHA256SUMS, skipping verification)"
elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$TMP_BIN" "$DOWNLOAD_URL" || { echo "download failed" >&2; exit 1; }
    wget -q -O "$TMP_SHA"  "$SHA256_URL"  || echo "    (could not fetch SHA256SUMS, skipping verification)"
else
    echo "neither curl nor wget found" >&2; exit 1
fi

# --- verify SHA256 if checksums were fetched ---
if [ -s "$TMP_SHA" ]; then
    EXPECTED="$(grep "  ${BIN_NAME}\$" "$TMP_SHA" | awk '{print $1}')"
    if [ -n "$EXPECTED" ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            ACTUAL="$(sha256sum "$TMP_BIN" | awk '{print $1}')"
        elif command -v shasum >/dev/null 2>&1; then
            ACTUAL="$(shasum -a 256 "$TMP_BIN" | awk '{print $1}')"
        else
            ACTUAL=""
            echo "    (no sha256 tool found, skipping verification)"
        fi
        if [ -n "$ACTUAL" ] && [ "$ACTUAL" != "$EXPECTED" ]; then
            echo "SHA256 mismatch!" >&2
            echo "  expected: $EXPECTED" >&2
            echo "  actual:   $ACTUAL" >&2
            exit 1
        elif [ -n "$ACTUAL" ]; then
            echo "    sha256 verified: $ACTUAL"
        fi
    fi
fi

chmod +x "$TMP_BIN"

# --- determine if we need sudo ---
NEEDS_SUDO=0
SUDO=""
if [ ! -w "$PREFIX" ] && [ "$PREFIX" != "$HOME/.local/bin" ]; then
    NEEDS_SUDO=1
    if ! command -v sudo >/dev/null 2>&1; then
        echo "install dir $PREFIX not writable and sudo not available" >&2
        exit 1
    fi
    SUDO="sudo"
fi

# --- if replacing a running binary, stop the service first ---
if [ "$WILL_REPLACE_RUNNING" = "1" ]; then
    echo "    stopping ctrld service (to allow binary replacement)..."
    if [ "$SERVICE_MGR" = "systemd" ]; then
        $SUDO systemctl stop ctrld 2>&1 || true
    elif [ "$SERVICE_MGR" = "sysv-init" ]; then
        $SUDO /etc/init.d/ctrld stop 2>&1 || true
    else
        # Try both, ignore failures
        systemctl stop ctrld 2>/dev/null || true
        /etc/init.d/ctrld stop 2>/dev/null || true
        # If those didn't work and we have a PID, kill it directly
        if [ -n "$CTRLD_PID" ] && kill -0 "$CTRLD_PID" 2>/dev/null; then
            echo "    service stop didn't kill pid $CTRLD_PID, sending SIGTERM directly"
            kill -TERM "$CTRLD_PID" 2>/dev/null || true
            sleep 2
            kill -0 "$CTRLD_PID" 2>/dev/null && kill -KILL "$CTRLD_PID" 2>/dev/null || true
        fi
    fi
    sleep 2
fi

# --- back up existing binary ---
if [ "$WILL_REPLACE_RUNNING" = "1" ] && [ "$NO_BACKUP" = "0" ] && [ -n "$EXISTING_CTRLD" ]; then
    BACKUP="${EXISTING_CTRLD}.bak"
    echo "    backing up $EXISTING_CTRLD -> $BACKUP"
    if [ "$NEEDS_SUDO" = "1" ]; then
        sudo cp -p "$EXISTING_CTRLD" "$BACKUP"
    else
        cp -p "$EXISTING_CTRLD" "$BACKUP"
    fi
fi

# --- install (replace the binary) ---
if [ "$NEEDS_SUDO" = "1" ]; then
    sudo install -m 0755 "$TMP_BIN" "$TARGET_BIN"
else
    install -m 0755 "$TMP_BIN" "$TARGET_BIN"
fi

# --- restart the service if we stopped it ---
if [ "$WILL_REPLACE_RUNNING" = "1" ]; then
    echo "    starting ctrld service..."
    if [ "$SERVICE_MGR" = "systemd" ]; then
        $SUDO systemctl start ctrld 2>&1 || true
    elif [ "$SERVICE_MGR" = "sysv-init" ]; then
        $SUDO /etc/init.d/ctrld start 2>&1 || true
    else
        systemctl start ctrld 2>/dev/null || /etc/init.d/ctrld start 2>/dev/null || true
    fi
    sleep 3   # give the service a moment to come up
fi

# --- verify install ---
echo "==> Installed:"
"$TARGET_BIN" --version 2>&1 | head -1 || true

# --- if we restarted, also confirm the running process picked up the new version ---
if [ "$WILL_REPLACE_RUNNING" = "1" ]; then
    echo
    echo "==> Service status:"
    if [ "$SERVICE_MGR" = "systemd" ]; then
        systemctl is-active ctrld 2>&1 || true
    elif [ "$SERVICE_MGR" = "sysv-init" ]; then
        /etc/init.d/ctrld status 2>&1 | head -3 || true
    fi

    # If metrics endpoint is configured, try to verify the running version matches
    METRICS_PORT=""
    if [ -f /etc/controld/ctrld.toml ]; then
        METRICS_PORT="$(grep -E '^\s*metrics_listener\s*=' /etc/controld/ctrld.toml 2>/dev/null \
                       | sed -E 's/.*=.*[\"'\'']([^:]+:[0-9]+)[\"'\''].*/\1/' | head -1 || true)"
    fi
    if [ -n "$METRICS_PORT" ]; then
        echo
        echo "==> Verifying running process via metrics endpoint ($METRICS_PORT):"
        if command -v curl >/dev/null 2>&1; then
            curl -fsS "http://${METRICS_PORT#127.0.0.1/}/metrics" 2>/dev/null | grep '^ctrld_build_info' | head -1 \
                | sed -E 's/^/    /' \
                || echo "    (metrics endpoint not responding yet — give ctrld a few more seconds to start)"
        fi
    fi
fi

echo
echo "Docs: https://github.com/$REPO/releases/tag/$VERSION"
if [ "$WILL_REPLACE_RUNNING" = "0" ]; then
    echo
    echo "Note: A running ctrld service was not detected, so this was a fresh install."
    echo "      To start ctrld as a service:"
    echo "        ctrld start --cd <your-resolver-id>"
fi
