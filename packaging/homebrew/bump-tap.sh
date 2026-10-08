#!/usr/bin/env bash
# bump-tap.sh — Move the Homebrew tap's formula to a published release.
#
# Usage:
#   packaging/homebrew/bump-tap.sh vX.Y.Z [--tap DIR] [--dry-run]
#
# Steps:
#   1. Render the formula with update-formula.sh, and check each URL's
#      SHA-256 against the release's own .sha256 file.
#   2. Lint it (ruby -c, brew style). Stop here with --dry-run.
#   3. In a worktree of the tap (default: ../quantifai-homebrew-tap), commit
#      the formula and the README version note, push, and open a PR.
#   4. Wait for the GitGuardian check, then merge pinned to the commit it
#      scanned. Delete the branch and worktree; fast-forward the tap's main
#      checkout if it is on main and clean.
#   5. brew update, then brew fetch the formula with a scratch trust store
#      (XDG_CONFIG_HOME), so ~/.homebrew/trust.json is never touched.
#
# A tap already at the version is left alone.
set -euo pipefail

TAP_REPO="nino-chavez/quantifai-homebrew-tap"
REPO="nino-chavez/quantifai-sync"
PLATFORMS=(darwin-arm64 darwin-amd64 linux-arm64 linux-amd64)

usage() { sed -n '3,5p' "$0" | sed 's/^# //' >&2; exit 2; }
die() { echo "bump-tap: $*" >&2; exit 1; }
step() { echo; echo "== $*"; }

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(git -C "$HERE" rev-parse --show-toplevel)"
VERSION="" TAP="" DRY_RUN=0
while [ $# -gt 0 ]; do
    case "$1" in
        --tap) TAP="${2:?--tap needs a directory}"; shift 2 ;;
        --dry-run) DRY_RUN=1; shift ;;
        -h|--help) usage ;;
        v*) VERSION="$1"; shift ;;
        *) usage ;;
    esac
done
[ -n "$VERSION" ] || usage
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must look like v1.2.3, got $VERSION"
TAP="${TAP:-$(dirname "$ROOT")/quantifai-homebrew-tap}"
# A worktree of quantifai-sync sits two levels deeper; fall back to the
# main checkout's sibling.
if [ ! -d "$TAP/.git" ]; then
    main_root="$(dirname "$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir)")"
    TAP="$(dirname "$main_root")/quantifai-homebrew-tap"
fi
[ -d "$TAP/.git" ] || die "no tap checkout at $TAP (use --tap DIR)"
for cmd in git gh curl ruby brew shasum; do command -v "$cmd" >/dev/null || die "$cmd is not installed"; done

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/quantifai-bump-tap.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT
NUM="${VERSION#v}"
BRANCH="bump/quantifai-sync-$VERSION"
WT="$TAP/.worktrees/bump-$VERSION"

git -C "$TAP" fetch -q --prune origin
current=$(git -C "$TAP" show origin/main:Formula/quantifai-sync.rb | sed -n 's/^ *version "\(.*\)"/\1/p' | head -1)
if [ "$current" = "$NUM" ]; then
    echo "The tap's formula is already at $VERSION; nothing to do."
    exit 0
fi
echo "tap formula: $current -> $NUM"

# --- steps 1 and 2 ----------------------------------------------------------

step "Render and check the formula"
formula="$SCRATCH/Formula/quantifai-sync.rb"
mkdir -p "$SCRATCH/Formula"
"$HERE/update-formula.sh" "$VERSION" "$formula" >/dev/null 2>"$SCRATCH/render.log" || { cat "$SCRATCH/render.log" >&2; die "update-formula.sh failed"; }
paste -d' ' <(grep -o 'url "[^"]*"' "$formula" | cut -d'"' -f2) <(grep -o 'sha256 "[^"]*"' "$formula" | cut -d'"' -f2) >"$SCRATCH/pairs"
[ "$(wc -l <"$SCRATCH/pairs" | tr -d ' ')" -eq "${#PLATFORMS[@]}" ] || die "formula has $(wc -l <"$SCRATCH/pairs") URL/SHA pairs, want ${#PLATFORMS[@]}"
while read -r url sha; do
    asset=$(basename "$url")
    [ "$url" = "https://github.com/$REPO/releases/download/$VERSION/$asset" ] || die "unexpected URL $url"
    want=$(curl -sSLf "https://github.com/$REPO/releases/download/$VERSION/$asset.sha256" | cut -d' ' -f1)
    [ -n "$want" ] && [ "$sha" = "$want" ] || die "$asset: formula SHA $sha, release .sha256 says '$want'"
    echo "OK   $asset"
