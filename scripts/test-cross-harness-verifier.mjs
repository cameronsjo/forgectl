#!/usr/bin/env node

import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { assertDuplicateRejected, assertRegistration, assertRoundtripOrder, parseArgs, startAckDroppingProxy, workspaceSandbox } from "./verify-cross-harness-loop.mjs";

const flags = ["--live", "--forgectl", "/test/forgectl", "--root", "/test/root", "--websocket-module", "/test/ws.mjs"];
assert.throws(() => parseArgs(flags), /--codex-model is required/);
const selected = parseArgs([...flags, "--codex-model", "coordinator-model", "--pi-provider", "worker-provider", "--pi-model", "worker-model"]);
assert.equal(selected["codex-model"], "coordinator-model");
assert.equal(selected.websocketModule, "/test/ws.mjs");
assert.equal(selected["pi-provider"], "worker-provider");
assert.equal(selected["pi-model"], "worker-model");
assert.throws(() => parseArgs([...flags, "--codex-model", "coordinator-model", "--pi-provider", "worker-provider"]), /must be supplied together/);
assert.throws(() => parseArgs(flags.slice(1)), /without explicit --live/);

const messages = ["delegate", "clarify", "answer", "complete"];
assert.deepEqual(assertRoundtripOrder(messages, messages), [0, 1, 2, 3]);
assert.deepEqual(assertRoundtripOrder(["other", ...messages], messages), [1, 2, 3, 4]);
assert.throws(() => assertRoundtripOrder(["delegate", "answer", "clarify", "complete"], messages), /causal mailbox order/);
assert.throws(() => assertRoundtripOrder(["delegate", "clarify", "complete", "answer"], messages), /causal mailbox order/);
assert.throws(() => assertRoundtripOrder(messages.slice(0, 3), messages), /causal mailbox order/);
assert.throws(() => assertRoundtripOrder(messages, ["delegate", "clarify", "answer", "answer"]), /causal mailbox order/);
assert.deepEqual(workspaceSandbox("/test/root"), { type: "workspaceWrite", networkAccess: false, writableRoots: ["/test/root"], excludeTmpdirEnvVar: true, excludeSlashTmp: true });

const coordinatorRegistration = {
  ledger: "/test/ledger",
  member: { name: "coordinator", harness: "pi", worktree: "/test/coordinator", coordinator: true, run_id: "run-1", socket: "/test/pi.sock" },
  environment: { FORGECTL_LEDGER: "/test/ledger", FORGECTL_RUN_ID: "run-1", FORGECTL_COORDINATOR: "coordinator", FORGECTL_INBOX: "/test/pi.sock" },
};
assert.equal(assertRegistration(coordinatorRegistration, "coordinator"), coordinatorRegistration);
for (const fields of [
  { FORGECTL_COORDINATOR: undefined },
  { FORGECTL_WORKER: "coordinator" },
  { FORGECTL_DRAIN_WORKER: "worker" },
  { FORGECTL_RUN_ID: "old-run" },
  { FORGECTL_RUN_ID: "" },
  { FORGECTL_INBOX: "/test/wrong.sock" },
]) {
  const broken = { ...coordinatorRegistration, environment: { ...coordinatorRegistration.environment, ...fields } };
  assert.throws(() => assertRegistration(broken, "coordinator"), /unexpected.*registration/);
}
const workerRegistration = {
  ledger: "/test/ledger",
  member: { name: "worker", harness: "pi", worktree: "/test/worker", run_id: "run-2", socket: "/test/worker.sock" },
  environment: { FORGECTL_LEDGER: "/test/ledger", FORGECTL_RUN_ID: "run-2", FORGECTL_WORKER: "worker", FORGECTL_INBOX: "/test/worker.sock" },
};
assert.equal(assertRegistration(workerRegistration, "worker"), workerRegistration);

assert.doesNotThrow(() => assertDuplicateRejected({ code: 1, stderr: "Duplicate message-record ID: fixture-1" }));
for (const result of [
  { code: 0, stderr: "Duplicate message-record ID: fixture-1" },
  { code: null, stderr: "Duplicate message-record ID: fixture-1" },
  { code: 1, stderr: "Cannot read input file" },
  { code: 1, stderr: "" },
]) assert.throws(() => assertDuplicateRejected(result), /duplicate-ID fixture/);

const proxyRoot = await mkdtemp(join(tmpdir(), "fctl-proxy-test-"));
const proxyEvents = [];
try {
  await assert.rejects(startAckDroppingProxy({ add: (...event) => proxyEvents.push(event) }, join(proxyRoot, "missing", "proxy.sock"), join(proxyRoot, "worker.sock")), (error) => error.syscall === "listen" && ["ENOENT", "EPERM", "EACCES"].includes(error.code));
  assert.deepEqual(proxyEvents, []);
} finally {
  await rm(proxyRoot, { recursive: true });
}

process.stdout.write("cross-harness verifier contract tests passed\n");
