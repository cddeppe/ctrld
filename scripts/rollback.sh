#!/bin/sh
# rollback.sh — restores the original ctrld binary from the .bak file
# created by install.sh, OR removes the prefetch binary if no backup exists.
#
# Mirrors install.sh's detection logic so the same script works on every
# device type (GL.iNet routers with Windscribe's binary at /data/controld/,
# vanilla Linux installs at /usr/local/bin/, etc.).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/rollback.sh | sh -
#   curl -fsSL .../rollback.sh | sh - -- --dry-run
#
# Flags:
#   --dry-run     Print what would happen, don't actually change anything
#   --no-restart  Don't restart the service after rollback (just swap binary)
#   -h, --help    Show this help
set -eu

# --- arg parsing ---
DRY_RUN=0
NO_RESTART=0

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run)    DRY_RUN=1; shift ;;
        --no-restart) NO_RESTART=1; shift ;;
        -h|--help)
            sed -n '3,15p' "$0"
            exit 0
            ;;
        --) shift; continue ;;
        *) echo "Unknown flag: $1" >&2; exit 1 ;;
    esac
done

# --- detect existing ctrld binary (same logic as install.sh) ---
EXISTING_CTRLD=""
EXISTING_CTRLD_VIA=""
SERVICE_MGR=""
CTRLD_PID=""

# Try running process detection (Linux /proc).
CTRLD_PID="$(pgrep -f '/ctrld run' 2>/dev/null | head -1 || true)"
if [ -z "$CTRLD_PID" ]; then
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
        EXEC_LINE="$(systemctl cat ctrld 2>/dev/null | grep -E '^ExecStart=' | head -1 || true)"
        if [ -n "$EXEC_LINE" ]; then
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
    for pattern in 's/.*BIN=\([^"]*\).*/\1/p' 's/.*daemon\s\([^"]*\).*/\1/p'; do
        EXEC_PATH="$(sed -n "$pattern" /etc/init.d/ctrld 2>/dev/null | head -1 || true)"
        if [ -n "$EXEC_PATH" ] && [ -x "$EXEC_PATH" ]; then
            EXISTING_CTRLD="$EXEC_PATH"
            EXISTING_CTRLD_VIA="sysv-init script (/etc/init.d/ctrld)"
            break
        fi
    done
fi

# If running process was found, also note the service manager.
if [ -n "$CTRLD_PID" ] && [ -z "$SERVICE_MGR" ]; then
    if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q '^ctrld\.service'; then
        SERVICE_MGR="systemd"
    elif [ -x "/etc/init.d/ctrld" ]; then
        SERVICE_MGR="sysv-init"
    fi
fi

# --- find the .bak file ---
BACKUP_FILE=""
if [ -n "$EXISTING_CTRLD" ]; then
    if [ -f "${EXISTING_CTRLD}.bak" ]; then
        BACKUP_FILE="${EXISTING_CTRLD}.bak"
    elif [ -f "${EXISTING_CTRLD}.windscribe.bak" ]; then
        # Older backup naming convention used in early install.sh versions.
        BACKUP_FILE="${EXISTING_CTRLD}.windscribe.bak"
    fi
fi

# --- print plan ---
echo "==> Rolling back ctrld prefetch binary"
if [ -n "$EXISTING_CTRLD_VIA" ]; then
    echo "    detected binary:     $EXISTING_CTRLD"
    echo "    detected via:       $EXISTING_CTRLD_VIA"
else
    echo "    detected binary:    (none — no ctrld process or service found)"
fi
if [ -n "$BACKUP_FILE" ]; then
    echo "    backup available:   $BACKUP_FILE"
    echo "    will:               stop service -> restore backup -> restart service"
elif [ -n "$EXISTING_CTRLD" ]; then
    echo "    backup available:   (none — no .bak file found)"
    echo "    will:               stop service -> remove prefetch binary"
    echo "                        (you'll need to reinstall ctrld via your original method"
    echo "                         — e.g. Control-D's installer or your distro's package)"
    if [ "$NO_RESTART" = "0" ]; then
        echo "                        service will NOT be restarted after rollback"
    fi
else
    echo "    will:               nothing to roll back (no ctrld detected)"
fi

if [ "$DRY_RUN" = "1" ]; then
    exit 0
fi

# --- perform rollback ---
if [ -z "$EXISTING_CTRLD" ]; then
    echo "Nothing to roll back."
    exit 0