done <"$SCRATCH/pairs"
ruby -c "$formula" >/dev/null || die "ruby -c failed"
brew style "$formula" >"$SCRATCH/style.log" 2>&1 || { cat "$SCRATCH/style.log" >&2; die "brew style failed"; }
echo "ruby -c and brew style pass"

if [ "$DRY_RUN" -eq 1 ]; then
    echo; echo "Dry run: changes against the tap's main:"
    diff <(git -C "$TAP" show origin/main:Formula/quantifai-sync.rb) "$formula" || true
    exit 0
fi

# --- step 3 -----------------------------------------------------------------

step "Open the tap PR"
git -C "$TAP" worktree add -q "$WT" -b "$BRANCH" origin/main
cp "$formula" "$WT/Formula/quantifai-sync.rb"
sed -i.bak -E "s/(real, tagged \`)v[0-9.]+(\` release)/\1$VERSION\2/" "$WT/README.md" && rm "$WT/README.md.bak"
# Only the version, URLs and SHA-256s may change, plus the README note.
unexpected=$(git -C "$WT" diff -U0 -- Formula | grep '^[-+] ' | grep -vE '^[-+] +(version|url|sha256) "' || true)
[ -z "$unexpected" ] || die "the formula diff changes more than version, URLs and SHA-256s:
$unexpected"
git -C "$WT" add Formula/quantifai-sync.rb README.md
git -C "$WT" commit -q -m "feat: bump quantifai-sync to $VERSION" -m "Rendered with quantifai-sync's update-formula.sh $VERSION. Only the
version, the four URLs and their SHA-256s change; each pair matches the
$VERSION release's published .sha256. README status note names $VERSION."
head=$(git -C "$WT" rev-parse HEAD)
git -C "$WT" push -q -u origin "$BRANCH"
pr_url=$(gh pr create -R "$TAP_REPO" --base main --head "$BRANCH" --title "feat: bump quantifai-sync to $VERSION" --body "Moves the formula from v$current to [$VERSION](https://github.com/$REPO/releases/tag/$VERSION), rendered with \`packaging/homebrew/update-formula.sh $VERSION\` from quantifai-sync.

- Only \`version\`, the four URLs and their SHA-256s change. Each URL is paired with the checksum in that archive's published \`.sha256\`; all four match.
- README status note: \`v$current\` -> \`$VERSION\`.

\`ruby -c\` passes; \`brew style\` reports no offenses.")
echo "$pr_url"

# --- step 4 -----------------------------------------------------------------

step "Wait for GitGuardian, then merge"
waited=0
while :; do
    state=$(gh pr checks "$BRANCH" -R "$TAP_REPO" 2>/dev/null | awk -F'\t' '$1 ~ /GitGuardian/ {print $2}')
    case "$state" in
        pass) echo "GitGuardian passed"; break ;;
        fail) die "GitGuardian failed on $pr_url; not merging" ;;
    esac
    [ "$waited" -lt 600 ] || die "GitGuardian has not reported after 10 minutes; not merging ($pr_url)"
    sleep 10; waited=$((waited + 10))
done
gh pr merge "$pr_url" --merge --match-head-commit "$head"
git -C "$TAP" fetch -q --prune origin
git -C "$TAP" merge-base --is-ancestor "$head" origin/main || die "merged commit is not in the tap's main"
git -C "$TAP" push -q origin --delete "$BRANCH" 2>/dev/null || true
git -C "$TAP" worktree remove "$WT"
git -C "$TAP" branch -q -D "$BRANCH"
if [ "$(git -C "$TAP" branch --show-current)" = main ] && [ -z "$(git -C "$TAP" status --porcelain)" ]; then
    git -C "$TAP" pull -q --ff-only && echo "tap checkout fast-forwarded to $(git -C "$TAP" log -1 --format=%h)"
else
    echo "tap checkout not on a clean main; left as is"
fi

# --- step 5 -----------------------------------------------------------------

step "Check through Homebrew"
trust="$HOME/.homebrew/trust.json"
before=$(shasum "$trust" 2>/dev/null || true)
brew update -q >/dev/null 2>&1 || true
fetched=$(XDG_CONFIG_HOME="$SCRATCH/xdg" brew fetch quantifai-sync 2>&1 | tail -2)
echo "$fetched"
echo "$fetched" | grep -q "($NUM)" || die "brew fetch did not get $NUM"
[ "$before" = "$(shasum "$trust" 2>/dev/null || true)" ] || die "$trust changed"
echo; echo "Tap bumped to $VERSION: $pr_url"
