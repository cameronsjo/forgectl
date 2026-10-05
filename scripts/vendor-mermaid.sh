#!/usr/bin/env bash
# Re-vendor mermaid.min.js and regenerate its third-party notices.
#
#   internal/docs/assets/mermaid.min.js
#   internal/docs/assets/font-licenses/THIRD_PARTY_NOTICES-mermaid.txt
#
# The pin lives in internal/docs/assets/provenance-mermaid.json: package,
# version, the sha256 of dist/mermaid.min.js, the sha256 of the npm tarball,
# and mermaid's direct dependency ranges. The script downloads that exact
# tarball, refuses to continue on any hash or dependency-list mismatch, writes
# mermaid.min.js from the tarball, and generates the notices file from the
# tarball's production dependency tree.
#
# Why notices: the minified bundle inlines dozens of libraries (marked, d3-*,
# dagre-d3-es, dayjs, langium, chevrotain, ...) but its inline banner carries
# only a few notices. MIT/ISC/BSD require the notice to travel with copies, and
# DOMPurify's Apache-2.0/MPL-2.0 requires the license text. The notices file
# rides the font-licenses/* goreleaser entries into both archives.
#
# The notices come from the tree the bundle was built from, not from a fresh
# resolve. The npm tarball ships no lockfile, and a fresh resolve picks
# @mermaid-js/parser 1.2.1, which bundles langium and chevrotain itself and
# lists them only as devDependencies, so they vanish from the tree. Instead the
# provenance file's notice_resolution pins each direct dependency (and the
# parser subtree) to the version mermaid's own pnpm-lock.yaml resolved at the
# pinned tag, and adds extra_dependencies for libraries the bundle inlines that
# the published dependency list omits. Transitive packages below those pins
# still resolve fresh within their ranges, so a version in the notices can
# differ by a patch or minor from the inlined copy; license texts are stable
# across such versions, and the file header says so. The listing is a superset
# (it includes type-only @types/* packages and packages the bundle may not
# include).
#
# Cross-check: provenance's bundle_markers maps a literal string found in
# mermaid.min.js to the package whose notice it requires. The script fails if a
# marker is in the bundle and its package has no notice. The Go test repeats the
# check on the committed files.
#
# mermaid.min.js and the notices are both built in a temp dir, verified, and
# only then moved into place, so a failure leaves the tree untouched.
#
# To bump mermaid: change "version" and the "dependencies" ranges in the
# provenance file, blank both sha256 fields, run with --print-hashes to see the
# new values, record them, then run this script normally.
#
# Usage:
#   scripts/vendor-mermaid.sh                # vendor + regenerate notices
#   scripts/vendor-mermaid.sh --print-hashes # download and print hashes only
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

ASSETS="internal/docs/assets"
PROV="$ASSETS/provenance-mermaid.json"
NOTICES="$ASSETS/font-licenses/THIRD_PARTY_NOTICES-mermaid.txt"

case "${1:-}" in
"" | --print-hashes) ;;
*)
	echo "usage: scripts/vendor-mermaid.sh [--print-hashes]" >&2
	exit 2
	;;
esac
[ "$#" -le 1 ] || { echo "usage: scripts/vendor-mermaid.sh [--print-hashes]" >&2; exit 2; }

for tool in jq node npm curl sha256sum tar; do
	command -v "$tool" >/dev/null || { echo "vendor-mermaid: $tool not found" >&2; exit 1; }
done

VERSION="$(jq -r '.files[0].version' "$PROV")"
WANT_JS_SHA="$(jq -r '.files[0].sha256' "$PROV")"
WANT_TGZ_SHA="$(jq -r '.files[0].tarball_sha256' "$PROV")"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/mermaid.tgz" "https://registry.npmjs.org/mermaid/-/mermaid-$VERSION.tgz"
tgz_sha="$(sha256sum "$work/mermaid.tgz" | cut -d' ' -f1)"
mkdir "$work/pkg"
tar -xzf "$work/mermaid.tgz" -C "$work/pkg"
js_sha="$(sha256sum "$work/pkg/package/dist/mermaid.min.js" | cut -d' ' -f1)"

if [ "${1:-}" = "--print-hashes" ]; then
	echo "tarball_sha256 $tgz_sha"
	echo "sha256         $js_sha"
	exit 0
fi

if [ "$tgz_sha" != "$WANT_TGZ_SHA" ]; then
	echo "vendor-mermaid: tarball sha256 $tgz_sha != pinned $WANT_TGZ_SHA" >&2
	exit 1
fi
if [ "$js_sha" != "$WANT_JS_SHA" ]; then
	echo "vendor-mermaid: dist/mermaid.min.js sha256 $js_sha != pinned $WANT_JS_SHA" >&2
	exit 1
fi

# The recorded dependency list must be the tarball's, so a bump cannot leave
# stale provenance behind.
got_deps="$(jq -S '.dependencies' "$work/pkg/package/package.json")"
want_deps="$(jq -S '.files[0].dependencies' "$PROV")"
if [ "$got_deps" != "$want_deps" ]; then
	echo "vendor-mermaid: tarball dependencies differ from $PROV" >&2
	diff <(echo "$want_deps") <(echo "$got_deps") >&2 || true
	exit 1
