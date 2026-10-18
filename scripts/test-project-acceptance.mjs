import assert from "node:assert/strict";
import { assertBoundRun, assertNativeEvidence } from "./project-acceptance.mjs";
import { assertAgentsSnapshot, codexViewSnapshot } from "./project-presentation-acceptance.mjs";

const member = { name: "worker", run_id: "run-1", harness: "pi", thread_id: "native-1", worktree: "/private/task/worker", socket: "/private/task/pi.sock" };
const binding = { member: member.name, run_id: member.run_id, harness: member.harness, native_session_id: member.thread_id, worktree: member.worktree, socket: member.socket };
const bound = { project_id: "project-1", thread: { id: "thread-1", active_run_id: "run-1", runs: [binding] } };
assert.equal(assertBoundRun(bound, "project-1", "thread-1", member), binding);
for (const changed of [{ ...member, run_id: "other-run" }, { ...member, thread_id: "other-native" }, { ...member, harness: "codex" }, { ...member, socket: "/other.sock" }]) assert.throws(() => assertBoundRun(bound, "project-1", "thread-1", changed));
assert.throws(() => assertBoundRun(bound, "other-project", "thread-1", member));
assert.throws(() => assertBoundRun(bound, "project-1", "other-thread", member));
assert.throws(() => assertBoundRun(bound, "project-1", "thread-1", member, [binding]));

const agentsBinding = {
  thread_id: "thread-1",
  current: { run_id: "run-1", native_session_id: "native-1", harness: "codex", source: "codex-turn-history", coverage: "native-history" },
  history: [
    { run_id: "old-run", native_session_id: "old-native", harness: "pi", source: "pi-agent-events", coverage: "current-process" },
    { run_id: "run-1", native_session_id: "native-1", harness: "codex", source: "codex-turn-history", coverage: "native-history" },
  ],
};
const agentsSnapshot = {
  version: 1, project_id: "project-1", observed_at: "2026-10-17T12:00:00.000Z", total_threads: 1,
  rows: [{
    thread_id: "thread-1", run_id: "run-1", harness: "codex", enrollment: "current", coverage: "native-history", spans: [
      { run_id: "old-run", native_session_id: "old-native", source: "pi-agent-events", span: { id: "old-turn", started_at: "2026-10-17T11:58:00.000Z", ended_at: "2026-10-17T11:58:30.000Z", state: "finished" } },
      { run_id: "run-1", native_session_id: "native-1", source: "codex-turn-history", span: { id: "turn-1", started_at: "2026-10-17T11:59:00.000Z", ended_at: "2026-10-17T11:59:30.000Z", state: "finished" } },
    ],
    points: [{ run_id: "old-run", turn_id: "old-point", at: "2026-10-17T11:58:45.000Z", state: "unknown", source: "pi-agent-events" }],
  }],
};
assert.equal(assertAgentsSnapshot(agentsSnapshot, "project-1", [agentsBinding]), agentsSnapshot);
for (const mutation of [
  { partial: true },
  { dropped_threads: 1 },
  { dropped_spans: 1 },
  { rows: [{ ...agentsSnapshot.rows[0], spans: [] }] },
  { rows: [{ ...agentsSnapshot.rows[0], spans: [{ ...agentsSnapshot.rows[0].spans[0], source: "unknown" }] }] },
  { rows: [{ ...agentsSnapshot.rows[0], spans: [{ ...agentsSnapshot.rows[0].spans[0], native_session_id: "other-native" }] }] },
  { rows: [{ ...agentsSnapshot.rows[0], spans: [{ ...agentsSnapshot.rows[0].spans[1], span: { ...agentsSnapshot.rows[0].spans[1].span, ended_at: undefined } }] }] },
  { rows: [{ ...agentsSnapshot.rows[0], partial: true }] },
  { rows: [{ ...agentsSnapshot.rows[0], coverage: "current-process" }] },
  { rows: [{ ...agentsSnapshot.rows[0], points: [{ ...agentsSnapshot.rows[0].points[0], source: "unknown" }] }] },
  { rows: [{ ...agentsSnapshot.rows[0], spans: [agentsSnapshot.rows[0].spans[0], { ...agentsSnapshot.rows[0].spans[1], span: { ...agentsSnapshot.rows[0].spans[1].span, state: "unknown" } }] }] },
]) assert.throws(() => assertAgentsSnapshot({ ...agentsSnapshot, ...mutation }, "project-1", [agentsBinding]));

