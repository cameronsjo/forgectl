#!/usr/bin/env node

import assert from "node:assert/strict";
import { chmod, lstat, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  assertAllowedChanges,
  assertDisjointPaths,
  assertMessageIdentity,
  assertRecovery,
  digest,
  goalSandbox,
  parseGoalArgs,
  readGoalStatus,
  validateGoal,
} from "./run-project-goal.mjs";
import { runCommand as runCrossHarnessCommand } from "./verify-cross-harness-loop.mjs";

const expectThrow = (callback, pattern) => {
  assert.throws(callback, pattern);
};

const expectReject = async (callback, pattern) => {
  await assert.rejects(callback, pattern);
};

const waitForProcessGone = async (pid, group = false) => {
  const deadline = Date.now() + 2_000;
  while (Date.now() < deadline) {
    try {
      process.kill(group ? -pid : pid, 0);
    } catch (error) {
      if (error.code === "ESRCH") return;
      throw error;
    }
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 25));
  }
  assert.fail(`owned ${group ? "process group" : "process"} ${pid} remains after close`);
};

let commandChild;
let reportedChildPid;
let terminationRequested = false;
const commandEvidence = {
  add() {},
  commandStarted(child) {
    commandChild = child;
    child.stdout.on("data", (chunk) => {
      const match = String(chunk).match(/child:(\d+)/u);
      if (!match || terminationRequested) return;
      reportedChildPid = Number(match[1]);
      terminationRequested = true;
      process.kill(-child.pid, "SIGTERM");
    });
  },
};
const commandResult = await runCrossHarnessCommand(commandEvidence, "/bin/sh", ["-c", "sleep 2 & child=$!; printf 'child:%s\\n' \"$child\"; wait"], { timeout: 5_000 });
assert(commandChild && Number.isInteger(commandChild.pid) && commandChild.pid > 1, "runCommand did not retain its child process");
assert(Number.isInteger(reportedChildPid) && reportedChildPid > 1, "shell did not report a live descendant PID");
assert(commandResult.code !== 0 || commandResult.signal !== null, "group termination did not produce a non-success result");
assert.equal(commandResult.signal, "SIGTERM");
await waitForProcessGone(commandChild.pid, true);
await waitForProcessGone(reportedChildPid);

const validArguments = [
  "--live",
  "--root", "/private/tmp/project-goal-runtime",
  "--worktree", "/private/tmp/project-goal-worktree",
  "--expect-head", "0123456789abcdef0123456789abcdef01234567",
  "--goal-file", "/private/tmp/project-goal.json",
  "--forgectl", "/private/bin/forgectl",
  "--codex", "/private/bin/codex",
  "--codex-model", "gpt-test",
  "--pi", "/private/bin/pi",
  "--pi-provider", "openai-codex",
  "--pi-model", "pi-test",
  "--pi-extension", "/private/extensions/one.mjs",
  "--websocket-module", "/private/modules/ws.mjs",
  "--apply-patch", "/private/bin/apply_patch",
];

assert.deepEqual(parseGoalArgs(["--help"]), { help: true });
assert.deepEqual(parseGoalArgs(["--status", "/private/tmp/project-goal-runtime"]), { status: "/private/tmp/project-goal-runtime" });
assert.deepEqual(parseGoalArgs(validArguments), {
  live: true,
  root: "/private/tmp/project-goal-runtime",
  worktree: "/private/tmp/project-goal-worktree",
  "expect-head": "0123456789abcdef0123456789abcdef01234567",
  "goal-file": "/private/tmp/project-goal.json",
  forgectl: "/private/bin/forgectl",
  codex: "/private/bin/codex",
  "codex-model": "gpt-test",
  pi: "/private/bin/pi",
  "pi-provider": "openai-codex",
  "pi-model": "pi-test",
  piExtensions: ["/private/extensions/one.mjs"],
  "websocket-module": "/private/modules/ws.mjs",
  "apply-patch": "/private/bin/apply_patch",
});
expectThrow(() => parseGoalArgs(validArguments.slice(1)), /explicit --live/);
expectThrow(() => parseGoalArgs(["--live"]), /--root is required/);
expectThrow(() => parseGoalArgs([...validArguments, "--unknown", "value"]), /unknown argument/);
expectThrow(() => parseGoalArgs([...validArguments, "--root", "/private/tmp/duplicate"]), /duplicate --root/);
expectThrow(() => parseGoalArgs([...validArguments, "--pi-extension", "/private/extensions/one.mjs"]), /duplicate Pi extension/);
expectThrow(() => parseGoalArgs(validArguments.map((value) => value.replace("openai-codex", "other-provider"))), /OpenAI provider/);
expectThrow(() => parseGoalArgs(validArguments.map((value) => value.replace("/private/extensions/one.mjs", "relative-extension.mjs"))), /absolute Pi safety/);
expectThrow(() => parseGoalArgs([...validArguments, "--live"]), /duplicate --live/);
expectThrow(() => parseGoalArgs(["--status", "relative-root"]), /absolute root/);
expectThrow(() => parseGoalArgs([...validArguments.slice(0, -1), "--apply-patch", ""]), /needs a value/);
expectThrow(() => parseGoalArgs([...validArguments.slice(0, -1), "bad\npath"]), /control characters/);

