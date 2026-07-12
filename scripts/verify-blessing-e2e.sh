#!/opt/homebrew/bin/bash
# verify-blessing-e2e.sh — live end-to-end proof of workflow blessing (#10).
#
# Requires a HUMAN: Touch ID prompts (the blessing ceremony) and one sudo
# password (the root-owned trust anchor). That is the entire point — an agent
# cannot complete this script, and every refusal it prints is the control
# working.
#
# Run from the repo root of the feat/workflow-blessing-release worktree:
#   bash scripts/verify-blessing-e2e.sh
#
# Bash 4+ required (macOS /bin/bash is 3.2; the shebang pins Homebrew's).
set -uo pipefail

readonly ANCHOR="/etc/forgectl/trust-anchor.pub"
readonly WF_NAME="bless-e2e-probe"

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    readonly BOLD=$'\033[1m' GREEN=$'\033[32m' RED=$'\033[31m' DIM=$'\033[2m' RESET=$'\033[0m'
else
    readonly BOLD='' GREEN='' RED='' DIM='' RESET=''
fi

pass_count=0
fail_count=0

step()   { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$RESET"; }
pass()   { printf '%s  PASS%s %s\n' "$GREEN" "$RESET" "$*"; pass_count=$((pass_count + 1)); }
fail()   { printf '%s  FAIL%s %s\n' "$RED" "$RESET" "$*" >&2; fail_count=$((fail_count + 1)); }
note()   { printf '%s  %s%s\n' "$DIM" "$*" "$RESET"; }

# expect_refusal <description> <expected-substring> <command...>
# The command MUST fail AND its output must name the expected reason. A command
# that succeeds here is a control that did not hold.
expect_refusal() {
    local desc="$1" want="$2"; shift 2
    local out rc
    out=$("$@" 2>&1); rc=$?
    if (( rc == 0 )); then
        fail "$desc — command SUCCEEDED but should have been refused"
        return
    fi
    if [[ "$out" != *"$want"* ]]; then
        fail "$desc — refused, but the reason did not mention '$want'"
        note "actual: ${out//$'\n'/ }"
        return
    fi
    pass "$desc — refused: ${want}"
}

expect_success() {
    local desc="$1"; shift
    local out rc
    out=$("$@" 2>&1); rc=$?
    if (( rc != 0 )); then
        fail "$desc — command failed"
        note "actual: ${out//$'\n'/ }"
        return
    fi
    pass "$desc"
}

# ---------------------------------------------------------------------------
step "Preparing: build forgectl + the ceremony helper"

BIN_DIR=$(mktemp -d)
trap 'rm -rf "$BIN_DIR"' EXIT

if ! CGO_ENABLED=0 go build -o "$BIN_DIR/forgectl" . ; then
    printf '%sFailed to build forgectl.%s\n' "$RED" "$RESET" >&2
    exit 1
fi

if ! (cd helper/forgectl-bless-helper && swift build -c release >/dev/null 2>&1); then
    printf '%sFailed to build forgectl-bless-helper.%s\n' "$RED" "$RESET" >&2
    exit 1
fi
cp "helper/forgectl-bless-helper/.build/release/forgectl-bless-helper" "$BIN_DIR/"
codesign -s - -f "$BIN_DIR/forgectl-bless-helper" 2>/dev/null

readonly FORGECTL="$BIN_DIR/forgectl"
note "forgectl + helper staged in $BIN_DIR (the helper is discovered as a sibling)"

WF_DIR=$("$FORGECTL" workflow list >/dev/null 2>&1; echo "$HOME/Library/Application Support/forgectl/workflows")
mkdir -p "$WF_DIR"
readonly WF_FILE="$WF_DIR/$WF_NAME.workflow.toml"
readonly SIDECAR="$WF_FILE.blessing"

cat > "$WF_FILE" <<'TOML'
dsl_version = 1
name = "bless-e2e-probe"
version = "1.0.0"
description = "Live blessing probe — prints a line; harmless."

[[step]]
uses = "run"
cmd = "echo"
args = ["blessed-workflow-executed"]
TOML
rm -f "$SIDECAR"
note "authored an unblessed workflow at $WF_FILE"

# ---------------------------------------------------------------------------
step "1. An unblessed workflow is REFUSED (and the message names the fix)"
expect_refusal "run unblessed" "workflow bless" "$FORGECTL" workflow run "$WF_NAME"

step "2. --dry-run is allowed unsigned (it executes nothing)"
expect_success "dry-run unblessed" "$FORGECTL" workflow run "$WF_NAME" --dry-run

step "3. Trust init — mints the key, signs the store, installs the anchor"
if [[ -e "$ANCHOR" ]]; then
    note "anchor already exists at $ANCHOR — skipping init (that refusal is itself correct)"
    expect_refusal "re-init refused while the anchor exists" "already exists" "$FORGECTL" workflow trust init
else
    printf '%s  A Touch ID prompt and a sudo password prompt are expected now.%s\n' "$DIM" "$RESET"
    if ! "$FORGECTL" workflow trust init; then
        fail "trust init did not complete"
        printf '\n%sCannot continue without a trust anchor.%s\n' "$RED" "$RESET" >&2
        exit 1
    fi
    pass "trust init completed"
fi

expect_success "trust list shows the enrolled key" "$FORGECTL" workflow trust list

step "4. Bless the workflow (Touch ID) — then it runs"
printf '%s  A Touch ID prompt is expected now.%s\n' "$DIM" "$RESET"
if ! "$FORGECTL" workflow bless "$WF_NAME"; then
    fail "bless did not complete"
    exit 1
fi
pass "bless wrote $SIDECAR"

expect_success "verify reports blessed and valid" "$FORGECTL" workflow verify "$WF_NAME"
expect_success "run executes the blessed workflow" "$FORGECTL" workflow run "$WF_NAME"

step "5. One edited byte invalidates the blessing"
printf '# tamper\n' >> "$WF_FILE"
expect_refusal "run after a one-byte edit" "does not match" "$FORGECTL" workflow run "$WF_NAME"
expect_refusal "verify after a one-byte edit" "does not match" "$FORGECTL" workflow verify "$WF_NAME"

# Restore the blessed bytes so the file is runnable again (proves the blessing
# tracks CONTENT, not a one-shot approval).
"${SED:-/usr/bin/sed}" -i '' -e '$d' "$WF_FILE"
expect_success "run again once the original bytes are restored" "$FORGECTL" workflow run "$WF_NAME"

step "6. A tampered trust store fails closed"
STORE="$HOME/Library/Application Support/forgectl/trust.toml"
cp "$STORE" "$STORE.bak"
printf '\n# tampered by the probe\n' >> "$STORE"
expect_refusal "run with a tampered trust store" "trust store" "$FORGECTL" workflow run "$WF_NAME"
mv "$STORE.bak" "$STORE"
expect_success "run recovers once the store is restored" "$FORGECTL" workflow run "$WF_NAME"

# ---------------------------------------------------------------------------
step "Cleanup"
rm -f "$WF_FILE" "$SIDECAR"
note "removed the probe workflow (the trust anchor, key, and store are KEPT — they are your real ones)"

printf '\n%s================================%s\n' "$BOLD" "$RESET"
if (( fail_count == 0 )); then
    printf '%sVERDICT: PASS%s — %d checks, 0 failures. Blessing enforces; tampering fails closed.\n' \
        "$GREEN" "$RESET" "$pass_count"
    exit 0
fi
printf '%sVERDICT: FAIL%s — %d passed, %d FAILED. Do not merge until these are understood.\n' \
    "$RED" "$RESET" "$pass_count" "$fail_count"
exit 1