const report = {
  project_id: "project-1", thread_id: "thread-1", run_id: "run-1", process_liveness: "unknown",
  native: { native_session_id: "native-1", provider: "openai-codex", model: "test-model", turn: { id: "turn-1", status: "completed" } },
  evidence: { thread_id: "thread-1", run_id: "run-1", native_session_id: "native-1", harness: "pi", source: "pi-inbox", turn_id: "turn-1", status: "completed" },
};
const expected = { provider: "openai-codex", model: "test-model", completed: true };
assert.equal(assertNativeEvidence(report, "project-1", "thread-1", binding, expected), report);
for (const changed of [
  { ...report, process_liveness: "running" },
  { ...report, run_id: "replacement" },
  { ...report, evidence: undefined },
  { ...report, native: { ...report.native, native_session_id: "wrong-native" } },
  { ...report, native: { ...report.native, provider: "other-provider" } },
  { ...report, native: { ...report.native, turn: { ...report.native.turn, status: "failed" } } },
  { ...report, evidence: { ...report.evidence, status: "failed" } },
  { ...report, evidence: { ...report.evidence, model: "invented-producer" } },
]) assert.throws(() => assertNativeEvidence(changed, "project-1", "thread-1", binding, expected));

const nativePolicy = {
  thread: { id: binding.native_session_id, cwd: binding.worktree }, cwd: binding.worktree,
  model: "test-model", modelProvider: "openai", approvalsReviewer: "user", approvalPolicy: "never",
  activePermissionProfile: null, runtimeWorkspaceRoots: [binding.worktree],
  sandbox: { type: "workspaceWrite", networkAccess: false, writableRoots: ["/private/task"], excludeSlashTmp: true, excludeTmpdirEnvVar: true },
};
const snapshot = codexViewSnapshot(nativePolicy, binding, "test-model", "/private/task");
assert.equal(snapshot.sandbox, nativePolicy.sandbox);
for (const mutation of [
  { model: "other-model" }, { modelProvider: "other-provider" }, { approvalsReviewer: "auto_review" },
  { approvalPolicy: "unknown" }, { cwd: "/different" }, { runtimeWorkspaceRoots: [] },
  { activePermissionProfile: { id: "custom" } }, { sandbox: { type: "dangerFullAccess" } },
  { sandbox: { ...nativePolicy.sandbox, networkAccess: true } }, { thread: { ...nativePolicy.thread, id: "other" } },
  { sandbox: { ...nativePolicy.sandbox, networkAccess: undefined } }, { sandbox: { ...nativePolicy.sandbox, writableRoots: ["/"] } },
  { sandbox: { ...nativePolicy.sandbox, writableRoots: undefined } }, { runtimeWorkspaceRoots: [binding.worktree, "/outside"] },
]) assert.throws(() => codexViewSnapshot({ ...nativePolicy, ...mutation }, binding, "test-model", "/private/task"));
for (const mutation of [
  { approvalPolicy: "on-request" }, { runtimeWorkspaceRoots: [binding.worktree, "/another"] },
  { sandbox: { ...nativePolicy.sandbox, writableRoots: [] } }, { sandbox: { ...nativePolicy.sandbox, excludeSlashTmp: false } },
]) assert.throws(() => assert.deepEqual(codexViewSnapshot({ ...nativePolicy, ...mutation }, binding, "test-model", "/private/task"), snapshot));
process.stdout.write("project acceptance assertion tests passed\n");