fi

# Production tree the bundle was built from: mermaid itself, the extra inlined
# libraries, and overrides pinning every resolved version named in provenance.
mkdir "$work/inst"
jq --arg tgz "$work/mermaid.tgz" '
	.files[0].notice_resolution as $r
	| {name: "mermaid-notices", private: true,
	   dependencies: ({mermaid: ("file:" + $tgz)} + $r.extra_dependencies),
	   overrides: $r.overrides}' "$PROV" >"$work/inst/package.json"
(cd "$work/inst" && npm install --omit=dev --ignore-scripts --no-audit --no-fund \
	--no-package-lock >/dev/null)

MERMAID_VERSION="$VERSION" NODE_MODULES="$work/inst/node_modules" OUT="$work/notices.txt" node - <<'JS'
const fs = require("fs");
const path = require("path");
const root = process.env.NODE_MODULES;

function walk(dir, out) {
  for (const name of fs.readdirSync(dir).sort()) {
    if (name[0] === ".") continue;
    const full = path.join(dir, name);
    if (name[0] === "@") { walk(full, out); continue; }
    if (!fs.existsSync(path.join(full, "package.json"))) continue;
    out.push(full);
    const nested = path.join(full, "node_modules");
    if (fs.existsSync(nested)) walk(nested, out);
  }
  return out;
}

const pkgs = new Map();
for (const dir of walk(root, [])) {
  const j = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
  if (j.name === "mermaid") continue; // its MIT notice is Mermaid-MIT.txt
  const files = fs.readdirSync(dir)
    .filter((f) => /^(licen[sc]e|copying|notice)(\.|-|$)/i.test(f) && fs.statSync(path.join(dir, f)).isFile())
    .sort();
  pkgs.set(`${j.name}@${j.version}`, { j, dir, files });
}

const lines = [];
lines.push(`Third-party notices for mermaid ${process.env.MERMAID_VERSION} (mermaid.min.js)`);
lines.push("");
lines.push("GENERATED by scripts/vendor-mermaid.sh. Do not edit by hand.");
lines.push("");
lines.push("mermaid.min.js inlines the libraries below. Each entry reproduces the");
lines.push("LICENSE file published in that package's npm tarball. The dependency tree");
lines.push("was pinned to the versions mermaid's own lockfile resolved at this release,");
lines.push("with transitive packages resolved within their ranges, so a version here can");
lines.push("differ by a patch or minor from the copy inlined in the bundle; license");
lines.push("texts are stable across such versions. The list is a superset: it also");
lines.push("carries type-only @types/* packages and dependencies the bundle may not");
lines.push("include. License texts are verbatim.");
lines.push("mermaid's own MIT notice is Mermaid-MIT.txt.");
lines.push("");
const ids = [...pkgs.keys()].sort();
lines.push("Packages:");
for (const id of ids) {
  const { j } = pkgs.get(id);
  const lic = typeof j.license === "string" ? j.license : (j.license && j.license.type) || "see text";
  lines.push(`  ${id} (${lic})`);
}
for (const id of ids) {
  const { j, dir, files } = pkgs.get(id);
  lines.push("");
  lines.push("=".repeat(78));
  lines.push(id);
  lines.push(`License: ${typeof j.license === "string" ? j.license : JSON.stringify(j.license) || "unspecified"}`);
  lines.push("=".repeat(78));
  lines.push("");
  const texts = files.filter((f) => !/\.(mjs|js|json)$/i.test(f));
  if (texts.length === 0) {
    console.error(`vendor-mermaid: no license file found for ${id}`);
    process.exit(1);
  }
  texts.forEach((f, i) => {
    if (texts.length > 1) lines.push(`--- ${f} ---`, "");
    lines.push(fs.readFileSync(path.join(dir, f), "utf8").replace(/\r\n/g, "\n").replace(/\s+$/, ""));
    if (i < texts.length - 1) lines.push("");
  });
}
fs.writeFileSync(process.env.OUT, lines.join("\n") + "\n");
JS

cp "$work/pkg/package/dist/mermaid.min.js" "$work/mermaid.min.js"

# Bundle-to-notices cross-check.
missing=0
while IFS=$'\t' read -r marker pkg; do
	if grep -qF -- "$marker" "$work/mermaid.min.js" && ! grep -q -- "^  $pkg@" "$work/notices.txt"; then
		echo "vendor-mermaid: bundle contains '$marker' but the notices have no entry for $pkg" >&2
		missing=1
	fi
done < <(jq -r '.files[0].bundle_markers | to_entries[] | [.key, .value] | @tsv' "$PROV")
[ "$missing" -eq 0 ] || exit 1

# Everything verified: move both artifacts into place.
cp "$work/mermaid.min.js" "$ASSETS/mermaid.min.js.tmp.$$"
mv "$ASSETS/mermaid.min.js.tmp.$$" "$ASSETS/mermaid.min.js"
cp "$work/notices.txt" "$NOTICES.tmp.$$"
mv "$NOTICES.tmp.$$" "$NOTICES"
[ "$(sha256sum "$ASSETS/mermaid.min.js" | cut -d' ' -f1)" = "$WANT_JS_SHA" ]

echo "vendored mermaid $VERSION; wrote $NOTICES"