const validGoal = {
  version: 1,
  title: "Keyboard help",
  goal: "Make the Agents overview keyboard controls discoverable.",
  allowed_paths: [
    "internal/tui/project_agents_unix.go",
    "internal/tui/project_agents_unix_test.go",
    "docs/commands/surface-project.md",
  ],
  acceptance: ["Help is discoverable in both footer layouts."],
};

assert.equal(validateGoal(validGoal), validGoal);
for (const malformed of [
  null,
  [],
  { ...validGoal, version: 2 },
  { ...validGoal, extra: true },
  { ...validGoal, title: "" },
  { ...validGoal, goal: "   " },
  { ...validGoal, title: "line\nbreak" },
  { ...validGoal, title: "tab\tbreak" },
  { ...validGoal, title: "x".repeat(129) },
  { ...validGoal, goal: "unsafe\u0001goal" },
  { ...validGoal, allowed_paths: [] },
  { ...validGoal, allowed_paths: [...validGoal.allowed_paths, ...validGoal.allowed_paths] },
  { ...validGoal, allowed_paths: ["/absolute/path"] },
  { ...validGoal, allowed_paths: ["../outside"] },
  { ...validGoal, allowed_paths: ["nested/../outside"] },
  { ...validGoal, allowed_paths: [".git/config"] },
  { ...validGoal, allowed_paths: [".claude/settings.json"] },
  { ...validGoal, allowed_paths: ["unsafe path"] },
  { ...validGoal, acceptance: [] },
  { ...validGoal, acceptance: ["x".repeat(4097)] },
  { ...validGoal, acceptance: Array.from({ length: 17 }, () => "criterion") },
  { ...validGoal, acceptance: ["   "] },
  { ...validGoal, acceptance: [42] },
  { ...validGoal, goal: "x".repeat(16 * 1024) },
]) expectThrow(() => validateGoal(malformed));

assert.doesNotThrow(() => assertDisjointPaths("/private/tmp/runtime", "/private/tmp/runtime-alt"));
assert.doesNotThrow(() => assertDisjointPaths("/private/tmp/runtime-alt", "/private/tmp/runtime"));
expectThrow(() => assertDisjointPaths("/private/tmp/runtime", "/private/tmp/runtime/child"), /disjoint/);
expectThrow(() => assertDisjointPaths("/private/tmp/runtime/child", "/private/tmp/runtime"), /disjoint/);
expectThrow(() => assertDisjointPaths("/private/tmp/runtime", "/private/tmp/runtime"), /disjoint/);
assert.deepEqual(goalSandbox("/private/tmp/project-goal-worktree", "/private/tmp/project-goal-ledger"), {
  type: "workspaceWrite",
  networkAccess: false,
  writableRoots: ["/private/tmp/project-goal-worktree", "/private/tmp/project-goal-ledger"],
  excludeTmpdirEnvVar: true,
  excludeSlashTmp: true,
});
expectThrow(() => goalSandbox("relative-worktree", "/private/tmp/project-goal-ledger"), /absolute/);
expectThrow(() => goalSandbox("/private/tmp/project-goal-worktree", "relative-ledger"), /absolute/);

assert.doesNotThrow(() => assertAllowedChanges(["docs/commands/surface-project.md"], [], validGoal.allowed_paths));
expectThrow(() => assertAllowedChanges([], [], validGoal.allowed_paths), /no patch/);
expectThrow(() => assertAllowedChanges(["internal/other.go"], [], validGoal.allowed_paths), /exceeds allowed scope/);
expectThrow(() => assertAllowedChanges(validGoal.allowed_paths, ["scratch.txt"], validGoal.allowed_paths), /untracked files/);

const sender = { name: "implementation", run_id: "implementation-run" };
const recipient = { name: "coordinator", run_id: "coordinator-run" };
const message = { id: "message-1", from: sender.name, to: recipient.name, body: "RESULT goal-1", status: "sent" };
const envelope = { ...message, from_run_id: sender.run_id, to_run_id: recipient.run_id };
assert.equal(assertMessageIdentity(message, [envelope], sender, recipient), envelope);
for (const invalid of [
  { message: { ...message, status: "queued" }, envelopes: [envelope] },
  { message: { ...message, status: "unconfirmed" }, envelopes: [{ ...envelope, status: "unconfirmed" }] },
  { message, envelopes: [{ ...envelope, from_run_id: "retired-run" }] },
  { message, envelopes: [{ ...envelope, to_run_id: "replacement-run" }] },
  { message: { ...message, body: "RESULT another-goal" }, envelopes: [envelope] },
  { message, envelopes: [envelope, { ...envelope }] },
  { message, envelopes: [{ ...envelope, id: "other-message" }] },
]) expectThrow(() => assertMessageIdentity(invalid.message, invalid.envelopes, sender, recipient));

