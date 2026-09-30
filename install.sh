#!/bin/sh
# Installs the Openite CLI for the current user (no sudo) on Linux/macOS, verifying its checksum.
#   curl -fsSL https://raw.githubusercontent.com/kstasielowicz/openite/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --version v0.3.0 --preset developer --yes
# It only downloads the release binary + SHA256SUMS over HTTPS, verifies, and copies to ~/.local/bin.
# Read it before running it.
set -eu
REPO=${OPENITE_REPO:-kstasielowicz/openite}
VERSION=latest; ARGS=""; YES=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION=$2; shift 2 ;;
    --preset)  ARGS="$ARGS --preset $2"; shift 2 ;;
    --yes|-y)  YES="-y"; shift ;;
    *)         ARGS="$ARGS $1"; shift ;;
  esac
done

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; armv7l|armv6l) arch=arm ;; *) echo "unsupported CPU" >&2; exit 1 ;; esac
asset="openite-$os-$arch"
if [ "$VERSION" = latest ]; then base="https://github.com/$REPO/releases/latest/download"; else base="https://github.com/$REPO/releases/download/$VERSION"; fi

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
echo "Downloading $asset ($VERSION)..."
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
expected=$(grep " \*\?$asset\$" "$tmp/SHA256SUMS" | awk '{print $1}' | head -n1)
[ -n "$expected" ] || { echo "SHA256SUMS has no entry for $asset" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$asset" | awk '{print $1}'); else actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}'); fi
[ "$expected" = "$actual" ] || { echo "Checksum mismatch! expected $expected got $actual. Not installing." >&2; exit 1; }
echo "Checksum OK."
if command -v gh >/dev/null 2>&1; then gh attestation verify "$tmp/$asset" --repo "$REPO" >/dev/null 2>&1 && echo "GitHub build attestation verified." || { echo "Attestation check FAILED. Not installing." >&2; exit 1; }; fi

dest="$HOME/.local/bin"; mkdir -p "$dest"
install -m 0755 "$tmp/$asset" "$dest/openite"
echo "Installed to $dest/openite"
case ":$PATH:" in *":$dest:"*) ;; *) echo "Add $dest to your PATH to use 'openite' directly." ;; esac

if [ -n "$ARGS" ]; then
  # shellcheck disable=SC2086
  exec "$dest/openite" install $ARGS $YES
fi
echo "Done. Try: openite pick   |   openite ui   |   openite help"
