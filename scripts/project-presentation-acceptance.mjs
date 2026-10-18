import assert from "node:assert/strict";
import { mkdir, readFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { assertNoDuplicateMarkers, assertUnrelatedUnchanged, commandEnvironment, herdrCommand, ownershipMarker, paneSnapshot, prepareRoot, startHerdr, stopOwnedServer, workspaceSnapshot } from "./verify-project-presentation.mjs";

const AGENTS_MAX_ROWS = 64;
const AGENTS_MAX_SPANS_PER_ROW = 64;
const AGENTS_MAX_SPANS = 512;
const AGENTS_MAX_JSON_BYTES = 2 << 20;
const AGENTS_SOURCES = new Set(["codex-turn-history", "pi-agent-events", "hermes-prompt"]);
const AGENTS_STATES = new Set(["running", "finished", "failed", "incomplete", "unknown"]);
const AGENTS_ACTIVITY = new Map([
  ["codex", { source: "codex-turn-history", coverage: "native-history" }],
  ["pi", { source: "pi-agent-events", coverage: "current-process" }],
  ["hermes", { source: "hermes-prompt", coverage: "current-process" }],
]);

function assertBoundedCount(value, label, maximum) {
  assert(Number.isInteger(value) && value >= 0 && value <= maximum, `${label} must be a bounded non-negative integer`);
}

function assertTimedAgentSpan(span, expected, observedAt) {
  assert.equal(span.run_id, expected.run_id);
  assert.equal(span.native_session_id, expected.native_session_id);
  assert.equal(span.source, expected.source);
  assert(AGENTS_SOURCES.has(span.source), `unsupported native activity source ${span.source}`);
  assert(span.span && typeof span.span === "object");
  assert(typeof span.span.id === "string" && span.span.id.length > 0);
  assert(AGENTS_STATES.has(span.span.state), `unsupported native activity state ${span.span.state}`);
  const started = Date.parse(span.span.started_at);
  assert(Number.isFinite(started), "native activity span lacks a valid start");
  if (span.span.ended_at !== undefined && span.span.ended_at !== null) {
    const ended = Date.parse(span.span.ended_at);
    assert(Number.isFinite(ended), "native activity span has an invalid end");
    assert(ended >= started, "native activity span ends before it starts");
  }
  const observed = Date.parse(observedAt);
  assert(Number.isFinite(observed), "Agents snapshot lacks a valid observation time");
  assert(started <= observed + 5_000, "native activity span starts after its observation");
  if (span.span.ended_at) assert(Date.parse(span.span.ended_at) <= observed + 5_000, "native activity span ends after its observation");
  return span.span.ended_at !== undefined && span.span.ended_at !== null;
}

export function assertAgentsSnapshot(snapshot, projectId, expectedBindings) {
  assert(snapshot && typeof snapshot === "object" && !Array.isArray(snapshot));
  assert.equal(snapshot.version, 1);
  assert.equal(snapshot.project_id, projectId);
  assert(Array.isArray(expectedBindings) && expectedBindings.length > 0);
  assertBoundedCount(snapshot.total_threads, "Agents total_threads", AGENTS_MAX_ROWS);
  assert.equal(snapshot.total_threads, expectedBindings.length);
  assert(Array.isArray(snapshot.rows));
  assert(snapshot.rows.length <= AGENTS_MAX_ROWS);
  assert.equal(snapshot.rows.length, expectedBindings.length);
  assert.equal(snapshot.partial ?? false, false, "connected Agents snapshot must not be partial");
  assertBoundedCount(snapshot.dropped_threads ?? 0, "Agents dropped_threads", AGENTS_MAX_ROWS);
  assertBoundedCount(snapshot.dropped_spans ?? 0, "Agents dropped_spans", AGENTS_MAX_SPANS);
  assert.equal(snapshot.dropped_threads ?? 0, 0);
  assert.equal(snapshot.dropped_spans ?? 0, 0);
  assert(typeof snapshot.observed_at === "string" && Number.isFinite(Date.parse(snapshot.observed_at)));
  assert(Buffer.byteLength(JSON.stringify(snapshot), "utf8") <= AGENTS_MAX_JSON_BYTES, "Agents snapshot exceeded its JSON byte bound");
  const seenThreads = new Set();
  let activityCount = 0;
  for (const expected of expectedBindings) {
    assert(typeof expected.thread_id === "string" && expected.thread_id.length > 0);
    assert(expected.current && expected.history?.length > 0);
    assert(typeof expected.current.run_id === "string" && expected.current.run_id.length > 0);
    assert(typeof expected.current.native_session_id === "string" && expected.current.native_session_id.length > 0);
    const row = snapshot.rows.find((candidate) => candidate.thread_id === expected.thread_id);
    assert(row, `Agents snapshot omitted bound thread ${expected.thread_id}`);
    assert(!seenThreads.has(row.thread_id), `Agents snapshot repeated thread ${row.thread_id}`);
    seenThreads.add(row.thread_id);
    assert.equal(row.run_id, expected.current.run_id);
    assert.equal(row.harness, expected.current.harness);
    assert.equal(row.enrollment, "current");
    assert.equal(row.partial ?? false, false, `connected Agents row ${expected.thread_id} must not be partial`);
    assert.equal(row.coverage, expected.current.coverage);
    assert(Array.isArray(row.spans));
    assert(row.spans.length <= AGENTS_MAX_SPANS_PER_ROW);
    assert(row.spans.length > 0, `Agents snapshot omitted native spans for ${expected.thread_id}`);
    const knownSpans = new Map(expected.history.map((binding) => [`${binding.run_id}\u0000${binding.native_session_id}\u0000${binding.source}`, binding]));
    const expectedState = expected.current.state ?? "finished";
    assert(["finished", "incomplete", "failed", "unknown"].includes(expectedState));
    let currentMatchingSpan = false;
    for (const span of row.spans) {
      activityCount += 1;
      const known = knownSpans.get(`${span.run_id}\u0000${span.native_session_id}\u0000${span.source}`);
      assert(known, `Agents snapshot included an unknown historical span for ${expected.thread_id}`);
      const ended = assertTimedAgentSpan(span, known, snapshot.observed_at);
      if (known.run_id === expected.current.run_id && known.native_session_id === expected.current.native_session_id && known.source === expected.current.source && ended && span.span.state === expectedState && (!expected.current.turnId || span.span.turn_id === expected.current.turnId)) currentMatchingSpan = true;
    }
    const points = row.points ?? [];
    assert(Array.isArray(points));
    assert(points.length <= AGENTS_MAX_SPANS_PER_ROW);
    const knownPoints = new Map(expected.history.map((binding) => [`${binding.run_id}\u0000${binding.source}`, binding]));
    for (const point of points) {
      activityCount += 1;
      const known = knownPoints.get(`${point.run_id}\u0000${point.source}`);
      assert(known, `Agents snapshot included an unknown historical point for ${expected.thread_id}`);
      assert(typeof point.turn_id === "string" && point.turn_id.length > 0);
      assert(AGENTS_STATES.has(point.state), `unsupported native activity point state ${point.state}`);
      const at = Date.parse(point.at);
      assert(Number.isFinite(at) && at <= Date.parse(snapshot.observed_at) + 5_000, "native activity point has an invalid timestamp");
    }
    assert(currentMatchingSpan, `Agents snapshot omitted the current ${expectedState} timed span for ${expected.thread_id}`);
  }
  assert.equal(seenThreads.size, snapshot.rows.length, "Agents snapshot contains an unexpected thread");
  assert(activityCount <= AGENTS_MAX_SPANS);
  return snapshot;
}

export function codexViewSnapshot(response, binding, model, acceptanceRoot) {
  assert(acceptanceRoot?.startsWith("/"));
  assert.equal(response.thread.id, binding.native_session_id);
  assert.equal(response.thread.cwd, binding.worktree);
  assert.equal(response.cwd, binding.worktree);
  assert.equal(response.model, model);
  assert.equal(response.modelProvider, "openai");
  assert.equal(response.approvalsReviewer, "user");
  assert.equal(response.approvalPolicy, "never");
  assert.equal(response.activePermissionProfile ?? null, null);
  assert.deepEqual(response.sandbox, { type: "workspaceWrite", networkAccess: false, writableRoots: [acceptanceRoot], excludeTmpdirEnvVar: true, excludeSlashTmp: true });
  assert(Array.isArray(response.runtimeWorkspaceRoots) && response.runtimeWorkspaceRoots.includes(binding.worktree));
  assert.equal(new Set(response.runtimeWorkspaceRoots).size, response.runtimeWorkspaceRoots.length);
  assert(response.runtimeWorkspaceRoots.every((root) => typeof root === "string" && (root === acceptanceRoot || root.startsWith(`${acceptanceRoot}/`))));
  return {
    sessionId: response.thread.id, cwd: response.cwd, model: response.model, provider: response.modelProvider,
    approvalPolicy: response.approvalPolicy, approvalsReviewer: response.approvalsReviewer,
    runtimeWorkspaceRoots: response.runtimeWorkspaceRoots, sandbox: response.sandbox,
    activePermissionProfile: response.activePermissionProfile ?? null,
  };
}

export class ProjectPresentationAcceptance {
  constructor(projects, { herdr, root }) {
    this.projects = projects;
    this.evidence = projects.evidence;
    this.herdr = herdr;
    this.root = join(root, "ui");
    this.acceptanceRoot = root;
    this.session = `fcp-${randomUUID().replaceAll("-", "").slice(0, 12)}`;
    this.env = commandEnvironment({}, this.root);
    this.views = new Map();
    this.renders = 0;
  }

  async command(args) {
    const result = await this.projects.runCommand(this.evidence, this.projects.forgectl, ["surface", "project", "--root", this.projects.root, ...args, "--session", this.session, "--herdr-binary", this.herdr, "--json"], { env: this.env, cwd: this.projects.worktree, timeout: 120_000 });
    assert.equal(result.code, 0, `presentation command failed: ${result.stderr}`);
    return JSON.parse(result.stdout);
  }

  async start() {
    await mkdir(this.root, { mode: 0o700 });
    await prepareRoot(this.root);
    this.server = await startHerdr(this.evidence, this.herdr, this.session, this.env);
    const unrelated = await herdrCommand(this.evidence, this.herdr, this.session, this.env, ["workspace", "create", "--cwd", this.projects.worktree, "--label", "Unrelated acceptance surface", "--no-focus"]);
    this.unrelatedId = unrelated.result.workspace.workspace_id;
    this.unrelatedBefore = await workspaceSnapshot(this.evidence, this.herdr, this.session, this.env, this.unrelatedId);
    const opened = await this.command(["open", this.projects.project.id]);
    assert.equal(opened.action, "create");
    assert.equal(opened.operation.phase, "verified");
    this.workspace = opened.workspace;
    const reopened = await this.command(["open", this.projects.project.id]);
    assert.equal(reopened.action, "focused");
    assert.deepEqual(reopened.workspace, this.workspace);
    await this.openTimeline("worker");
    return this;
  }

  async open(name, view, extra = []) {
    const threadId = this.projects.thread(name);
    const opened = await this.command(["thread", "--project", this.projects.project.id, "open", threadId, "--view", view, ...extra]);
    assert.equal(opened.thread_id, threadId);
    assert.equal(opened.run_id, this.projects.bindings.get(name).run_id);
    assert.equal(opened.operation.presentation, view === "native" ? "codex-remote" : "timeline");
    assert.equal(opened.operation.phase, "verified");
    assert.equal(opened.process_liveness, "unknown");
    assert.deepEqual(opened.operation.parent, this.workspace.ref);
    this.views.set(name, opened.operation);
    await this.checkSurface(name);
    const reopened = await this.command(["thread", "--project", this.projects.project.id, "open", threadId, "--view", view, ...extra]);
    assert.equal(reopened.action, "focused");
    assert.equal(reopened.operation.id, opened.operation.id);
    await this.checkSurface(name);
    return opened.operation;
  }

  async openTimeline(name) { return this.open(name, "timeline"); }

  async checkSurface(name) {
    const operation = this.views.get(name);
    const snapshot = await workspaceSnapshot(this.evidence, this.herdr, this.session, this.env, this.workspace.ref.workspace);
    assertNoDuplicateMarkers(snapshot.workspaces, snapshot.tabs, `fc-surface-${this.workspace.ref.tag}`, ownershipMarker(operation));
    const pane = await paneSnapshot(this.evidence, this.herdr, this.session, this.env, operation.ref.pane);
    for (const [field, expected] of Object.entries({ workspace_id: operation.ref.workspace, tab_id: operation.ref.tab, terminal_id: operation.ref.terminal })) assert.equal(pane[field], expected);
    const shown = await this.projects.command(["thread", "--project", this.projects.project.id, "show", this.projects.thread(name)]);
    assert.equal(pane.display_agent, shown.thread.name);
    assert.equal(snapshot.workspace.label, `${this.projects.project.name} · fc-surface-${this.workspace.ref.tag}`);
    assert.equal(snapshot.tabs.find((tab) => tab.tab_id === operation.ref.tab)?.label, `${shown.thread.name} · ${ownershipMarker(operation)}`);
    assert.equal(snapshot.workspace.tokens?.forgectl_project, this.projects.project.name);
  }

  async render(name, content = "") {
    const shown = await this.projects.command(["thread", "--project", this.projects.project.id, "show", this.projects.thread(name)]);
    await this.command(["thread", "--project", this.projects.project.id, "focus", this.projects.thread(name)]);
    const output = join(this.root, `render-${++this.renders}`);
    const probe = join(dirname(fileURLToPath(import.meta.url)), "probe-project-render.py");
    const args = [probe, "--herdr", this.herdr, "--session", this.session, "--output", output, "--expect-project", this.projects.project.name, "--expect-thread", shown.thread.name];
    if (content) args.push("--expect-content", content);
    const result = await this.projects.runCommand(this.evidence, "/usr/bin/python3", args, { env: this.env, cwd: this.projects.worktree, timeout: 120_000 });
    assert.equal(result.code, 0, `rendered project client failed: ${result.stderr}`);
    this.evidence.add("project-rendered-client", { projectId: this.projects.project.id, threadId: shown.thread.id, operationId: this.views.get(name).id, receipt: JSON.parse(result.stdout), contentMarker: content || null });
  }

  async closeView(name) {
    const operation = this.views.get(name);
    if (!operation) return;
    const closed = await this.command(["thread", "--project", this.projects.project.id, "close-view", this.projects.thread(name), "--operation", operation.id]);
    assert(["closed", "already_gone"].includes(closed.operation.retirement.phase));
    this.views.delete(name);
    this.evidence.add("project-view-retired", { operationId: operation.id, threadId: this.projects.thread(name), runId: operation.run_id, nativeProcessStopped: false });
  }

  async native({ codex, model, rpc, marker }) {
    const binding = this.projects.bindings.get("coordinator");
    const params = { threadId: binding.native_session_id, excludeTurns: true };
    const before = codexViewSnapshot(await rpc.rpc("thread/resume", params), binding, model, this.acceptanceRoot);
    await this.closeView("coordinator");
    const extra = ["--codex-binary", codex, "--expect-provider", "openai", "--expect-model", model];
    const first = await this.open("coordinator", "native", extra);
    await this.render("coordinator", marker);
    const after = codexViewSnapshot(await rpc.rpc("thread/resume", params), binding, model, this.acceptanceRoot);
    assert.deepEqual(after, before, "native UI changed model, policy, or workspace roots");
    await this.closeView("coordinator");
    const second = await this.open("coordinator", "native", extra);
    assert.notEqual(second.id, first.id);
    assert.notEqual(second.ref.terminal, first.ref.terminal);
    await this.render("coordinator", marker);
    assert.deepEqual(codexViewSnapshot(await rpc.rpc("thread/resume", params), binding, model, this.acceptanceRoot), before);
    this.evidence.add("project-native-ui-reconnected", { projectId: this.projects.project.id, threadId: this.projects.thread("coordinator"), runId: binding.run_id, nativeSessionId: binding.native_session_id, previousOperationId: first.id, operationId: second.id, postureUnchanged: true, renderedHistoryMarker: marker });
  }

  expectedAgentBindings(expectedActivity = {}) {
    return [...this.projects.bindings.entries()].map(([name, binding]) => ({
      name,
      thread_id: this.projects.thread(name),
      current: { ...this.expectedAgentBinding(binding), ...expectedActivity[name] },
      history: (this.projects.histories.get(name) ?? [binding]).map((historical) => this.expectedAgentBinding(historical)),
    }));
  }

  expectedAgentBinding(binding) {
    const activity = AGENTS_ACTIVITY.get(binding.harness);
    assert(activity, `unsupported Agents harness ${binding.harness}`);
    return { run_id: binding.run_id, native_session_id: binding.native_session_id, harness: binding.harness, ...activity };
  }

  async focusAgentsTab() {
    const tab = this.workspace.ref.tab;
    await herdrCommand(this.evidence, this.herdr, this.session, this.env, ["tab", "focus", tab]);
    const focused = await workspaceSnapshot(this.evidence, this.herdr, this.session, this.env, this.workspace.ref.workspace);
    assert.equal(focused.workspace.active_tab_id, tab);
    assert(focused.tabs.some((candidate) => candidate.tab_id === tab));
    return focused;
  }

  async focusWorkerFromAgents(snapshot) {
    const workerThread = this.projects.thread("worker");
    const workerIndex = snapshot.rows.findIndex((row) => row.thread_id === workerThread);
    assert(workerIndex >= 0, "Agents snapshot omitted the worker row");
    const beforeReport = await this.projects.command(["show", this.projects.project.id]);
    const beforeSurfaceOperations = beforeReport.project.surface_operations.map((operation) => ({ id: operation.id, thread_id: operation.thread_id, run_id: operation.run_id, presentation: operation.presentation, phase: operation.phase, retirement: operation.retirement?.phase ?? null }));
    const before = await this.focusAgentsTab();
    await this.sendKeys([...Array(AGENTS_MAX_ROWS).fill("up"), ...Array(workerIndex).fill("down")]);
    await this.sendKeys(["Enter"]);
    const workerOperation = this.views.get("worker");
    assert(workerOperation, "worker view is required before Agents Enter acceptance");
    let after;
    const deadline = Date.now() + 15_000;
    while (Date.now() < deadline) {
      after = await workspaceSnapshot(this.evidence, this.herdr, this.session, this.env, this.workspace.ref.workspace);
      if (after.workspace.active_tab_id === workerOperation.ref.tab) break;
      await new Promise((resolvePromise) => setTimeout(resolvePromise, 250));
    }
    assert(after);
    assert.equal(after.workspace.active_tab_id, workerOperation.ref.tab, "Agents Enter did not focus the exact worker view");
    assert.equal(after.tabs.length, before.tabs.length, "Agents Enter created a duplicate tab");
    assertNoDuplicateMarkers(after.workspaces, after.tabs, `fc-surface-${this.workspace.ref.tag}`, ownershipMarker(workerOperation));
    const workerPane = await paneSnapshot(this.evidence, this.herdr, this.session, this.env, workerOperation.ref.pane);
    assert.equal(workerPane.workspace_id, workerOperation.ref.workspace);
    assert.equal(workerPane.tab_id, workerOperation.ref.tab);
    assert.equal(workerPane.terminal_id, workerOperation.ref.terminal);
    assert.equal(workerPane.display_agent, (await this.projects.command(["thread", "--project", this.projects.project.id, "show", workerThread])).thread.name);
    const afterReport = await this.projects.command(["show", this.projects.project.id]);
    const afterSurfaceOperations = afterReport.project.surface_operations.map((operation) => ({ id: operation.id, thread_id: operation.thread_id, run_id: operation.run_id, presentation: operation.presentation, phase: operation.phase, retirement: operation.retirement?.phase ?? null }));
    assert.deepEqual(afterSurfaceOperations, beforeSurfaceOperations, "Agents Enter changed surface operations");
    this.evidence.add("project-agents-enter-focus", { projectId: this.projects.project.id, workerThread, workerRunId: this.projects.bindings.get("worker").run_id, projectTab: this.workspace.ref.tab, workerTab: workerOperation.ref.tab, tabCountUnchanged: true, surfaceOperationsUnchanged: true });
  }

  async sendKeys(keys) {
    const result = await this.projects.runCommand(this.evidence, this.herdr, ["--session", this.session, "pane", "send-keys", this.workspace.ref.pane, ...keys], { env: this.env });
    assert.equal(result.code, 0, `herdr send-keys failed: ${result.stderr}`);
  }

  async renderAgents(snapshot) {
    await this.focusAgentsTab();
    const shown = await this.projects.command(["thread", "--project", this.projects.project.id, "show", this.projects.thread("worker")]);
    const output = join(this.root, `agents-render-${++this.renders}`);
    const probe = join(dirname(fileURLToPath(import.meta.url)), "probe-project-render.py");
    const args = [probe, "--herdr", this.herdr, "--session", this.session, "--output", output, "--expect-project", this.projects.project.name, "--expect-content", shown.thread.name];
    const result = await this.projects.runCommand(this.evidence, "/usr/bin/python3", args, { env: this.env, cwd: this.projects.worktree, timeout: 120_000 });
    assert.equal(result.code, 0, `Agents render probe failed: ${result.stderr}`);
    const receipt = JSON.parse(result.stdout);
    assert.equal(receipt.assertions.project, true);
    assert.equal(receipt.assertions.content, true);
    const screen = await readFile(join(output, "render.txt"), "utf8");
    assert(screen.includes("Agents"), "Agents render omitted its title");
    assert(screen.includes("30m"), "Agents render omitted the 30m axis label");
    assert(screen.includes(shown.thread.name), "Agents render omitted the worker row");
    this.evidence.add("project-agents-rendered", { projectId: this.projects.project.id, workerThread: shown.thread.id, receipt, contentMarker: "Agents", rowMarker: shown.thread.name, axisMarker: "30m", centralContent: true });
    const negativeOutput = join(this.root, `agents-render-negative-${this.renders}`);
    const wrongContent = `absent-agents-${randomUUID()}`;
    const negative = await this.projects.runCommand(this.evidence, "/usr/bin/python3", [probe, "--herdr", this.herdr, "--session", this.session, "--output", negativeOutput, "--expect-project", this.projects.project.name, "--expect-content", wrongContent], { env: this.env, cwd: this.projects.worktree, timeout: 120_000 });
    assert.equal(negative.code, 1, "Agents render probe must reject wrong content");
    const negativeReceipt = JSON.parse(await readFile(join(negativeOutput, "render.json"), "utf8"));
    assert.equal(negativeReceipt.ok, false);
    assert.equal(negativeReceipt.assertions.content, false);
    assert(negativeReceipt.screen.captured_bytes > 0, "wrong-content Agents control must capture terminal bytes");
    this.evidence.add("project-agents-render-negative", { projectId: this.projects.project.id, wrongContent, receipt: negativeReceipt, capturedBytes: negativeReceipt.screen.captured_bytes });
    return snapshot;
  }

  async agents(expectedActivity = {}) {
    const snapshot = await this.projects.command(["agents", this.projects.project.id]);
    assertAgentsSnapshot(snapshot, this.projects.project.id, this.expectedAgentBindings(expectedActivity));
    await this.renderAgents(snapshot);
    await this.focusWorkerFromAgents(snapshot);
    return snapshot;
  }

  async audit(expectedActivity = {}) {
    await this.agents(expectedActivity);
    for (const name of this.views.keys()) await this.checkSurface(name);
    assertUnrelatedUnchanged(this.unrelatedBefore, await workspaceSnapshot(this.evidence, this.herdr, this.session, this.env, this.unrelatedId));
    this.evidence.add("project-presentation-audited", { projectId: this.projects.project.id, session: this.session, renderedClients: this.renders, unrelatedSurfaceUnchanged: true });
  }

  async close() {
    if (!this.server) return;
    const server = this.server;
    this.server = undefined;
    try {
      const result = await this.projects.runCommand(this.evidence, this.herdr, ["--session", this.session, "server", "stop"], { env: this.env, timeout: 30_000 });
      assert.equal(result.code, 0, "owned presentation server did not accept shutdown");
    } finally {
      await stopOwnedServer(server);
    }
    this.evidence.add("project-presentation-cleaned", { session: this.session, pid: server.pid });
  }
}
