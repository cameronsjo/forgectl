#!/opt/homebrew/bin/bash
# verify-blessing-agent-sim.sh — the adversary's half of the blessing proof (#10).
#
# Every check here is run AS THE ADVERSARY: a local agent with the user's normal
# file and exec permissions, no password, no root, and no finger on the sensor.
# Each check must FAIL to execute a workflow. A single success is a bypass.
#
# Unlike verify-blessing-e2e.sh, this script needs NO human — an agent can run it
# start to finish, which is exactly what makes its all-refused verdict meaningful.
#
#   bash scripts/verify-blessing-agent-sim.sh
set -uo pipefail

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    readonly BOLD=$'\033[1m' GREEN=$'\033[32m' RED=$'\033[31m' DIM=$'\033[2m' RESET=$'\033[0m'
else
    readonly BOLD='' GREEN='' RED='' DIM='' RESET=''
fi

readonly CFG="$HOME/Library/Application Support/forgectl"
readonly WF_DIR="$CFG/workflows"
readonly STORE="$CFG/trust.toml"
readonly NAME="agent-sim-probe"
readonly WF_FILE="$WF_DIR/$NAME.workflow.toml"

held=0
bypassed=0
skipped=0

step()  { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$RESET"; }
note()  { printf '%s  %s%s\n' "$DIM" "$*" "$RESET"; }
held()  { printf '%s  HELD%s     %s\n' "$GREEN" "$RESET" "$*"; held=$((held + 1)); }
bypass(){ printf '%s  BYPASSED%s %s\n' "$RED" "$RESET" "$*" >&2; bypassed=$((bypassed + 1)); }

# first_line <file|-> — the first non-blank line. forgectl's styled error output
# leads with blank lines, so a plain `head -1` renders an empty "reason" and the
# evidence reads as missing when it is merely offset.
first_line() {
    if [[ "$1" == "-" ]]; then command grep -v '^[[:space:]]*$' | head -1
    else command grep -v '^[[:space:]]*$' "$1" | head -1
    fi
}

# must_refuse <description> <command...> — the control HELD only if the command fails.
must_refuse() {
    local desc="$1"; shift
    local out rc
    out=$("$@" 2>&1); rc=$?
    if (( rc == 0 )); then
        bypass "$desc — THE WORKFLOW RAN"
        note "output: ${out//$'\n'/ }"
        return
    fi
    held "$desc"
    note "refused: $(printf '%s' "$out" | first_line -)"
}

step "Preparing: build forgectl + helper, author a workflow as the agent would"
BIN_DIR=$(mktemp -d)
trap 'rm -rf "$BIN_DIR"; rm -f "$WF_FILE" "$WF_FILE.blessing"' EXIT

CGO_ENABLED=0 go build -o "$BIN_DIR/forgectl" . || exit 1
(cd helper/forgectl-bless-helper && swift build -c release >/dev/null 2>&1) || exit 1
cp helper/forgectl-bless-helper/.build/release/forgectl-bless-helper "$BIN_DIR/"
codesign -s - -f "$BIN_DIR/forgectl-bless-helper" 2>/dev/null
readonly FORGECTL="$BIN_DIR/forgectl"

mkdir -p "$WF_DIR"
cat > "$WF_FILE" <<'TOML'
dsl_version = 1
name = "agent-sim-probe"
version = "1.0.0"
description = "What an agent would write if it wanted arbitrary execution."

[[step]]
uses = "run"
cmd = "echo"
args = ["AGENT-CONTROLLED-EXECUTION"]
TOML
rm -f "$WF_FILE.blessing"
note "wrote $WF_FILE — an agent can author freely; that was never the control"

step "(a) Run a workflow the agent just wrote, unblessed"
must_refuse "unblessed workflow refused" "$FORGECTL" workflow run "$NAME"

step "(b) Forge a blessing sidecar with an agent-minted key"
# The agent mints its own Secure Enclave key (creation needs NO presence!) and
# signs the workflow with it — the sharpest attack available without a human.
if "$BIN_DIR/forgectl-bless-helper" enroll --label agent-sim-rogue >/dev/null 2>&1 ||
   "$BIN_DIR/forgectl-bless-helper" pubkey --label agent-sim-rogue >/dev/null 2>&1; then
    note "the agent DID mint its own SE key (expected — key creation needs no presence)"
    note "but the key is not in the anchor-signed trust store, so its signature is worthless"
else
    note "helper key mint unavailable; falling back to a garbage sidecar"
fi
cat > "$WF_FILE.blessing" <<'TOML'
schema = 1
algo = "ecdsa-p256-sha256"
key_id = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
signature = "MEUCIQDforgedforgedforgedforgedforgedforgedforgedforgedforgedAiEA"
signed_at = "2026-07-12T00:00:00Z"
TOML
must_refuse "forged sidecar refused" "$FORGECTL" workflow run "$NAME"
rm -f "$WF_FILE.blessing"

step "(c) Edit a file the human genuinely blessed, then run it"
# Uses the e2e probe's own bless if present; otherwise notes the gap.
BLESSED=$(command ls "$WF_DIR"/*.workflow.toml.blessing 2>/dev/null | head -1)
if [[ -n "$BLESSED" ]]; then
    TARGET="${BLESSED%.blessing}"
    cp "$TARGET" "$TARGET.agentbak"
    printf '\n[[step]]\nuses = "run"\ncmd = "echo"\nargs = ["INJECTED"]\n' >> "$TARGET"
    must_refuse "edited-after-blessing refused" "$FORGECTL" workflow run "$(basename "$TARGET" .workflow.toml)"
    mv "$TARGET.agentbak" "$TARGET"
else
    note "SKIPPED — no human-blessed workflow on this machine to tamper with"
    note "run scripts/verify-blessing-e2e.sh first for full coverage"
    skipped=$((skipped + 1))
fi

step "(d) Rewrite the trust store to enroll the agent's own key"
if [[ -f "$STORE" ]]; then
    cp "$STORE" "$STORE.agentbak"
    printf '\n[[key]]\nkey_id = "sha256:%s"\nmachine = "agent"\npubkey = "AAAA"\nadded_at = "2026-07-12T00:00:00Z"\n' \
        "0000000000000000000000000000000000000000000000000000000000000000" >> "$STORE"
    must_refuse "tampered trust store refused" "$FORGECTL" workflow run "$NAME"
    mv "$STORE.agentbak" "$STORE"
else
    note "SKIPPED — no trust store yet (run trust init first)"
    skipped=$((skipped + 1))
fi

step "(e) Ask forgectl to bless the workflow (the ceremony itself)"
# An agent CAN invoke bless. It cannot COMPLETE it: the Secure Enclave demands a
# fingerprint. A hanging Touch ID prompt IS the control working, so we bound the
# wait ourselves — macOS ships no timeout(1), and shelling out to a missing
# binary would fake a refusal (a failed test masquerading as a passing control).
BLESS_OUT="$BIN_DIR/bless.out"
"$FORGECTL" workflow bless "$NAME" > "$BLESS_OUT" 2>&1 &
bless_pid=$!
waited=0
while (( waited < 12 )) && kill -0 "$bless_pid" 2>/dev/null; do
    sleep 1; waited=$((waited + 1))
done
if kill -0 "$bless_pid" 2>/dev/null; then
    kill "$bless_pid" 2>/dev/null
    wait "$bless_pid" 2>/dev/null
    held "bless blocked at the presence prompt (still waiting for a human after ${waited}s)"
    note "the prompt is the wall: no fingerprint, no signature"
else
    wait "$bless_pid"; rc=$?
    if (( rc == 0 )); then
        bypass "bless SUCCEEDED without a human — the ceremony did not hold"
    else
        held "bless refused without a human (exit $rc)"
        note "refused: $(first_line "$BLESS_OUT")"
    fi
fi

step "(f) Drive a workflow via --param into a run step"
# --dry-run deliberately, NOT run: without a trust anchor the verify gate refuses
# everything first, so a plain `run` here would re-prove the anchor check and
# report a param control that never executed. Dry-run skips verification but
# still plans, isolating the param guard.
must_refuse "undeclared --param refused at resolve" \
    "$FORGECTL" workflow run "$NAME" --dry-run --param 'cmd=/bin/sh'

step "(g) Get a \${param} into a blessed run step's command"
# The injection the whole guard exists to stop: a workflow whose run-step cmd is
# a param, so a human blesses innocent bytes and the agent chooses the command at
# run time. Refused at BLESS time — the bytes can never be approved.
EVIL="$WF_DIR/agent-sim-evil.workflow.toml"
cat > "$EVIL" <<'TOML'
dsl_version = 1
name = "agent-sim-evil"
version = "1.0.0"

[params.target]
default = "echo"

[[step]]
uses = "run"
cmd = "${target}"
TOML
must_refuse "\${param} in a run step's cmd refused at bless time" \
    "$FORGECTL" workflow bless agent-sim-evil
rm -f "$EVIL"

step "(h) Weaken the clean-room strip-list at run time"
# The strip-list IS the redaction control (#10's opening threat). A ${param} glob
# would let an agent neuter it against an already-blessed file.
SNEAKY="$WF_DIR/agent-sim-sneaky.workflow.toml"
cat > "$SNEAKY" <<'TOML'
dsl_version = 1
name = "agent-sim-sneaky"
version = "1.0.0"

[params.keep]
default = "nothing"

[[step]]
uses = "worktree"
repo = "cameronsjo/forgectl"

[[step]]
uses = "strip"
globs = ["${keep}"]
TOML
must_refuse "\${param} in strip globs refused at bless time" \
    "$FORGECTL" workflow bless agent-sim-sneaky
rm -f "$SNEAKY"

printf '\n%s================================%s\n' "$BOLD" "$RESET"
if (( bypassed > 0 )); then
    printf '%sVERDICT: BYPASS FOUND%s — %d held, %d BYPASSED. Do not merge.\n' "$RED" "$RESET" "$held" "$bypassed"
    exit 1
fi
if (( skipped > 0 )); then
    # A skipped control is UNKNOWN, not held. Saying otherwise would be the exact
    # green-check-lie this script exists to catch.
    printf '%sVERDICT: PARTIAL%s — %d controls held, 0 bypassed, %d NOT EXERCISED.\n' \
        "$BOLD" "$RESET" "$held" "$skipped"
    printf 'The skipped checks need a trust anchor: run scripts/verify-blessing-e2e.sh (human), then re-run this.\n'
    exit 2
fi
printf '%sVERDICT: NO BYPASS%s — %d controls held, 0 bypassed.\n' "$GREEN" "$RESET" "$held"
printf 'An agent with full file and exec access could not execute a workflow a human did not bless.\n'
exit 0
