#!/usr/bin/env bash
# Re-vendor the Claude Code settings JSON schema used as a test fixture:
#
#   internal/pr/testdata/claude-code-settings.schema.json
#
# TestReviewSettingsJSON_ValidatesAgainstTheSettingsSchema validates every
# --settings document the Claude reviewer is launched with against it
# (forgectl#694): a wrong-typed key makes Claude Code drop the whole document
# silently in -p mode. Refresh it when Claude Code's settings change, then run
# `go test ./internal/pr/` — a new schema can turn the reviewer's document red,
# which is the point.
#
# Source: SchemaStore (https://json.schemastore.org/claude-code-settings.json),
# Apache-2.0. The licence text and SchemaStore's NOTICE live beside the fixture
# in internal/pr/testdata/LICENSE-schemastore; keep them when re-vendoring.
#
# The script downloads to a temp file, refuses anything that is not a draft-07
# JSON schema titled "Claude Code Settings", prepends a $comment recording the
# source, fetch date, Last-Modified and sha256 of the fetched bytes, and only
# then moves it into place. It prints the sha256 so the PR can quote it.
#
# Needs: curl, jq, sha256sum (or shasum on macOS).
set -euo pipefail

url="https://json.schemastore.org/claude-code-settings.json"
root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/internal/pr/testdata/claude-code-settings.schema.json"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -D "$tmp/headers" -o "$tmp/schema.json" "$url"

if command -v sha256sum >/dev/null 2>&1; then
	sha="$(sha256sum "$tmp/schema.json" | cut -d' ' -f1)"
else
	sha="$(shasum -a 256 "$tmp/schema.json" | cut -d' ' -f1)"
fi
last_modified="$(grep -i '^last-modified:' "$tmp/headers" | tail -n1 | cut -d' ' -f2- | tr -d '\r' || true)"
fetched="$(date -u +%Y-%m-%d)"

title="$(jq -r '.title // empty' "$tmp/schema.json")"
draft="$(jq -r '."$schema" // empty' "$tmp/schema.json")"
if [ "$title" != "Claude Code Settings" ] || [ "$draft" != "http://json-schema.org/draft-07/schema#" ]; then
	echo "refusing: $url is not the draft-07 Claude Code Settings schema (title=$title, \$schema=$draft)" >&2
	exit 1
fi

comment="Vendored test fixture for internal/pr/reviewsandbox_schema_test.go, written by scripts/vendor-claude-settings-schema.sh. Source: $url, fetched $fetched, Last-Modified ${last_modified:-unknown}, sha256 $sha of the fetched bytes. Copyright JSON Schema Store (Mads Kristensen and Contributors), licensed under the Apache License 2.0; see LICENSE-schemastore beside this file. Community-maintained, not authoritative: claude doctor is (see TestReviewSettingsJSON_ClaudeDoctorAcceptsIt). Unmodified apart from this \$comment key."

jq --arg c "$comment" '{"$comment": $c} + .' "$tmp/schema.json" > "$tmp/out.json"
mv "$tmp/out.json" "$dest"
echo "vendored $url"
echo "sha256 $sha"
echo "last-modified ${last_modified:-unknown}"
