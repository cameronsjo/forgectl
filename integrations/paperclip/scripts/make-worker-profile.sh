#!/usr/bin/env bash
# Build a curated Claude Code config dir for Paperclip workers and register it
# as a forgectl surface profile. The dir shares plugins, rules, skills and
# project memory with ~/.claude by symlink; only settings.json is its own file,
# derived from ~/.claude/settings.json with the session mods switched off and
# the two dialogs that stall an unattended pane disabled.
#
# Usage: make-worker-profile.sh [--dry-run] [--refresh] [--name NAME]
#   --refresh   rewrite settings.json from the current ~/.claude/settings.json
#   --name      profile name and dir suffix (default: worker -> ~/.claude-worker)
set -euo pipefail

MODS=(sploot c3 atelier board verbs guard-toast herdr-bridge)
SHARED=(CLAUDE.md AGENTS.md agents commands docs hooks rules skills plugins projects plans persona themes workflows)

main() {
  local dry=0 refresh=0 name=worker
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dry-run) dry=1 ;;
      --refresh) refresh=1 ;;
      --name) [[ $# -ge 2 ]] || usage; name=$2; shift ;;
      -h|--help|help) usage 0 ;;
      *) usage ;;
    esac
    shift
  done
  [[ "$name" =~ ^[a-z][a-z0-9-]{0,31}$ ]] || { echo "profile name must match [a-z][a-z0-9-]*" >&2; exit 2; }

  local src="$HOME/.claude" dir="$HOME/.claude-$name"
  local fconf="$HOME/Library/Application Support/forgectl/config.toml"
  [[ "$(uname -s)" == Darwin ]] || fconf="${XDG_CONFIG_HOME:-$HOME/.config}/forgectl/config.toml"
  run() { if ((dry)); then printf 'would: %s\n' "$*"; else "$@"; fi; }

  [[ -d "$dir" ]] || run mkdir -m 0700 "$dir"
  local item
  for item in "${SHARED[@]}"; do
    [[ -e "$src/$item" ]] || continue
    if [[ -L "$dir/$item" || ! -e "$dir/$item" ]]; then
      run ln -sfn "$src/$item" "$dir/$item"
    fi
  done

  local settings="$dir/settings.json"
  if [[ -L "$settings" ]]; then
    echo "$settings is a symlink; remove it so the worker profile gets its own settings" >&2
    exit 1
  fi
  if [[ ! -e "$settings" || $refresh -eq 1 ]]; then
    local mods_json
    mods_json=$(printf '%s\n' "${MODS[@]}" | jq -R . | jq -sc .)
    local tmp
    tmp=$(mktemp)
    jq --argjson mods "$mods_json" '
      .enabledPlugins = ((.enabledPlugins // {}) | with_entries(
        if (.key | split("@")[0]) as $p | $mods | index($p) then .value = false else . end))
      # Mods install from workbench; pin them off even when the user file never named them.
      | .enabledPlugins += ($mods | map({key: (. + "@workbench"), value: false}) | from_entries)
      | .promptSuggestionEnabled = false
      | .skillOverrides = ((.skillOverrides // {}) + {"auto-mode-setup": "off"})
    ' "$src/settings.json" > "$tmp"
    if ((dry)); then
      echo "would write $settings; mods now off:"
      jq -r --argjson mods "$mods_json" '.enabledPlugins | to_entries[]
        | select(.value == false and ((.key | split("@")[0]) as $p | $mods | index($p))) | "  " + .key' "$tmp"
      rm -f "$tmp"
    else
      install -m 0600 "$tmp" "$settings"
      rm -f "$tmp"
      echo "wrote $settings"
    fi
  else
    echo "$settings exists; pass --refresh to rebuild it"
  fi

  if [[ -f "$fconf" ]] && command grep -Eq "surface\.profiles\.\"?$name\"?([] .=]|$)|profiles *= *\{" "$fconf"; then
    echo "forgectl profile $name, or an inline profiles table, is already in $fconf; add the profile by hand if missing"
  elif ((dry)); then
    echo "would append [surface.profiles.$name] config_dir = \"~/.claude-$name\" to $fconf"
  else
    mkdir -p "$(dirname "$fconf")"
    printf '\n[surface.profiles.%s]\nconfig_dir = "~/.claude-%s"\n' "$name" "$name" >> "$fconf"
    echo "registered forgectl profile $name"
  fi
  echo "next: CLAUDE_CONFIG_DIR=$dir claude, then /login and trust each worker repo root once"
}

usage() { sed -n '9,11p' "$0" >&2; exit "${1:-2}"; }

main "$@"
