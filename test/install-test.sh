#!/bin/sh
# Tests install.sh against the binaries `make dist` just built, served from
# disk through QIFA_DOWNLOAD_URL, so a broken installer fails CI before a tag
# publishes it.
#
#   test/install-test.sh        (make test-install; runs make dist first)
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/qifa-install-test-XXXXXX")
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

[ -f "$root/dist/SHA256SUMS" ] || fail "no dist/SHA256SUMS; run make dist first"

# A good release installs, and the binary runs.
QIFA_DOWNLOAD_URL="file://$root/dist" QIFA_INSTALL_DIR="$work/good" sh "$root/install.sh"
"$work/good/qifa" version >/dev/null || fail "the installed binary does not run"
echo "ok: installs and runs: $("$work/good/qifa" version)"

# A copy of the release whose checksums are wrong installs nothing.
mkdir -p "$work/bad-sums"
cp "$root"/dist/qifa-* "$work/bad-sums/"
sed 's/^[0-9a-f]\{64\}/0000000000000000000000000000000000000000000000000000000000000000/' \
  "$root/dist/SHA256SUMS" > "$work/bad-sums/SHA256SUMS"
if QIFA_DOWNLOAD_URL="file://$work/bad-sums" QIFA_INSTALL_DIR="$work/bad" sh "$root/install.sh" 2> "$work/bad.err"; then
  fail "installed despite a checksum mismatch"
fi
grep -q "checksum mismatch" "$work/bad.err" || fail "unexpected error: $(cat "$work/bad.err")"
[ ! -e "$work/bad/qifa" ] || fail "a binary was left behind after a checksum mismatch"
echo "ok: a checksum mismatch installs nothing"

# A release whose SHA256SUMS does not list the binary installs nothing.
mkdir -p "$work/no-entry"
cp "$root"/dist/qifa-* "$work/no-entry/"
: > "$work/no-entry/SHA256SUMS"
if QIFA_DOWNLOAD_URL="file://$work/no-entry" QIFA_INSTALL_DIR="$work/none" sh "$root/install.sh" 2> "$work/none.err"; then
  fail "installed with no checksum to check against"
fi
grep -q "lists no qifa-" "$work/none.err" || fail "unexpected error: $(cat "$work/none.err")"
[ ! -e "$work/none/qifa" ] || fail "a binary was left behind with no checksum entry"
echo "ok: a missing checksum entry installs nothing"

echo "PASS: install.sh"
