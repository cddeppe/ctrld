#!/bin/sh
# install.sh — installer for ctrld v1.5.8-prefetch (cddeppe/ctrld fork).
#
# Mirrors Control-D's official installer UX but pulls binaries from the
# cddeppe/ctrld GitHub release instead of api.controld.com.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/install.sh | sh -
#   curl -fsSL https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/install.sh | sh - -- --arch arm64
#   curl -fsSL https://raw.githubusercontent.com/cddeppe/ctrld/main/scripts/install.sh | sh - -- --prefix ~/bin
#
# Flags:
#   --arch <amd64|arm64>   Force architecture (default: auto-detect via uname -m)
#   --os <linux|darwin>    Force OS (default: auto-detect via uname -s)
#   --prefix <dir>         Install directory (default: /usr/local/bin if writable or sudo available, else $HOME/.local/bin)
#   --version <tag>        Release tag to install (default: v1.5.8-prefetch)
#   --dry-run              Print what would happen, don't actually download or install
#   -h, --help             Show this help
set -eu

REPO="cddeppe/ctrld"
DEFAULT_VERSION="v1.5.8-prefetch"

# --- arg parsing ---
ARCH=""
OS=""
PREFIX=""
VERSION="$DEFAULT_VERSION"
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --arch)    ARCH="$2"; shift 2 ;;
        --os)      OS="$2"; shift 2 ;;
        --prefix)  PREFIX="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h|--help)
            sed -n '3,20p' "$0"
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

# --- resolve install prefix ---
if [ -z "$PREFIX" ]; then
    if [ -w "/usr/local/bin" ]; then
        PREFIX="/usr/local/bin"
    elif command -v sudo >/dev/null 2>&1; then
        PREFIX="/usr/local/bin"
    else
        PREFIX="$HOME/.local/bin"
    fi
fi
mkdir -p "$PREFIX" 2>/dev/null || {
    echo "Cannot create install dir: $PREFIX" >&2
    exit 1
}

BIN_NAME="ctrld-${VERSION#v}-${OS}-${ARCH}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${BIN_NAME}"
SHA256_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.txt"

echo "==> Installing ctrld $VERSION ($OS/$ARCH) to $PREFIX/ctrld"
if [ "$DRY_RUN" = "1" ]; then
    echo "    [dry-run] would download: $DOWNLOAD_URL"
    echo "    [dry-run] would verify with: $SHA256_URL"
    echo "    [dry-run] would install to: $PREFIX/ctrld"
    exit 0
fi

# --- download binary to temp file ---
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

# --- install ---
NEEDS_SUDO=0
if [ ! -w "$PREFIX" ]; then
    NEEDS_SUDO=1
    if ! command -v sudo >/dev/null 2>&1; then
        echo "install dir $PREFIX not writable and sudo not available" >&2
        exit 1
    fi
fi
if [ "$NEEDS_SUDO" = "1" ]; then
    sudo mv "$TMP_BIN" "$PREFIX/ctrld"
    sudo chmod +x "$PREFIX/ctrld"
else
    mv "$TMP_BIN" "$PREFIX/ctrld"
    chmod +x "$PREFIX/ctrld"
fi

# --- verify ---
echo "==> Installed:"
"$PREFIX/ctrld" --version 2>&1 | head -1 || true
echo
echo "Next steps:"
echo "  ctrld run --cd <your-resolver-id>     # run in foreground"
echo "  ctrld start --cd <your-resolver-id>  # install as system service"
echo
echo "Docs: https://github.com/$REPO/releases/tag/$VERSION"
