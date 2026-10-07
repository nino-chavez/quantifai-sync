#!/usr/bin/env bash
# update-formula.sh — Render the Homebrew formula for a published release.
#
# Usage: ./update-formula.sh <version> <output>
#   e.g.: ./update-formula.sh v0.3.0 bin/quantifai-sync.rb
#
# Downloads the release's four macOS/Linux archives, computes their SHA-256
# checksums, and writes the filled-in formula to <output>. The template
# next to this script is never modified. Copy <output> to the tap's
# Formula/quantifai-sync.rb.
set -euo pipefail

VERSION="${1:?Usage: $0 <version> <output>}"
OUTPUT="${2:?Usage: $0 <version> <output>}"
VERSION="${VERSION#v}" # accept v0.3.0 (git describe) or 0.3.0
REPO="nino-chavez/quantifai-sync"
TEMPLATE="$(cd "$(dirname "$0")" && pwd)/quantifai-sync.rb"
BASE_URL="https://github.com/${REPO}/releases/download/v${VERSION}"

PLATFORMS=(
    "darwin-arm64"
    "darwin-amd64"
    "linux-arm64"
    "linux-amd64"
)

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "Rendering formula for v${VERSION}..." >&2

formula="${WORK}/formula.rb"
sed "s/VERSION/${VERSION}/g" "${TEMPLATE}" > "${formula}"

for platform in "${PLATFORMS[@]}"; do
    asset="quantifai-sync-${platform}.tar.gz"
    dest="${WORK}/${asset}"

    echo "  Downloading ${asset}..." >&2
    if ! curl -sSL -f -o "${dest}" "${BASE_URL}/${asset}"; then
        echo "error: could not download ${BASE_URL}/${asset}" >&2
        exit 1
    fi
    sha=$(shasum -a 256 "${dest}" | awk '{print $1}')
    echo "  SHA256: ${sha}" >&2

    # darwin-arm64 -> SHA256_DARWIN_ARM64
    placeholder="SHA256_$(echo "${platform}" | tr '[:lower:]-' '[:upper:]_')"
    sed -i.bak "s/${placeholder}/${sha}/g" "${formula}"
done

if grep -q 'SHA256_\|VERSION' "${formula}"; then
    echo "error: unfilled placeholder left in formula" >&2
    exit 1
fi

mkdir -p "$(dirname "${OUTPUT}")"
cp "${formula}" "${OUTPUT}"
echo "Formula written: ${OUTPUT}" >&2