const checkpoint = {
  task_id: "goal-1",
  goal_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  project_id: "project-1",
  base_revision: "0123456789abcdef0123456789abcdef01234567",
  patch_sha256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
  implementation_message_id: "implementation-message",
  review_message_id: "review-message",
  changed_files: ["internal/tui/project_agents_unix.go", "docs/commands/surface-project.md"],
  bindings: {
    coordinator: { run_id: "coordinator-run", native_session_id: "coordinator-native" },
    implementation: { run_id: "implementation-run", native_session_id: "implementation-native" },
    reviewer: { run_id: "reviewer-run", native_session_id: "reviewer-native" },
  },
};
const recovery = { ...checkpoint, needs_operator: "independent verification" };
assert.doesNotThrow(() => assertRecovery(recovery, checkpoint));
for (const field of ["task_id", "goal_sha256", "project_id", "base_revision", "patch_sha256", "implementation_message_id", "review_message_id"]) {
  expectThrow(() => assertRecovery({ ...recovery, [field]: `wrong-${field}` }, checkpoint), new RegExp(`recovery ${field}`));
  const omittedRecovery = { ...recovery };
  delete omittedRecovery[field];
  expectThrow(() => assertRecovery(omittedRecovery, checkpoint), new RegExp(`recovery ${field}`));
  const omittedCheckpoint = { ...checkpoint };
  delete omittedCheckpoint[field];
  expectThrow(() => assertRecovery(recovery, omittedCheckpoint), new RegExp(`checkpoint ${field}`));
}
expectThrow(() => assertRecovery({ ...recovery, changed_files: ["unrelated.go"] }, checkpoint), /changed paths/);
expectThrow(() => assertRecovery({ ...recovery, bindings: { ...recovery.bindings, reviewer: { run_id: "other-run", native_session_id: "reviewer-native" } } }, checkpoint), /role bindings/);
const omittedChangedFiles = { ...recovery };
delete omittedChangedFiles.changed_files;
expectThrow(() => assertRecovery(omittedChangedFiles, checkpoint), /changed paths/);
const omittedBindings = { ...recovery };
delete omittedBindings.bindings;
expectThrow(() => assertRecovery(omittedBindings, checkpoint), /role bindings/);
expectThrow(() => assertRecovery({ ...recovery, changed_files: undefined }, checkpoint), /changed paths/);
expectThrow(() => assertRecovery({ ...recovery, bindings: undefined }, checkpoint), /role bindings/);
expectThrow(() => assertRecovery({ ...recovery, needs_operator: "resume automatically" }, checkpoint), /independent verification/);

assert.equal(digest("hello"), "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824");
assert.equal(digest(Buffer.from("hello")), digest("hello"));

const statusRoot = await mkdtemp(join(tmpdir(), "fctl-project-goal-status-"));
try {
  const state = { version: 1, task_id: "goal-1", phase: "ready_for_verification" };
  const statePath = join(statusRoot, "state.json");
  await writeFile(statePath, `${JSON.stringify(state)}\n`, { mode: 0o600 });
  await chmod(statePath, 0o600);
  const before = await readFile(statePath);
  const result = await readGoalStatus(statusRoot);
  assert.deepEqual(result, state);
  assert.deepEqual(await readFile(statePath), before);
  assert.deepEqual((await lstat(statePath)).mode & 0o777, 0o600);

  await rm(statePath);
  await expectReject(() => readGoalStatus(statusRoot), /ENOENT/);

  await writeFile(statePath, "not-json\n", { mode: 0o600 });
  await chmod(statePath, 0o600);
  await expectReject(() => readGoalStatus(statusRoot), /Unexpected token/);

  await writeFile(statePath, JSON.stringify({ ...state, phase: "unknown" }), { mode: 0o600 });
  await chmod(statePath, 0o600);
  await expectReject(() => readGoalStatus(statusRoot), /unsupported task state/);

  await writeFile(statePath, JSON.stringify({ ...state, phase: "unconfirmed" }), { mode: 0o600 });
  await chmod(statePath, 0o600);
  await expectReject(() => readGoalStatus(statusRoot), /unsupported task state/);

  await writeFile(statePath, JSON.stringify(state), { mode: 0o644 });
  await chmod(statePath, 0o644);
  await expectReject(() => readGoalStatus(statusRoot), /owner-private/);

  const privateTarget = join(statusRoot, "private-state.json");
  await writeFile(privateTarget, JSON.stringify(state), { mode: 0o600 });
  await chmod(privateTarget, 0o600);
  await rm(statePath);
  await symlink(privateTarget, statePath);
  await expectReject(() => readGoalStatus(statusRoot), /bounded, unlinked regular file/);
} finally {
  await rm(statusRoot, { recursive: true, force: true });
}

process.stdout.write("project goal contract tests passed\n");
