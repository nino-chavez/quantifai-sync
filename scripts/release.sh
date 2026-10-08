#!/usr/bin/env bash
# release.sh — Build, publish and verify a quantifai-sync release.
#
# Usage:
#   scripts/release.sh vX.Y.Z --notes FILE [--commit SHA] [--yes]
#   scripts/release.sh vX.Y.Z --notes FILE --dry-run
#   scripts/release.sh vX.Y.Z --verify-only
#
# Steps:
#   1. Preflight: the version is new (no tag, no release), the commit
#      (default: origin/main) passed the linux and windows CI workflows.
#   2. Build in a temporary worktree at that commit: go test, make
#      cross-build, and check each binary's stamped version.
#   3. Package: one .tar.gz per macOS/Linux binary, a .zip for Windows, and
#      a .sha256 per archive. Agents already deployed depend on these names
#      and on the binary name inside each archive (internal/updater).
#   4. Publish (asks first unless --yes): annotated tag, push, GitHub release.
#   5. Verify the published release:
#      - every asset downloads again and matches its .sha256;
#      - the REST API (how v0.4.0 and v0.4.1 find releases) and the
#        releases/latest redirect (v0.4.2 on) both name this version;
#      - the previous release's binary for this machine, run in isolation
#        with a fake key and a dead API URL, updates itself to this version
#        and installs a byte-identical binary.
#
# --dry-run stops after step 3 and keeps the archives. --verify-only runs
# step 5 for a release that is already published.
#
# The Homebrew tap is a separate step: packaging/homebrew/bump-tap.sh.
#
# Not GoReleaser: the archive layout above is a contract with every agent
# already deployed, the tap formula is a maintained template, and the
# post-publish checks are what this script is mostly for.
set -euo pipefail

REPO="nino-chavez/quantifai-sync"
PLATFORMS=(darwin-amd64 darwin-arm64 linux-amd64 linux-arm64)
WORKFLOWS=(linux windows)

usage() { sed -n '3,6p' "$0" | sed 's/^# //' >&2; exit 2; }
die() { echo "release: $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

VERSION="" NOTES="" COMMIT="" DRY_RUN=0 VERIFY_ONLY=0 YES=0
while [ $# -gt 0 ]; do
    case "$1" in
        --notes) NOTES="${2:?--notes needs a file}"; shift 2 ;;
        --commit) COMMIT="${2:?--commit needs a SHA}"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        --verify-only) VERIFY_ONLY=1; shift ;;
        --yes) YES=1; shift ;;
        -h|--help) usage ;;
        v*) VERSION="$1"; shift ;;
        *) usage ;;
    esac
done
[ -n "$VERSION" ] || usage
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must look like v1.2.3, got $VERSION"

for cmd in git gh go make curl shasum tar zip unzip; do
    command -v "$cmd" >/dev/null || die "$cmd is not installed"
done
ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/quantifai-release.XXXXXX")"
WORKTREE=""
cleanup() {
    if [ -n "$WORKTREE" ]; then git -C "$ROOT" worktree remove --force "$WORKTREE" 2>/dev/null || true; fi
    rm -rf "$SCRATCH"
}
trap cleanup EXIT

# --- step 5 -----------------------------------------------------------------

