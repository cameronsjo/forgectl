#!/usr/bin/env bash
# Stdio smoke for `forgectl tasks mcp`, run against a LIVE Vikunja instance.
#
# It drives the real transport — three JSON-RPC frames on stdin, responses on
# stdout — and asserts three things in a deliberate order:
#
#   1. initialize returns a result                  (the transport works)
#   2. tools/list names all six tools               (registration works)
#   3. tools/call list_projects returns projects    (the credential is ALIVE)
#   4. tools/call create_task returns a tool error  (the credential is READ-ONLY)
#
# Step 4 is evidence only because step 3 passed. An out-of-scope write and a
# revoked token both answer 401, so a refusal on its own proves nothing about
# scope — the passing read is what makes the refusal mean "denied" rather than
# "dead". Reversing that order would turn this script into a check that cannot
# go red for the reason it claims to.
#
# Usage:
#   bash scripts/mcp-stdio-smoke.sh [--keychain-service NAME] [--host HOST]
#
# Exit codes: 0 all assertions held; 1 an assertion failed; 2 the binary could
# not be built or the instance was unreachable.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEYCHAIN_SERVICE="vikunja-readonly"
HOST="tasks.sjo.lol"
EXPECTED_PROJECTS=3

while [ $# -gt 0 ]; do
	case "$1" in
	--keychain-service)
		KEYCHAIN_SERVICE="$2"
		shift 2
		;;
	--host)
		HOST="$2"
		shift 2
		;;
	--expect-projects)
		EXPECTED_PROJECTS="$2"
		shift 2
		;;
	*)
		echo "unknown argument: $1" >&2
		exit 2
		;;
	esac
done

# NO_COLOR is honoured, and so is a non-TTY stdout: this script's output is
# read by an agent as often as by a person.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	RED=$'\033[31m' GREEN=$'\033[32m' DIM=$'\033[2m' RESET=$'\033[0m'
else
	RED='' GREEN='' DIM='' RESET=''
fi

fail_count=0
step() { printf '%s==>%s %s\n' "$DIM" "$RESET" "$1" >&2; }
pass() { printf '%sPASS%s %s\n' "$GREEN" "$RESET" "$1" >&2; }
fail() {
	printf '%sFAIL%s %s\n' "$RED" "$RESET" "$1" >&2
	fail_count=$((fail_count + 1))
}

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Step 4 files a task and asserts a 401. Pointed at a WRITE credential the
# assertion goes red AND a real row lands on the shared board with nobody
# cleaning it up — the writer profile cannot delete it, so it takes a human in
# the UI. Refuse up front rather than discover it from the wreckage.
case "$KEYCHAIN_SERVICE" in
*readonly* | *read-only* | *ro) ;;
*)
	echo "${RED}REFUSING${RESET} keychain service '$KEYCHAIN_SERVICE': this smoke test files a task and asserts it is REFUSED." >&2
	echo "Against a write credential the create SUCCEEDS — the assertion fails and a real task is left on the board." >&2
	echo "Re-run with a read-only entry (a service name containing 'readonly'), or use the container transport for write testing." >&2
	exit 2
	;;
esac

step "building forgectl"
if ! go build -o "$WORKDIR/forgectl" "$REPO_ROOT" 2>"$WORKDIR/build.log"; then
	cat "$WORKDIR/build.log" >&2
	echo "VERDICT: FAIL — could not build forgectl" >&2
	exit 2
fi

step "driving the stdio transport against $HOST (keychain service: $KEYCHAIN_SERVICE)"
{
	echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"stdio-smoke","version":"0"}}}'
	echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
	echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
	echo '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}'
	echo '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_task","arguments":{"project_id":1,"title":"stdio-smoke-DO-NOT-KEEP"}}}'
	# The server exits when stdin closes; the sleep gives it time to answer
	# the last call before that happens.
	sleep 5
} | "$WORKDIR/forgectl" tasks mcp --host "$HOST" --keychain-service "$KEYCHAIN_SERVICE" \
	>"$WORKDIR/out.jsonl" 2>"$WORKDIR/err.log"
server_rc=$?

if [ ! -s "$WORKDIR/out.jsonl" ]; then
	echo "--- stderr ---" >&2
	cat "$WORKDIR/err.log" >&2
	echo "VERDICT: FAIL — the server produced no output (exit $server_rc); the instance is probably unreachable from here" >&2
	exit 2
fi

# One python pass over the frames, so each assertion reads the same parsed
# data. Parsing the same file five times with grep is how two assertions end
# up disagreeing about what the server said.
python3 - "$WORKDIR/out.jsonl" "$EXPECTED_PROJECTS" >"$WORKDIR/verdicts.txt" <<'PY'
import json, sys

frames = {}
with open(sys.argv[1]) as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        try:
            frame = json.loads(line)
        except json.JSONDecodeError:
            continue
        if "id" in frame:
            frames[frame["id"]] = frame

expected_projects = int(sys.argv[2])
out = []

def verdict(ok, label):
    out.append(("PASS" if ok else "FAIL") + " " + label)

init = frames.get(1, {})
verdict("result" in init, "initialize returned a result")

tools_frame = frames.get(2, {})
names = sorted(t["name"] for t in tools_frame.get("result", {}).get("tools", []))
want = ["add_comment", "create_task", "get_task", "list_projects", "list_tasks", "ready_tasks"]
verdict(names == want, "tools/list names the six tools (got: %s)" % ", ".join(names))

projects = frames.get(3, {})
text = "".join(c.get("text", "") for c in projects.get("result", {}).get("content", []))
is_err = projects.get("result", {}).get("isError", False)
count = 0
if text:
    head = text.split(" project(s)")[0].strip()
    count = int(head) if head.isdigit() else 0
verdict(not is_err and count >= expected_projects,
        "list_projects returned %d project(s), want at least %d — THIS is what proves the credential is alive"
        % (count, expected_projects))
verdict("<board-text-" in text, "list_projects fenced its board text")

create = frames.get(4, {})
create_text = "".join(c.get("text", "") for c in create.get("result", {}).get("content", []))
create_is_err = create.get("result", {}).get("isError", False)
# Match the refusal the client actually produces, not a word it might have
# used. The sentinel renders as "rejected the credential" and carries the 401;
# an earlier version of this assertion looked for "unauthorized", which the
# message never contains — so it reported a red against correct behaviour.
verdict(create_is_err and "401" in create_text and "rejected the credential" in create_text,
        "create_task was refused 401 under the read-only entry — meaningful ONLY because the read above passed (got: %s)"
        % create_text.strip()[:140])

print("\n".join(out))
PY

while IFS= read -r line; do
	case "$line" in
	PASS*) pass "${line#PASS }" ;;
	FAIL*) fail "${line#FAIL }" ;;
	esac
done <"$WORKDIR/verdicts.txt"

if [ "$fail_count" -eq 0 ]; then
	echo "VERDICT: PASS — stdio transport, six tools, credential alive and read-only" >&2
	exit 0
fi
echo "VERDICT: FAIL — $fail_count assertion(s) failed" >&2
exit 1
