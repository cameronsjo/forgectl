#!/usr/bin/env bash
# Run Paperclip as a pinned launchd/systemd service (instance "forgectl") with
# the herdr_forgectl adapter registered. Leaves every other Paperclip instance
# alone. Safe to re-run; it never overwrites the operator settings file.
# Usage: install-service.sh --repos /abs/repo[:/abs/repo...] [--reviewer USER_ID] [--dry-run]
# Env:   PAPERCLIP_HOME (default ~/.paperclip)
set -euo pipefail

VERSION=2026.1005.0 INSTANCE=forgectl PORT=3110 PG_PORT=54339
HOME_DIR=${PAPERCLIP_HOME:-$HOME/.paperclip}
INST="$HOME_DIR/instances/$INSTANCE"
SHIM="$HOME/.local/bin/paperclipai"
ADAPTER_DIR="$HOME/.local/share/forgectl-paperclip/adapter-herdr"
OPCONF="$HOME/.config/forgectl/paperclip-$INSTANCE.json"
PKG="$(cd "$(dirname "$(realpath "${BASH_SOURCE[0]}")")/../packages/adapter-herdr" && pwd -P)"

main() {
  local repos="" reviewer="" dry=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --repos) [[ $# -ge 2 ]] || usage; repos=$2; shift ;;
      --reviewer) [[ $# -ge 2 ]] || usage; reviewer=$2; shift ;;
      --dry-run) dry=1 ;;
      -h|--help|help) usage 0 ;;
      *) usage ;;
    esac
    shift
  done
  [[ -n "$repos" ]] || usage
  local r; IFS=: read -r -a rs <<<"$repos"
  for r in "${rs[@]}"; do [[ "$r" == /* ]] || { echo "--repos entries must be absolute paths; got $r" >&2; exit 2; }; done
  [[ -z "$reviewer" || "$reviewer" =~ ^[A-Za-z0-9_-]{1,64}$ ]] || { echo "reviewer id must be [A-Za-z0-9_-]" >&2; exit 2; }
  local fc hd; { fc=$(command -v forgectl) && hd=$(command -v herdr); } || { echo "forgectl and herdr must be on PATH" >&2; exit 1; }
  step() { echo "== $*"; }
  run() { if ((dry)); then printf 'would: %s\n' "$*"; else "$@"; fi; }

  step "1/6 Paperclip CLI $VERSION at $SHIM"  # the shim prints "<version> (managed npm pinned; ...)"
  if [[ -x "$SHIM" && "$("$SHIM" --version 2>/dev/null)" == "$VERSION "* ]]; then echo "already pinned"
  else run pnpm dlx "paperclipai@$VERSION" install --version "$VERSION"; fi

  step "2/6 instance $INSTANCE (loopback, ports $PORT/$PG_PORT)"
  if [[ -f "$INST/config.json" ]]; then echo "already onboarded"
  elif ((dry)); then echo "would: onboard -y --bind loopback, stop it once healthy, pin ports"
  else onboard; fi
  if ((!dry)); then
    local tmp; tmp=$(mktemp)
    jq --argjson p "$PORT" --argjson g "$PG_PORT" '.server.port = $p | .server.bind = "loopback" | .server.host = "127.0.0.1" | .database.embeddedPostgresPort = $g' "$INST/config.json" > "$tmp"
    install -m 0600 "$tmp" "$INST/config.json"; rm -f "$tmp"
    env_add PAPERCLIP_AGENT_JWT_TTL_SECONDS 7200
    # The adapter registry is shared by every instance; only this one's env names the operator file.
    env_add FORGECTL_PAPERCLIP_CONFIG "$OPCONF"
  fi

  step "3/6 adapter bundle -> $ADAPTER_DIR"
  run pnpm --dir "$PKG" build
  run mkdir -p "$ADAPTER_DIR/dist"
  run install -m 0644 "$PKG/package.json" "$ADAPTER_DIR/package.json"
  run install -m 0644 "$PKG/dist/index.js" "$ADAPTER_DIR/dist/index.js"
  step "4/6 register herdr_forgectl in $HOME_DIR/adapter-plugins.json"
  local reg="$HOME_DIR/adapter-plugins.json" cur='[]' rt
  if ((!dry)); then
    [[ -f "$reg" ]] && cur=$(<"$reg"); rt=$(mktemp "$HOME_DIR/.adapter-plugins.XXXXXX")
    jq --arg d "$ADAPTER_DIR" --arg t "$(date -u +%FT%TZ)" '[.[] | select(.type != "herdr_forgectl")] + [{packageName: "@forgectl/paperclip-adapter-herdr", localPath: $d, type: "herdr_forgectl", installedAt: $t, version: "0.0.0"}]' <<<"$cur" > "$rt"
    chmod 0600 "$rt"; mv "$rt" "$reg"
  else echo "would upsert the herdr_forgectl entry"; fi
  step "5/6 operator settings $OPCONF"
  if [[ -f "$OPCONF" ]]; then echo "exists; edit it by hand to change settings"
  elif ((dry)); then echo "would write repos=$repos forgectl=$fc api=http://127.0.0.1:$PORT"
  else
    (umask 077; mkdir -p "$(dirname "$OPCONF")"; jq -n --arg r "$repos" --arg f "$fc" --arg a "http://127.0.0.1:$PORT" --arg u "$reviewer" \
      --arg p "$(dirname "$hd"):$(dirname "$fc"):/usr/bin:/bin:/usr/sbin:/sbin" \
      '{FORGECTL_PAPERCLIP_REPOS: $r, FORGECTL_PAPERCLIP_FORGECTL: $f, FORGECTL_PAPERCLIP_HERDR_SESSION: "paperclip",
        FORGECTL_PAPERCLIP_API_URL: $a, FORGECTL_PAPERCLIP_PATH: $p} + (if $u == "" then {} else {FORGECTL_PAPERCLIP_REVIEWER_USER_ID: $u} end)' > "$OPCONF")
  fi

  step "6/6 service"
  run "$SHIM" service install --instance "$INSTANCE"
  echo "UI and API: http://127.0.0.1:$PORT  (herdr session 'paperclip' must be running: herdr --session paperclip server)"
}

# Append KEY=VALUE to the instance .env once, starting a new line if the file lacks a final newline.
env_add() { command grep -q "^$1=" "$INST/.env" && return 0; [[ -z "$(tail -c1 "$INST/.env")" ]] || echo >> "$INST/.env"; printf '%s=%s\n' "$1" "$2" >> "$INST/.env"; }
# First start creates the database, secrets and .env; stop it once healthy so the service owns later starts.
onboard() {
  local log; log=$(mktemp)
  PAPERCLIP_HOME="$HOME_DIR" PAPERCLIP_INSTANCE_ID="$INSTANCE" "$SHIM" onboard -y --bind loopback --no-install-service > "$log" 2>&1 &
  local pid=$! i port
  for ((i = 0; i < 120; i++)); do
    sleep 1
    port=$(jq -r '.port // empty' "$INST/runtime-info.json" 2>/dev/null || true)
    [[ -n "$port" ]] && command curl -fsS -o /dev/null "http://127.0.0.1:$port/api/health" && break
    kill -0 "$pid" 2>/dev/null || { echo "onboard exited early; log: $log" >&2; exit 1; }
  done
  kill -INT "$pid"; wait "$pid" || true
  ((i < 120)) || { echo "onboard never became healthy in 120 s; log: $log" >&2; exit 1; }
  rm -f "$log"
}
usage() { sed -n '5,6p' "$0" >&2; exit "${1:-2}"; }

main "$@"
