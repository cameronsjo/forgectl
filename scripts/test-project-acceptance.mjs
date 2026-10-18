import assert from "node:assert/strict";
import { assertBoundRun, assertNativeEvidence } from "./project-acceptance.mjs";
import { assertAgentsModelLabels, assertAgentsSnapshot, codexViewSnapshot, ProjectPresentationAcceptance } from "./project-presentation-acceptance.mjs";

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
const unknownAgents = structuredClone(agentsSnapshot);
unknownAgents.rows[0].spans[1].span.state = "unknown";
unknownAgents.rows[0].spans[1].span.turn_id = "recovery-turn";
const unknownBinding = { ...agentsBinding, current: { ...agentsBinding.current, state: "unknown", turnId: "recovery-turn" } };
assert.equal(assertAgentsSnapshot(unknownAgents, "project-1", [unknownBinding]), unknownAgents);
assert.throws(() => assertAgentsSnapshot(unknownAgents, "project-1", [agentsBinding]));
assert.throws(() => assertAgentsSnapshot(unknownAgents, "project-1", [{ ...unknownBinding, current: { ...unknownBinding.current, turnId: "different-turn" } }]));
assert.throws(() => assertAgentsSnapshot(agentsSnapshot, "project-1", [unknownBinding]));
const modelSnapshot = { rows: [{ harness: "pi", model: "gpt-5.6-sol" }, { harness: "codex", model: "gpt-6-astra" }] };
assert.deepEqual(assertAgentsModelLabels("pi / gpt-5.6-sol\ncodex / gpt-6-astra", modelSnapshot), ["pi / gpt-5.6-sol", "codex / gpt-6-astra"]);
assert.throws(() => assertAgentsModelLabels("pi / openai-codex / gp\ncodex / gpt-6-astra", modelSnapshot));
assert.throws(() => assertAgentsModelLabels("pi / gpt-5.6-sol", modelSnapshot));

const report = {
  project_id: "project-1", thread_id: "thread-1", run_id: "run-1", process_liveness: "unknown",
  native: { native_session_id: "native-1", provider: "openai-codex", model: "test-model", turn: { id: "turn-1", status: "completed", observed_at: "2026-10-17T12:00:00.000Z" } },
  evidence: { thread_id: "thread-1", run_id: "run-1", native_session_id: "native-1", harness: "pi", source: "pi-inbox", turn_id: "turn-1", status: "completed", observed_at: "2026-10-17T12:00:00.000Z" },
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
for (const observed_at of [undefined, null, 123, "invalid", "0001-01-01T00:00:00Z"]) {
  assert.throws(() => assertNativeEvidence({ ...report, evidence: { ...report.evidence, observed_at } }, "project-1", "thread-1", binding, expected));
  assert.throws(() => assertNativeEvidence({ ...report, native: { ...report.native, turn: { ...report.native.turn, observed_at } } }, "project-1", "thread-1", binding, expected));
}
assert.throws(() => assertNativeEvidence({ ...report, evidence: { ...report.evidence, observed_at: "2026-10-17T12:00:01.000Z" } }, "project-1", "thread-1", binding, expected));

const interval = {
  id: "span-1", turn_id: "turn-1", state: "finished", provider: "openai-codex", model: "turn-model",
  started_at: "2026-10-17T11:59:00.000Z", ended_at: "2026-10-17T11:59:30.000Z",
};
const timedReport = {
  ...report,
  native: { ...report.native, activity: { version: 1, source: "pi-agent-events", observed_at: "2026-10-17T12:00:00.000Z", spans: [interval] } },
  evidence: { ...report.evidence, observed_at: "2026-10-17T12:00:00.000Z", started_at: interval.started_at, ended_at: interval.ended_at, provider: interval.provider, model: interval.model },
};
assert.equal(assertNativeEvidence(timedReport, "project-1", "thread-1", binding, expected), timedReport);
const repeatedReport = { ...timedReport, evidence: { ...timedReport.evidence, observed_at: "2026-10-17T11:59:45.000Z" } };
assert.equal(assertNativeEvidence(repeatedReport, "project-1", "thread-1", binding, expected), repeatedReport);
for (const mutation of [
  { source: "codex-turn-history" },
  { spans: [] },
  { spans: [interval, { ...interval, id: "span-2" }] },
  { spans: [{ ...interval, turn_id: "other-turn" }] },
  { spans: [{ ...interval, ended_at: undefined }] },
  { spans: [{ ...interval, state: "failed" }] },
  { spans: [{ ...interval, started_at: "invalid" }] },
  { spans: [{ ...interval, ended_at: "2026-10-17T11:58:00.000Z" }] },
  { observed_at: "2026-10-17T11:59:00.000Z" },
]) assert.throws(() => assertNativeEvidence({ ...timedReport, native: { ...timedReport.native, activity: { ...timedReport.native.activity, ...mutation } } }, "project-1", "thread-1", binding, expected));
for (const mutation of [
  { model: report.native.model },
  { provider: "invented-provider" },
  { started_at: undefined },
  { ended_at: "2026-10-17T11:59:31.000Z" },
  { source: "hermes-supervisor" },
  { observed_at: undefined },
  { observed_at: "2026-10-17T11:59:00.000Z" },
  { observed_at: "2026-10-17T12:00:01.000Z" },
]) assert.throws(() => assertNativeEvidence({ ...timedReport, evidence: { ...timedReport.evidence, ...mutation } }, "project-1", "thread-1", binding, expected));
for (const span of [{ ...interval, turn_id: "other-turn" }, { ...interval, ended_at: undefined, state: "running" }]) {
  const untimed = { ...timedReport, native: { ...timedReport.native, activity: { ...timedReport.native.activity, spans: [span] } }, evidence: report.evidence };
  assert.equal(assertNativeEvidence(untimed, "project-1", "thread-1", binding, expected), untimed);
}
const unknownReport = structuredClone(timedReport);
unknownReport.native.turn.status = "unknown";
unknownReport.evidence.status = "unknown";
unknownReport.native.activity.spans[0].state = "unknown";
const unknownExpected = { provider: expected.provider, model: expected.model, status: "unknown", turnId: "turn-1" };
assert.equal(assertNativeEvidence(unknownReport, "project-1", "thread-1", binding, unknownExpected), unknownReport);
assert.throws(() => assertNativeEvidence(unknownReport, "project-1", "thread-1", binding, expected));
assert.throws(() => assertNativeEvidence(unknownReport, "project-1", "thread-1", binding, { ...unknownExpected, turnId: "other-turn" }));
assert.throws(() => assertNativeEvidence(timedReport, "project-1", "thread-1", binding, unknownExpected));

const keyCalls = [];
let keyCode = 0;
const keyTarget = {
  evidence: {}, herdr: "/private/herdr", session: "isolated-session", env: {}, workspace: { ref: { pane: "w1:p1" } },
  projects: { runCommand: async (...args) => { keyCalls.push(args); return { code: keyCode, stdout: "", stderr: keyCode ? "refused" : "" }; } },
};
await ProjectPresentationAcceptance.prototype.sendKeys.call(keyTarget, ["up", "down", "Enter"]);
assert.deepEqual(keyCalls[0][2], ["--session", "isolated-session", "pane", "send-keys", "w1:p1", "up", "down", "Enter"]);
keyCode = 1;
await assert.rejects(() => ProjectPresentationAcceptance.prototype.sendKeys.call(keyTarget, ["Enter"]), /herdr send-keys failed: refused/);

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
