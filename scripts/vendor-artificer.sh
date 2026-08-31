#!/usr/bin/env bash
# Re-vendor the Artificer design system into internal/docs/assets/artificer/,
# or check the vendored pin against the npm registry.
#
# Usage:
#   scripts/vendor-artificer.sh            # re-vendor at the pinned version
#   scripts/vendor-artificer.sh --check    # advisory drift check (never fails CI)
#
# Bump PIN deliberately: walk the primitive-mint ledger (primitives.json
# versions{}.breaking[]) for every boundary crossed before swapping assets.
set -euo pipefail

PIN="0.25.0"
DEST="internal/docs/assets/artificer"
PKG="@cameronsjo/artificer"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	GREEN=$'\033[32m' YELLOW=$'\033[33m' RED=$'\033[31m' RESET=$'\033[0m'
else
	GREEN='' YELLOW='' RED='' RESET=''
fi

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

if [ "${1:-}" = "--check" ]; then
	vendored="$(jq -r .version "$DEST/provenance.json")"
	latest="$(npm view "$PKG" version)"
	if [ "$vendored" = "$latest" ]; then
		echo "${GREEN}CURRENT${RESET}: vendored Artificer $vendored matches the latest npm release"
	else
		echo "${YELLOW}DRIFT${RESET}: vendored Artificer $vendored, npm latest is $latest — advisory only; re-vendor via this script after walking the ledger"
	fi
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
