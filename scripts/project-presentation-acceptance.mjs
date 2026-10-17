import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { assertNoDuplicateMarkers, assertUnrelatedUnchanged, commandEnvironment, herdrCommand, ownershipMarker, paneSnapshot, prepareRoot, startHerdr, stopOwnedServer, workspaceSnapshot } from "./verify-project-presentation.mjs";

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

  async audit() {
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
