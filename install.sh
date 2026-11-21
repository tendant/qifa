#!/bin/sh
# Install qifa from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/tendant/qifa/main/install.sh | sh
#
# Settings, all optional, as environment variables:
#   QIFA_VERSION      a release tag such as v0.6.0 (default: the latest release)
#   QIFA_INSTALL_DIR  where to put the binary (default: /usr/local/bin if you
#                     can write there, else ~/.local/bin). Never uses sudo.
#   QIFA_DOWNLOAD_URL base URL holding qifa-<os>-<arch> and SHA256SUMS, for a
#                     mirror (default: the GitHub release of QIFA_VERSION)
#
# The binary is checked against the release's SHA256SUMS before it is
# installed; a mismatch installs nothing.
set -eu

repo="tendant/qifa"

say() { printf 'qifa install: %s\n' "$*" >&2; }
fail() { say "$*"; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "no release binary for $(uname -s); build from source: go install github.com/$repo/cmd/qifa@latest" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "no release binary for $(uname -m); build from source: go install github.com/$repo/cmd/qifa@latest" ;;
esac
asset="qifa-$os-$arch"

if [ -n "${QIFA_DOWNLOAD_URL:-}" ]; then
  base="${QIFA_DOWNLOAD_URL%/}"
elif [ -n "${QIFA_VERSION:-}" ]; then
  base="https://github.com/$repo/releases/download/$QIFA_VERSION"
else
  base="https://github.com/$repo/releases/latest/download"
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "needs curl or wget to download"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "needs sha256sum or shasum to verify the download"
fi

tmp="$(mktemp -d)"
trap 'rm -f "$tmp/$asset" "$tmp/SHA256SUMS"; rmdir "$tmp" 2>/dev/null || true' EXIT

say "downloading $asset from $base"
fetch "$base/$asset" "$tmp/$asset" || fail "download of $base/$asset failed"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "download of $base/SHA256SUMS failed"

want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$want" ] || fail "SHA256SUMS lists no $asset"
got="$(sha256 "$tmp/$asset")"
[ "$got" = "$want" ] || fail "checksum mismatch for $asset (got $got, want $want); nothing installed"

if [ -n "${QIFA_INSTALL_DIR:-}" ]; then
  dir="$QIFA_INSTALL_DIR"
elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
  dir=/usr/local/bin
else
  dir="$HOME/.local/bin"
fi
mkdir -p "$dir" || fail "cannot create $dir; set QIFA_INSTALL_DIR to a directory you can write"
chmod 755 "$tmp/$asset"
mv "$tmp/$asset" "$dir/qifa" || fail "cannot write $dir/qifa; set QIFA_INSTALL_DIR to a directory you can write"

say "installed $("$dir/qifa" version) to $dir/qifa"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "$dir is not on your PATH; add it: export PATH=\"$dir:\$PATH\"" ;;
esac
say "next: qifa init && qifa deploy"