fi

# Determine if we need sudo
NEEDS_SUDO=0
SUDO=""
if [ ! -w "$(dirname "$EXISTING_CTRLD")" ]; then
    NEEDS_SUDO=1
    if command -v sudo >/dev/null 2>&1; then
        SUDO="sudo"
    else
        echo "Cannot write to $(dirname "$EXISTING_CTRLD") and sudo not available" >&2
        exit 1
    fi
fi

# Stop the service so we can replace the running binary.
if [ "$NO_RESTART" = "0" ]; then
    echo "    stopping ctrld service..."
    if [ "$SERVICE_MGR" = "systemd" ]; then
        $SUDO systemctl stop ctrld 2>&1 || true
    elif [ "$SERVICE_MGR" = "sysv-init" ]; then
        $SUDO /etc/init.d/ctrld stop 2>&1 || true
    else
        systemctl stop ctrld 2>/dev/null || true
        /etc/init.d/ctrld stop 2>/dev/null || true
        if [ -n "$CTRLD_PID" ] && kill -0 "$CTRLD_PID" 2>/dev/null; then
            echo "    service stop didn't kill pid $CTRLD_PID, sending SIGTERM"
            kill -TERM "$CTRLD_PID" 2>/dev/null || true
            sleep 2
            kill -0 "$CTRLD_PID" 2>/dev/null && kill -KILL "$CTRLD_PID" 2>/dev/null || true
        fi
    fi
    sleep 2
fi

# Restore or remove.
if [ -n "$BACKUP_FILE" ]; then
    echo "    restoring $BACKUP_FILE -> $EXISTING_CTRLD"
    $SUDO cp -p "$BACKUP_FILE" "$EXISTING_CTRLD"
    $SUDO chmod +x "$EXISTING_CTRLD"

    # Sanity-check the restored binary.
    "$EXISTING_CTRLD" --version 2>&1 | head -1 || true
else
    echo "    removing prefetch binary at $EXISTING_CTRLD (no backup to restore)"
    $SUDO rm -f "$EXISTING_CTRLD"
    echo "    removed. ctrld is no longer installed at this path."
fi

# Restart the service if applicable.
if [ "$NO_RESTART" = "0" ] && [ -n "$BACKUP_FILE" ]; then
    echo "    starting ctrld service..."
    if [ "$SERVICE_MGR" = "systemd" ]; then
        $SUDO systemctl start ctrld 2>&1 || true
    elif [ "$SERVICE_MGR" = "sysv-init" ]; then
        $SUDO /etc/init.d/ctrld start 2>&1 || true
    else
        systemctl start ctrld 2>/dev/null || /etc/init.d/ctrld start 2>/dev/null || true
    fi
    sleep 3
    echo
    echo "==> Service status:"
    if [ "$SERVICE_MGR" = "systemd" ]; then
        systemctl is-active ctrld 2>&1 || true
    elif [ "$SERVICE_MGR" = "sysv-init" ]; then
        /etc/init.d/ctrld status 2>&1 | head -3 || true
    fi

    # Verify the running process is now the restored binary.
    if [ -n "$BACKUP_FILE" ]; then
        NEW_PID="$(pgrep -f '/ctrld run' 2>/dev/null | head -1 || true)"
        if [ -z "$NEW_PID" ]; then
            NEW_PID="$(pgrep -x ctrld 2>/dev/null | head -1 || true)"
        fi
        if [ -n "$NEW_PID" ] && [ -r "/proc/$NEW_PID/exe" ]; then
            NEW_BIN="$(readlink -f "/proc/$NEW_PID/exe" 2>/dev/null || true)"
            echo
            echo "==> Running binary:"
            echo "    $NEW_BIN"
            "$NEW_BIN" --version 2>&1 | head -1 || true
        fi
    fi
fi

echo
echo "Rollback complete."
if [ -z "$BACKUP_FILE" ] && [ -n "$EXISTING_CTRLD" ]; then
    echo
    echo "Note: ctrld was removed but no backup was available. To reinstall"
    echo "the original ctrld, use whichever method you originally used:"
    echo "  - Control-D installer: sh -c \"\$(curl -sL https://api.controld.com/dl)\""
    echo "  - Your distro's package manager (apt/dnf/etc.)"
    echo "  - GL.iNet/Windscribe device: re-flash the firmware or use their"
    echo "    restore tool"
fi