verify_release() {
    local v="$1" dl="$SCRATCH/readback"
    step "Verify the published $v"
    mkdir -p "$dl"
    gh release download "$v" -R "$REPO" --dir "$dl"
    local n
    n=$(find "$dl" -type f | wc -l | tr -d ' ')
    [ "$n" -eq 10 ] || die "release has $n files, want 10"
    (cd "$dl" && shasum -a 256 -c ./*.sha256) || die "a downloaded asset does not match its .sha256"

    local api
    api=$(gh api "repos/$REPO/releases/latest" --jq '"\(.tag_name) \(.assets|length) \(.draft) \(.prerelease)"')
    [ "$api" = "$v 10 false false" ] || die "REST API latest release is '$api', want '$v 10 false false'"
    echo "REST API latest release: $v, 10 assets"

    local loc
    loc=$(curl -sSI "https://github.com/$REPO/releases/latest" | tr -d '\r' | awk 'tolower($1)=="location:" {print $2}')
    [ "$loc" = "https://github.com/$REPO/releases/tag/$v" ] || die "releases/latest redirects to '$loc'"
    echo "releases/latest redirects to $v"

    self_update_check "$v" "$dl"
}

# The previous release's binary for this machine must update itself to $v.
self_update_check() {
    local v="$1" dl="$2" os arch asset prev
    os=$(go env GOHOSTOS) arch=$(go env GOHOSTARCH)
    if [ "$os" = windows ]; then echo "skipping the self-update check on Windows"; return; fi
    asset="quantifai-sync-$os-$arch.tar.gz"
    prev=$(gh release list -R "$REPO" --exclude-drafts --exclude-pre-releases --limit 10 --json tagName --jq '.[].tagName' \
        | grep -vx "$v" | head -1)
    [ -n "$prev" ] || die "no earlier release to update from"
    step "Self-update: $prev -> $v ($os/$arch, isolated)"

    local e="$SCRATCH/e2e" port=19899
    mkdir -p "$e/dl" "$e/bin" "$e/home" "$e/watch"
    gh release download "$prev" -R "$REPO" -p "$asset" -p "$asset.sha256" --dir "$e/dl"
    (cd "$e/dl" && shasum -a 256 -c "$asset.sha256" >/dev/null) || die "$prev $asset does not match its .sha256"
    tar xzf "$e/dl/$asset" -C "$e/bin"
    local bin="$e/bin/quantifai-sync"
    mv "$e/bin/quantifai-sync-$os-$arch" "$bin"
    while lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; do port=$((port + 1)); done

    # A scratch home, a keyring service that does not exist, a fake key and
    # an API URL nothing listens on: nothing is read from or sent to the
    # real agent's setup.
    env -i PATH=/usr/bin:/bin HOME="$e/home" USER=release-check TMPDIR="$SCRATCH" \
        QUANTIFAI_KEYRING_SERVICE=quantifai-sync-release-check-absent \
        QUANTIFAI_API_URL=http://127.0.0.1:9 QUANTIFAI_API_KEY=release-check-not-a-real-key \
        QUANTIFAI_WATCH_DIR="$e/watch" QUANTIFAI_STATE_FILE="$e/state.json" \
        QUANTIFAI_HEALTH_PORT="$port" QUANTIFAI_LOG_LEVEL=debug \
        QUANTIFAI_AUTO_UPDATE=true QUANTIFAI_UPDATE_REPO="$REPO" \
        "$bin" run >"$e/agent.log" 2>&1 &
    local pid=$! waited=0
    until { grep -q "\"version\":\"$v\"" "$e/agent.log" && grep -q "already up to date" "$e/agent.log"; } \
        || grep -q "update check failed\|restart into the new binary failed" "$e/agent.log" \
        || [ "$waited" -ge 120 ]; do
        sleep 2; waited=$((waited + 2))
    done
    local alive=no
    kill -0 "$pid" 2>/dev/null && alive=yes
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true

    local now installed published
    now=$("$bin" version 2>/dev/null || true)
    installed=$(shasum -a 256 "$bin" | cut -d' ' -f1)
    published=$(tar xzOf "$dl/$asset" | shasum -a 256 | cut -d' ' -f1)
    if [ "$alive" != yes ] || [ "$now" != "quantifai-sync $v" ] || [ "$installed" != "$published" ] \
        || ! grep -q "\"version\":\"$v\"" "$e/agent.log"; then
        echo "--- agent log:" >&2
        grep -o '"msg":"[^"]*"\|"error":"[^"]*"' "$e/agent.log" >&2 || cat "$e/agent.log" >&2
        die "self-update from $prev did not end on $v (binary: '$now', process alive: $alive)"
    fi
    echo "$prev updated itself to $v in the same process; installed binary matches the release"
}

if [ "$VERIFY_ONLY" -eq 1 ]; then
    verify_release "$VERSION"
    echo; echo "$VERSION verified."
    exit 0
fi

# --- step 1 -----------------------------------------------------------------

step "Preflight"
[ -n "$NOTES" ] || die "--notes FILE is required"
[ -s "$NOTES" ] || die "notes file $NOTES is missing or empty"
NOTES="$(cd "$(dirname "$NOTES")" && pwd)/$(basename "$NOTES")"
git -C "$ROOT" fetch -q --tags origin
COMMIT="$(git -C "$ROOT" rev-parse "${COMMIT:-origin/main}^{commit}")"
if git -C "$ROOT" rev-parse -q --verify "refs/tags/$VERSION" >/dev/null \
    || [ -n "$(git -C "$ROOT" ls-remote --tags origin "refs/tags/$VERSION")" ]; then
    die "tag $VERSION already exists"
fi
if gh release view "$VERSION" -R "$REPO" >/dev/null 2>&1; then die "release $VERSION already exists"; fi
for wf in "${WORKFLOWS[@]}"; do
    result=$(gh run list -R "$REPO" --commit "$COMMIT" --workflow "$wf" --limit 1 \
        --json status,conclusion --jq '.[0] | "\(.status) \(.conclusion)"')
    if [ "$result" != "completed success" ]; then
        msg="$wf CI on ${COMMIT:0:7} is '${result:-not run}', want 'completed success'"
        if [ "$DRY_RUN" -eq 1 ]; then echo "warning: $msg"; else die "$msg"; fi
    else
        echo "$wf CI passed on ${COMMIT:0:7}"
    fi
done
echo "$VERSION is free; building $(git -C "$ROOT" log -1 --format='%h %s' "$COMMIT")"

# --- step 2 -----------------------------------------------------------------

step "Build $VERSION"
WORKTREE="$SCRATCH/worktree"
git -C "$ROOT" worktree add -q --detach "$WORKTREE" "$COMMIT"
(cd "$WORKTREE" && go test ./... >"$SCRATCH/test.log" 2>&1) || { tail -30 "$SCRATCH/test.log" >&2; die "go test failed"; }
echo "go test passed"
(cd "$WORKTREE" && make cross-build VERSION="$VERSION" >"$SCRATCH/build.log" 2>&1) || { tail -30 "$SCRATCH/build.log" >&2; die "make cross-build failed"; }
for b in "$WORKTREE"/bin/quantifai-sync-*; do
    go version -m "$b" | grep -q "cmd.Version=$VERSION" || die "$(basename "$b") is not stamped $VERSION"
done
echo "5 binaries built, each stamped $VERSION"

# --- step 3 -----------------------------------------------------------------

step "Package"
DIST="$SCRATCH/dist"
mkdir -p "$DIST"
for p in "${PLATFORMS[@]}"; do
    (cd "$WORKTREE/bin" && COPYFILE_DISABLE=1 tar czf "$DIST/quantifai-sync-$p.tar.gz" "quantifai-sync-$p")
    [ "$(tar tzf "$DIST/quantifai-sync-$p.tar.gz")" = "quantifai-sync-$p" ] || die "quantifai-sync-$p.tar.gz does not hold exactly its binary"
done
(cd "$WORKTREE/bin" && zip -q -X "$DIST/quantifai-sync-windows-amd64.zip" quantifai-sync-windows-amd64.exe)
[ "$(unzip -Z1 "$DIST/quantifai-sync-windows-amd64.zip")" = "quantifai-sync-windows-amd64.exe" ] || die "the Windows zip does not hold exactly its binary"
(cd "$DIST" && for f in *.tar.gz *.zip; do shasum -a 256 "$f" >"$f.sha256"; done && shasum -a 256 -c ./*.sha256 >/dev/null)
(cd "$DIST" && for f in *; do echo "$(wc -c <"$f" | tr -d ' ') $f"; done)

if [ "$DRY_RUN" -eq 1 ]; then
    OUT="${TMPDIR:-/tmp}/quantifai-sync-$VERSION-dist"
    rm -rf "$OUT" && cp -R "$DIST" "$OUT"
    echo; echo "Dry run: nothing tagged or published. Archives kept in $OUT"
    exit 0
fi

# --- step 4 -----------------------------------------------------------------

step "Publish"
if [ "$YES" -ne 1 ]; then
    read -r -p "Publish $VERSION from ${COMMIT:0:7} to github.com/$REPO? [y/N] " answer
    [ "$answer" = y ] || [ "$answer" = Y ] || die "not published"
fi
git -C "$ROOT" tag -a "$VERSION" -m "$VERSION" "$COMMIT"
git -C "$ROOT" push -q origin "$VERSION"
gh release create "$VERSION" -R "$REPO" --verify-tag --title "$VERSION" --notes-file "$NOTES" "$DIST"/*

verify_release "$VERSION"

echo
echo "$VERSION published and verified."
echo "Next: packaging/homebrew/bump-tap.sh $VERSION"
echo "Optional: re-run Windows CI on ${COMMIT:0:7} to self-update a build of it to $VERSION under the logon task."
