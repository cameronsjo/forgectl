#!/usr/bin/env bash
# Re-vendor the Artificer design system, or check the vendored pin.
#
# TWO artifacts, one version, two delivery channels:
#
#   internal/docs/assets/artificer/  — the WEB assets (css/js/tokens.json),
#       from the npm package, for `forgectl docs serve`.
#   internal/theme/artificer/        — the TERMINAL palette (_palette.json),
#       from the design system's git repo, for internal/theme.
#
# The palette is not on npm — themes/ is repo-only — so it is fetched from
# GitHub. That repo is PRIVATE, which rules out a raw.githubusercontent.com
# URL (it 404s unauthenticated); `gh api` carries the operator's credentials.
# It also has no tags or releases, so PALETTE_REF is a commit SHA rather than a
# version tag. A SHA is immutable, which is what a pin is for.
#
# The two pins are kept in step by construction: --check fails if the palette
# at PALETTE_REF does not carry the same $version as the npm pin.
#
# Usage:
#   scripts/vendor-artificer.sh            # re-vendor both artifacts
#   scripts/vendor-artificer.sh --check    # drift check (advisory on npm, strict on the pair)
#
# Bump PIN deliberately: walk the primitive-mint ledger (primitives.json
# versions{}.breaking[]) for every boundary crossed before swapping assets.
# Bump PALETTE_REF to a commit whose _palette.json carries the matching
# $version, then re-run `go generate ./internal/theme`.
set -euo pipefail

PIN="0.25.0"
DEST="internal/docs/assets/artificer"
PKG="@cameronsjo/artificer"

PALETTE_REPO="cameronsjo/artificer-design-system"
PALETTE_REF="c5aa91b4b92826d931c21706b26764eb8b892f4d"
PALETTE_PATH="themes/_palette.json"
PALETTE_DEST="internal/theme/artificer/_palette.json"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	GREEN=$'\033[32m' YELLOW=$'\033[33m' RED=$'\033[31m' RESET=$'\033[0m'
else
	GREEN='' YELLOW='' RED='' RESET=''
fi

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# fetch_palette writes the pinned _palette.json to $1.
#
# `gh api` rather than curl: the repo is private, so the request needs the
# operator's credentials, and gh already holds them.
fetch_palette() {
	local out=$1
	gh api "repos/$PALETTE_REPO/contents/$PALETTE_PATH?ref=$PALETTE_REF" \
		--jq '.content' | base64 -d >"$out"
}

if [ "${1:-}" = "--check" ]; then
	vendored="$(jq -r .version "$DEST/provenance.json")"
	latest="$(npm view "$PKG" version)"
	if [ "$vendored" = "$latest" ]; then
		echo "${GREEN}CURRENT${RESET}: vendored Artificer $vendored matches the latest npm release"
	else
		echo "${YELLOW}DRIFT${RESET}: vendored Artificer $vendored, npm latest is $latest — advisory only; re-vendor via this script after walking the ledger"
	fi

	# The pair check is NOT advisory. Two artifacts claiming to be one design
	# system at two different versions is the drift this whole file exists to
	# prevent, and it is invisible in either artifact alone.
	if [[ ! -f "$PALETTE_DEST" ]]; then
		echo "${RED}FAIL${RESET}: $PALETTE_DEST is missing — run this script with no arguments"
		exit 1
	fi
	palette_version="$(jq -r '."$version"' "$PALETTE_DEST")"
	if [[ "$palette_version" != "$vendored" ]]; then
		echo "${RED}FAIL${RESET}: vendored palette is \$version $palette_version but the npm assets are $vendored — the two pins have diverged"
		exit 1
	fi
	echo "${GREEN}OK${RESET}: palette \$version $palette_version matches the npm pin"
	exit 0
fi

echo "Vendoring $PKG@$PIN into $DEST (strict) ..."
rc=0
npx --yes "$PKG@$PIN" vendor --dest "$DEST" --strict || rc=$?
if [ "$rc" -ne 0 ]; then
	echo "${RED}FAIL${RESET}: artificer vendor exited $rc (7 = strict drift: hand-edited vendored files; resolve before re-vendoring)"
	exit "$rc"
fi

got="$(jq -r .version "$DEST/provenance.json")"
if [ "$got" != "$PIN" ]; then
	echo "${RED}FAIL${RESET}: provenance.json reports $got, expected $PIN"
	exit 1
fi
echo "${GREEN}OK${RESET}: vendored Artificer $PIN; provenance.json verified"

echo "Fetching $PALETTE_PATH from $PALETTE_REPO@${PALETTE_REF:0:12} ..."
mkdir -p "$(dirname "$PALETTE_DEST")"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
fetch_palette "$tmp"

# Assert the shape before overwriting a good file with a 404 body, an empty
# response, or an HTML error page — all of which arrive as bytes and none of
# which `gh` exit status alone rules out.
shape="$(jq -r 'if (has("dark") and has("light") and has("$version")) then "ok" else "bad" end' "$tmp" 2>/dev/null || echo bad)"
if [[ "$shape" != "ok" ]]; then
	echo "${RED}FAIL${RESET}: fetched palette is not a palette (missing dark/light/\$version) — $PALETTE_DEST left untouched"
	exit 1
fi

palette_version="$(jq -r '."$version"' "$tmp")"
if [[ "$palette_version" != "$PIN" ]]; then
	echo "${RED}FAIL${RESET}: palette at ${PALETTE_REF:0:12} is \$version $palette_version, expected $PIN — pick a commit whose palette matches the npm pin"
	exit 1
fi

mv "$tmp" "$PALETTE_DEST"
trap - EXIT
echo "${GREEN}OK${RESET}: vendored palette \$version $palette_version into $PALETTE_DEST"
echo "Next: go generate ./internal/theme"
