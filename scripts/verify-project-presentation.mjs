#!/usr/bin/env node

import assert from "node:assert/strict";
import { appendFileSync } from "node:fs";
import { lstat, mkdir, readdir, readFile, realpath, stat, writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { spawn } from "node:child_process";
import { join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const COMMAND_TIMEOUT = 30_000;
const SERVER_TIMEOUT = 120_000;
const MAX_OUTPUT = 24_000;
const PROJECT_TOKEN = "forgectl_project";
const OWNERSHIP_PREFIX = "fc-surface-";

const usage = `usage: verify-project-presentation.mjs --live --forgectl ABS --herdr ABS --root ABS

Runs the disposable source-build forgectl Projects/Herdr presentation acceptance
loop. The root must already be an existing empty private directory. The run
uses only a unique named Herdr session and leaves JSONL evidence in the root.
`;

function fail(message) {
  throw new Error(message);
}

export function parseArgs(argv) {
  const result = { live: false };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--live") result.live = true;
    else if (argument === "--help" || argument === "-h") return { help: true };
    else if (["--forgectl", "--herdr", "--root"].includes(argument)) {
      const value = argv[++index];
      if (!value || value.startsWith("--")) fail(`${argument} needs a value`);
      result[argument.slice(2)] = value;
    } else fail(`unknown argument ${argument}`);
  }
  if (!result.live) fail("refusing a live presentation run without explicit --live");
  for (const name of ["forgectl", "herdr", "root"]) {
    if (!result[name] || !result[name].startsWith("/")) fail(`--${name} must be an absolute path`);
  }
  return result;
}

function bounded(value) {
  const text = String(value ?? "");
  return text.length > MAX_OUTPUT ? `${text.slice(0, MAX_OUTPUT)}…` : text;
}

function sleep(milliseconds) {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds));
}

function lastJson(text, label) {
  const raw = String(text).trim();
  if (!raw) fail(`${label} returned empty stdout; expected JSON`);
  try {
    return JSON.parse(raw);
  } catch (error) {
    fail(`${label} returned invalid JSON: ${bounded(raw)} (${error})`);
  }
}

function resultOf(document, label) {
  if (!document || typeof document !== "object" || !document.result || typeof document.result !== "object") {
    fail(`${label} did not return a Herdr result object`);
  }
  return document.result;
}

function ensureString(value, label) {
  if (typeof value !== "string" || value.length === 0) fail(`${label} must be a non-empty string`);
  return value;
}

function ensureRef(ref, label) {
  if (!ref || typeof ref !== "object") fail(`${label} is missing a reference`);
  for (const field of ["workspace", "tab", "pane", "terminal"]) ensureString(ref[field], `${label}.${field}`);
  assert.equal(ref.version, 2);
  assert.equal(ref.kind, "herdr");
  assert.equal(ref.source, "herdr-named-session");
  assert.match(ref.tag, /^[a-f0-9]{32}$/);
  assert.match(ref.server, /^[a-f0-9]{64}$/);
  assert.match(ref.endpoint, /^[a-f0-9]{64}$/);
  return ref;
}

export function ownershipMarker(operation) {
  assert.match(operation.tag, /^[a-f0-9]{32}$/);
  assert.equal(operation.ref.tag, operation.tag);
  assert.equal(operation.ref.server, operation.expected_server);
  assert.equal(operation.ref.endpoint, operation.expected_endpoint);
  return `${OWNERSHIP_PREFIX}${operation.tag}`;
}

export function assertProjectOperation(operation, projectId, session, projectName) {
  assert.equal(operation.kind, "workspace");
  assert.equal(operation.session, session);
  assert.equal(operation.phase, "verified");
  assert.equal(operation.thread_id ?? "", "");
  assert.equal(operation.ref.workspace, operation.ref.tab.split(":")[0]);
  ensureRef(operation.ref, "workspace operation ref");
  ownershipMarker(operation);
  assert.equal(projectId.length > 0, true);
  assert.equal(projectName.length > 0, true);
  return operation;
}

export function assertThreadOperation(operation, projectId, threadId, runId, session) {
  assert.equal(operation.kind, "thread");
  assert.equal(operation.thread_id, threadId);
  assert.equal(operation.run_id, runId);
  assert.equal(operation.session, session);
  assert.equal(operation.presentation, "timeline");
  assert.equal(operation.phase, "verified");
  ensureRef(operation.parent, "thread operation parent");
  ensureRef(operation.ref, "thread operation ref");
  assert.equal(operation.ref.workspace, operation.parent.workspace);
  assert.equal(projectId.length > 0, true);
  assert.equal(operation.ref.server, operation.parent.server);
  assert.equal(operation.ref.endpoint, operation.parent.endpoint);
  assert.notEqual(operation.ref.tab, operation.parent.tab);
  assert.notEqual(operation.ref.pane, operation.parent.pane);
  assert.notEqual(operation.ref.terminal, operation.parent.terminal);
  ownershipMarker(operation);
  return operation;
}

export function assertNoDuplicateMarkers(workspaces, tabs, workspaceMarker, tabMarker) {
  const matches = (label, marker) => label === marker || label.endsWith(` · ${marker}`);
  assert.equal(workspaces.filter((workspace) => matches(workspace.label, workspaceMarker)).length, 1, `workspace marker ${workspaceMarker} must be unique`);
  assert.equal(tabs.filter((tab) => matches(tab.label, tabMarker)).length, 1, `tab marker ${tabMarker} must be unique`);
}

function stableWorkspaceSnapshot(snapshot) {
  return {
    workspace: {
      workspace_id: snapshot.workspace.workspace_id,
      label: snapshot.workspace.label,
      pane_count: snapshot.workspace.pane_count,
      tab_count: snapshot.workspace.tab_count,
      active_tab_id: snapshot.workspace.active_tab_id,
      tokens: snapshot.workspace.tokens ?? {},
    },
    tabs: snapshot.tabs.map((tab) => ({
      tab_id: tab.tab_id,
      workspace_id: tab.workspace_id,
      label: tab.label,
      pane_count: tab.pane_count,
    })),
    panes: snapshot.panes.map((pane) => ({
      pane_id: pane.pane_id,
      tab_id: pane.tab_id,
      workspace_id: pane.workspace_id,
      terminal_id: pane.terminal_id,
      cwd: pane.cwd,
      label: pane.label,
      title: pane.title,
      display_agent: pane.display_agent,
    })),
  };
}

export function assertUnrelatedUnchanged(before, after) {
  assert.deepEqual(stableWorkspaceSnapshot(after), stableWorkspaceSnapshot(before));
}

class Evidence {
  constructor(root) {
    this.root = root;
    this.path = join(root, "evidence.jsonl");
    this.records = [];
  }

  add(kind, data = {}) {
    const record = { at: new Date().toISOString(), kind, ...data };
    this.records.push(record);
    appendFileSync(this.path, `${JSON.stringify(record)}\n`, { mode: 0o600 });
  }
}

export function commandEnvironment(base, root) {
  const environment = { PATH: "/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin", LANG: "en_US.UTF-8", TERM: "xterm-256color", SHELL: "/bin/sh", ENV: "/dev/null", BASH_ENV: "/dev/null" };
  if (base.TZ) environment.TZ = base.TZ;
  environment.HOME = join(root, "home");
  environment.ZDOTDIR = environment.HOME;
  environment.XDG_CONFIG_HOME = join(root, "xdg-config");
  environment.XDG_STATE_HOME = join(root, "xdg-state");
  environment.XDG_CACHE_HOME = join(root, "xdg-cache");
  environment.TMPDIR = join(root, "tmp");
  environment.HERDR_CONFIG_PATH = join(root, "herdr-config", "config.toml");
  return environment;
}

function runCommand(evidence, command, args, options = {}) {
  const env = options.env ?? process.env;
  const timeout = options.timeout ?? COMMAND_TIMEOUT;
  evidence.add("command-start", { command, args, cwd: options.cwd });
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { cwd: options.cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    let killTimer;
    const timer = setTimeout(() => {
      child.kill("SIGTERM");
      killTimer = setTimeout(() => child.kill("SIGKILL"), 5_000);
    }, timeout);
    child.once("error", (error) => { clearTimeout(timer); clearTimeout(killTimer); reject(error); });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      clearTimeout(killTimer);
      const result = { code, signal, stdout, stderr };
      evidence.add("command-result", { command, args, code, signal, stdout: bounded(stdout), stderr: bounded(stderr) });
      resolvePromise(result);
    });
  });
}

async function requireCommand(evidence, command, args, options = {}) {
  const result = await runCommand(evidence, command, args, options);
  if (result.code !== 0) fail(`${command} ${args.join(" ")} failed with ${result.code}: ${bounded(result.stderr)}`);
  return result;
}

function waitClosed(child, timeout = SERVER_TIMEOUT) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((resolvePromise, reject) => {
    const timer = setTimeout(() => reject(new Error(`owned Herdr server child ${child.pid ?? "unknown"} did not exit`)), timeout);
    child.once("close", () => { clearTimeout(timer); resolvePromise(); });
  });
}

async function ensureFreshRoot(root) {
  const linkInfo = await lstat(root).catch(() => undefined);
  if (linkInfo?.isSymbolicLink()) fail("--root must not be a symbolic link");
  const info = await stat(root).catch(() => undefined);
  if (!info?.isDirectory()) fail("--root must be an existing directory created for this run");
  if ((info.mode & 0o077) !== 0) fail("--root must be private (mode 0700 or stricter)");
  const canonical = await realpath(root);
  if (canonical === "/" || canonical === resolve(process.cwd())) fail("--root is ambiguous or unsafe");
  if ((await readdir(canonical)).length !== 0) fail("--root must be empty; refusing to reuse a scratch directory");
  return canonical;
}

async function executable(path, label) {
  const info = await stat(path).catch(() => undefined);
  if (!info?.isFile() || (info.mode & 0o111) === 0) fail(`${label} is not a regular executable: ${path}`);
  return await realpath(path);
}

export async function prepareRoot(root) {
  for (const directory of ["home", "xdg-config", "xdg-state", "xdg-cache", "tmp", "herdr-config", "projects", "target-worktree", "unrelated-worktree", "worker-worktree", "unrelated-herdr-workspace"]) {
    await mkdir(join(root, directory), { recursive: true, mode: 0o700 });
  }
  const config = `onboarding = false\n\n[terminal]\ndefault_shell = "/bin/sh"\nshell_mode = "non_login"\n\n[update]\nversion_check = false\nmanifest_check = false\n\n[ui.sidebar.agents]\ngroup_by = { token = "${PROJECT_TOKEN}" }\n`;
  await writeFile(join(root, "herdr-config", "config.toml"), config, { mode: 0o600 });
  return config;
}

function herdrArgs(session, args) {
  return ["--session", session, ...args];
}

export async function herdrCommand(evidence, herdr, session, env, args, label = `herdr ${args.join(" ")}`) {
  const result = await requireCommand(evidence, herdr, herdrArgs(session, args), { env });
  return lastJson(result.stdout, label);
}

export async function startHerdr(evidence, herdr, session, env) {
  const child = spawn(herdr, ["--session", session, "server"], { env, stdio: ["ignore", "pipe", "pipe"] });
  child.stdout.on("data", (chunk) => evidence.add("herdr-server-stdout", { text: bounded(chunk) }));
  child.stderr.on("data", (chunk) => evidence.add("herdr-server-stderr", { text: bounded(chunk) }));
  evidence.add("herdr-server-start", { pid: child.pid, session });
  try {
    const deadline = Date.now() + SERVER_TIMEOUT;
    let lastFailure = "";
    while (Date.now() < deadline) {
      if (child.exitCode !== null || child.signalCode !== null) fail(`named Herdr server exited before readiness: ${lastFailure}`);
      const result = await runCommand(evidence, herdr, herdrArgs(session, ["workspace", "list"]), { env, timeout: 10_000 });
      if (result.code === 0) {
        const document = lastJson(result.stdout, "herdr workspace list");
        resultOf(document, "herdr workspace list");
        return child;
      }
      lastFailure = bounded(result.stderr);
      await sleep(500);
    }
    fail(`named Herdr server did not become ready: ${lastFailure}`);
  } catch (error) {
    await stopOwnedServer(child);
    throw error;
  }
}

export async function stopOwnedServer(child) {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill("SIGTERM");
  try {
    await waitClosed(child, 20_000);
  } catch {
    child.kill("SIGKILL");
    await waitClosed(child, 20_000);
    fail("owned server required forced termination; descendant cleanup is unverified");
  }
  assert(child.exitCode !== null || child.signalCode !== null, "owned Herdr server cleanup was not confirmed");
}

export async function workspaceSnapshot(evidence, herdr, session, env, workspaceId) {
  const listDocument = await herdrCommand(evidence, herdr, session, env, ["workspace", "list"]);
  const workspaces = resultOf(listDocument, "workspace list").workspaces;
  if (!Array.isArray(workspaces)) fail("workspace list did not return workspaces");
  const workspace = workspaces.find((candidate) => candidate.workspace_id === workspaceId);
  if (!workspace) fail(`workspace ${workspaceId} disappeared`);
  const workspaceDocument = await herdrCommand(evidence, herdr, session, env, ["workspace", "get", workspaceId]);
  const workspaceResult = resultOf(workspaceDocument, "workspace get");
  const tabsDocument = await herdrCommand(evidence, herdr, session, env, ["tab", "list", "--workspace", workspaceId]);
  const tabs = resultOf(tabsDocument, "tab list").tabs;
  const panesDocument = await herdrCommand(evidence, herdr, session, env, ["pane", "list", "--workspace", workspaceId]);
  const panes = resultOf(panesDocument, "pane list").panes;
  if (!Array.isArray(tabs) || !Array.isArray(panes)) fail("Herdr workspace snapshot omitted tabs or panes");
  return { workspace: workspaceResult.workspace ?? workspace, tabs, panes, workspaces };
}

export async function paneSnapshot(evidence, herdr, session, env, paneId) {
  const document = await herdrCommand(evidence, herdr, session, env, ["pane", "get", paneId]);
  const pane = resultOf(document, "pane get").pane;
  if (!pane) fail(`pane ${paneId} was not returned`);
  return pane;
}

async function forgeCommand(evidence, forgectl, env, root, args, cwd) {
  return requireCommand(evidence, forgectl, ["surface", "project", "--root", join(root, "projects"), ...args], { env, cwd });
}

async function forgeTeamCommand(evidence, forgectl, env, ledger, args, cwd) {
  return requireCommand(evidence, forgectl, ["surface", "team", "--ledger", ledger, ...args], { env, cwd });
}

async function runAcceptance(args) {
  const root = await ensureFreshRoot(args.root);
  const evidence = new Evidence(root);
  const session = `fcp-${randomUUID().replaceAll("-", "").slice(0, 12)}`;
  if (["default", "fleet"].includes(session)) fail("generated session name collided with a forbidden global session");
  const forgectl = await executable(args.forgectl, "--forgectl");
  const herdr = await executable(args.herdr, "--herdr");
  const env = commandEnvironment(process.env, root);
  for (const key of ["HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "HERDR_CONFIG_PATH", "TMPDIR"]) {
    const value = resolve(env[key]);
    if (value !== root && !value.startsWith(`${root}/`)) fail(`${key} escaped the private acceptance root`);
  }
  for (const key of ["HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_CLIENT_SOCKET_PATH"]) {
    if (Object.hasOwn(env, key)) fail(`${key} must not be inherited by the acceptance run`);
  }
  let server;
  let completed = false;
  let summary;
  evidence.add("acceptance-start", { session, forgectl, herdr, root, render: "pending" });
  try {
    const config = await prepareRoot(root);
    evidence.add("isolated-environment", {
      session,
      config,
      HOME: env.HOME,
      XDG_CONFIG_HOME: env.XDG_CONFIG_HOME,
      XDG_STATE_HOME: env.XDG_STATE_HOME,
      HERDR_CONFIG_PATH: env.HERDR_CONFIG_PATH,
      cleared: ["HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_CLIENT_SOCKET_PATH"],
    });
    server = await startHerdr(evidence, herdr, session, env);
    await requireCommand(evidence, herdr, herdrArgs(session, ["server", "reload-config"]), { env });
    evidence.add("herdr-config-reloaded", { session, configPath: env.HERDR_CONFIG_PATH, versionCheck: false, manifestCheck: false, groupToken: PROJECT_TOKEN });

    const targetCreateResult = await forgeCommand(evidence, forgectl, env, root, ["create", "Presentation Target", "--worktree", join(root, "target-worktree"), "--json"], join(root, "target-worktree"));
    const targetCreate = lastJson(targetCreateResult.stdout, "target project create");
    const targetProject = targetCreate.project;
    ensureString(targetProject?.id, "target project id");
    const targetLedger = ensureString(targetCreate.ledger, "target project ledger");
    if (resolve(targetLedger) !== root && !resolve(targetLedger).startsWith(`${root}/`)) fail("target project ledger escaped the private acceptance root");
    const unrelatedCreateResult = await forgeCommand(evidence, forgectl, env, root, ["create", "Unrelated Project", "--worktree", join(root, "unrelated-worktree"), "--json"], join(root, "unrelated-worktree"));
    const unrelatedCreate = lastJson(unrelatedCreateResult.stdout, "unrelated project create");
    const unrelatedProject = unrelatedCreate.project;
    ensureString(unrelatedProject?.id, "unrelated project id");
    const teamRunId = `presentation-worker-${randomUUID().replaceAll("-", "").slice(0, 16)}`;
    const registrationResult = await forgeTeamCommand(evidence, forgectl, env, targetLedger, [
      "register", "worker-fixture", "--harness", "codex", "--worktree", join(root, "worker-worktree"),
      "--socket", join(root, "codex.sock"), "--run-id", teamRunId, "--json",
    ], join(root, "worker-worktree"));
    const registration = lastJson(registrationResult.stdout, "surface team register");
    assert.equal(registration.ledger, targetLedger);
    assert.equal(registration.member.name, "worker-fixture");
    assert.equal(registration.member.harness, "codex");
    assert.equal(registration.member.run_id, teamRunId);
    assert.equal(registration.member.thread_id ?? "", "", "worker fixture must have no native thread");
    assert.equal(registration.environment.FORGECTL_RUN_ID, teamRunId);
    evidence.add("worker-fixture", { name: registration.member.name, harness: registration.member.harness, runId: teamRunId, nativeThread: "none", observation: "unknown", ledger: targetLedger });
    const workerThreadResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "create", "Worker timeline", "--json"], join(root, "target-worktree"));
    const workerThread = lastJson(workerThreadResult.stdout, "worker thread create").thread;
    ensureString(workerThread?.id, "worker thread id");
    const bindResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "bind", workerThread.id, "--member", "worker-fixture", "--run-id", teamRunId, "--expect-run", "none", "--json"], join(root, "target-worktree"));
    const bindingReport = lastJson(bindResult.stdout, "worker thread bind");
    assert.equal(bindingReport.thread.active_run_id, teamRunId);
    const bound = bindingReport.thread.runs.find((run) => run.run_id === teamRunId);
    assert(bound, "binding report must include the explicit worker run");
    assert.equal(bound.native_session_id, "");
    const boundThreadResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "show", workerThread.id, "--json"], join(root, "target-worktree"));
    const boundThreadReport = lastJson(boundThreadResult.stdout, "bound thread show");
    assert.equal(boundThreadReport.observation.native_session_known, false);
    assert.equal(boundThreadReport.observation.process_liveness, "unknown");
    evidence.add("logical-projects", { target: targetProject.id, unrelated: unrelatedProject.id, workerThread: workerThread.id, runId: teamRunId, nativeSession: "none" });

    const unrelatedWorkspaceDocument = await herdrCommand(evidence, herdr, session, env, ["workspace", "create", "--cwd", join(root, "unrelated-herdr-workspace"), "--label", "Unrelated Herdr", "--no-focus"]);
    const unrelatedWorkspace = resultOf(unrelatedWorkspaceDocument, "unrelated workspace create").workspace;
    ensureString(unrelatedWorkspace?.workspace_id, "unrelated Herdr workspace id");
    const unrelatedSurfaceBefore = await workspaceSnapshot(evidence, herdr, session, env, unrelatedWorkspace.workspace_id);
    const unrelatedProjectBefore = (await forgeCommand(evidence, forgectl, env, root, ["show", unrelatedProject.id, "--json"], join(root, "unrelated-worktree"))).stdout;
    const unrelatedProjectBeforeObject = lastJson(unrelatedProjectBefore, "unrelated project show").project;
    evidence.add("unrelated-baseline", { workspace: stableWorkspaceSnapshot(unrelatedSurfaceBefore), project: unrelatedProjectBeforeObject });

    const projectOpenResult = await forgeCommand(evidence, forgectl, env, root, ["open", targetProject.id, "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    const projectOpen = lastJson(projectOpenResult.stdout, "project open");
    assert.equal(projectOpen.action, "create");
    const projectOperation = projectOpen.operation;
    assertProjectOperation(projectOperation, targetProject.id, session, targetProject.name);
    const projectMarker = ownershipMarker(projectOperation);
    const projectLabel = `${targetProject.name} · ${projectMarker}`;
    const projectRef = ensureRef(projectOperation.ref, "project operation ref");
    const targetSurface = await workspaceSnapshot(evidence, herdr, session, env, projectRef.workspace);
    assert.equal(targetSurface.workspace.label, projectLabel);
    assert.equal(targetSurface.workspace.workspace_id, projectRef.workspace);
    const projectRootPane = await paneSnapshot(evidence, herdr, session, env, projectRef.pane);
    assert.equal(projectRootPane.tab_id, projectRef.tab);
    assert.equal(projectRootPane.workspace_id, projectRef.workspace);
    assert.equal(projectRootPane.terminal_id, projectRef.terminal);
    assert.equal(projectRootPane.display_agent, targetProject.name);
    assert.equal(targetSurface.workspace.tokens?.[PROJECT_TOKEN], targetProject.name);
    evidence.add("project-surface-verified", { operationId: projectOperation.id, marker: projectMarker, ref: projectRef, projectToken: targetSurface.workspace.tokens?.[PROJECT_TOKEN], displayAgent: projectRootPane.display_agent });

    const threadOpenResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "open", workerThread.id, "--view", "timeline", "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    const threadOpen = lastJson(threadOpenResult.stdout, "thread open");
    assert.equal(threadOpen.action, "create-view");
    const threadOperation = threadOpen.operation;
    assertThreadOperation(threadOperation, targetProject.id, workerThread.id, teamRunId, session);
    assert.deepEqual(threadOperation.parent, projectRef);
    const threadMarker = ownershipMarker(threadOperation);
    const threadRef = ensureRef(threadOperation.ref, "thread operation ref");
    assert.equal(threadRef.workspace, projectRef.workspace);
    const afterThreadSurface = await workspaceSnapshot(evidence, herdr, session, env, projectRef.workspace);
    assert.equal(afterThreadSurface.workspace.label, projectLabel);
    assertNoDuplicateMarkers(afterThreadSurface.workspaces, afterThreadSurface.tabs, projectMarker, threadMarker);
    const childTab = afterThreadSurface.tabs.find((tab) => tab.tab_id === threadRef.tab);
    assert(childTab, "thread operation tab must exist");
    assert.equal(childTab.label, `${workerThread.name} · ${threadMarker}`);
    assert.equal(childTab.workspace_id, projectRef.workspace);
    const childPane = await paneSnapshot(evidence, herdr, session, env, threadRef.pane);
    assert.equal(childPane.tab_id, threadRef.tab);
    assert.equal(childPane.workspace_id, projectRef.workspace);
    assert.equal(childPane.terminal_id, threadRef.terminal);
    assert.equal(childPane.display_agent, workerThread.name);
    evidence.add("thread-surface-verified", { operationId: threadOperation.id, marker: threadMarker, ref: threadRef, displayAgent: childPane.display_agent, runId: threadOperation.run_id });

    const focusedProjectResult = await forgeCommand(evidence, forgectl, env, root, ["open", targetProject.id, "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    assert.equal(lastJson(focusedProjectResult.stdout, "project reopen").action, "focused");
    const focusedThreadResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "open", workerThread.id, "--view", "timeline", "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    const focusedThread = lastJson(focusedThreadResult.stdout, "thread reopen");
    assert.equal(focusedThread.action, "focused");
    assert.equal(focusedThread.operation.id, threadOperation.id);
    const afterFocusSurface = await workspaceSnapshot(evidence, herdr, session, env, projectRef.workspace);
    assertNoDuplicateMarkers(afterFocusSurface.workspaces, afterFocusSurface.tabs, projectMarker, threadMarker);
    evidence.add("focus-reopen-no-duplicate", { projectOperationId: projectOperation.id, threadOperationId: threadOperation.id, projectMarker, threadMarker });

    const closeResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "close-view", workerThread.id, "--operation", threadOperation.id, "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    const closed = lastJson(closeResult.stdout, "thread close-view");
    assert.equal(closed.action, "close-view");
    assert.equal(closed.operation.id, threadOperation.id);
    assert.equal(closed.operation.retirement?.phase, "closed");
    const afterCloseSurface = await workspaceSnapshot(evidence, herdr, session, env, projectRef.workspace);
    assert.equal(afterCloseSurface.workspace.label, projectLabel);
    assert.equal(afterCloseSurface.tabs.some((tab) => tab.label === threadMarker || tab.label.endsWith(` · ${threadMarker}`)), false);
    evidence.add("child-view-closed-only", { operationId: threadOperation.id, retirement: "closed", workspacePreserved: true, projectMarker });

    const focusedAfterCloseResult = await forgeCommand(evidence, forgectl, env, root, ["open", targetProject.id, "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    assert.equal(lastJson(focusedAfterCloseResult.stdout, "project reopen after child close").action, "focused");

    const reopenedResult = await forgeCommand(evidence, forgectl, env, root, ["thread", "--project", targetProject.id, "open", workerThread.id, "--view", "timeline", "--session", session, "--herdr-binary", herdr, "--json"], join(root, "target-worktree"));
    const reopened = lastJson(reopenedResult.stdout, "thread reopen after close");
    assert.equal(reopened.action, "create-view");
    assert.notEqual(reopened.operation.id, threadOperation.id);
    assertThreadOperation(reopened.operation, targetProject.id, workerThread.id, teamRunId, session);
    assert.deepEqual(reopened.operation.parent, projectRef);
    const reopenedRef = ensureRef(reopened.operation.ref, "reopened thread operation ref");
    const finalSurface = await workspaceSnapshot(evidence, herdr, session, env, projectRef.workspace);
    assertThreadOperation(reopened.operation, targetProject.id, workerThread.id, teamRunId, session);
    assert.equal(finalSurface.workspace.label, projectLabel);
    assertNoDuplicateMarkers(finalSurface.workspaces, finalSurface.tabs, projectMarker, ownershipMarker(reopened.operation));
    const finalProjectRootPane = await paneSnapshot(evidence, herdr, session, env, projectRef.pane);
    assert.equal(finalProjectRootPane.tab_id, projectRef.tab);
    assert.equal(finalProjectRootPane.workspace_id, projectRef.workspace);
    assert.equal(finalProjectRootPane.terminal_id, projectRef.terminal);
    assert.equal(finalProjectRootPane.display_agent, targetProject.name);
    const reopenedPane = await paneSnapshot(evidence, herdr, session, env, reopenedRef.pane);
    assert.equal(reopenedPane.tab_id, reopenedRef.tab);
    assert.equal(reopenedPane.workspace_id, reopenedRef.workspace);
    assert.equal(reopenedPane.terminal_id, reopenedRef.terminal);
    assert.equal(reopenedPane.display_agent, workerThread.name);
    evidence.add("thread-close-reopen-identity", { oldOperationId: threadOperation.id, newOperationId: reopened.operation.id, threadId: workerThread.id, runId: teamRunId, workspace: projectRef.workspace, childTab: reopenedRef.tab, childPane: reopenedRef.pane, childTerminal: reopenedRef.terminal });

    const renderProbe = fileURLToPath(new URL("./probe-project-render.py", import.meta.url));
    const renderArgs = [renderProbe, "--herdr", herdr, "--session", session, "--expect-project", targetProject.name, "--expect-thread", workerThread.name];
    const rendered = await requireCommand(evidence, "/usr/bin/python3", [...renderArgs, "--output", join(root, "render")], { env, timeout: 60_000 });
    assert.equal(lastJson(rendered.stdout, "render probe").ok, true);
    const wrongLabel = await runCommand(evidence, "/usr/bin/python3", [renderProbe, "--herdr", herdr, "--session", session, "--expect-project", `absent-${randomUUID()}`, "--output", join(root, "render-negative")], { env, timeout: 60_000 });
    assert.equal(wrongLabel.code, 1, "render probe must reject a label absent from the actual screen");
    const negativeReceipt = JSON.parse(await readFile(join(root, "render-negative", "render.json"), "utf8"));
    assert.equal(negativeReceipt.ok, false);
    assert.deepEqual(negativeReceipt.assertions, { project: false });
    assert(negativeReceipt.screen.captured_bytes > 0, "negative label control must observe actual terminal output");
    assert.equal(negativeReceipt.preflight.workspace_count, finalSurface.workspaces.length);
    assert(negativeReceipt.child.exit_code !== null || negativeReceipt.child.signal !== null, "negative label control must reap its client");
    evidence.add("render-status", { status: "verified", wrongLabelRejected: true });

    const unrelatedSurfaceAfter = await workspaceSnapshot(evidence, herdr, session, env, unrelatedWorkspace.workspace_id);
    assertUnrelatedUnchanged(unrelatedSurfaceBefore, unrelatedSurfaceAfter);
    const unrelatedProjectAfterResult = await forgeCommand(evidence, forgectl, env, root, ["show", unrelatedProject.id, "--json"], join(root, "unrelated-worktree"));
    const unrelatedProjectAfter = lastJson(unrelatedProjectAfterResult.stdout, "unrelated project final show").project;
    assert.deepEqual(unrelatedProjectAfter, unrelatedProjectBeforeObject);
    evidence.add("unrelated-state-preserved", { workspace: unrelatedWorkspace.workspace_id, project: unrelatedProject.id });
    evidence.add("acceptance-summary", { session, projectId: targetProject.id, threadId: workerThread.id, runId: teamRunId, workspace: projectRef.workspace, render: "verified", nativeThread: "none", nativeProcessLiveness: "unknown" });
    completed = true;
    summary = { root, session, project: targetProject.id, thread: workerThread.id, runId: teamRunId, render: "verified", status: "verified-presentation" };
  } catch (error) {
    evidence.add("acceptance-failed", { error: String(error) });
    throw error;
  } finally {
    if (server && server.exitCode === null && server.signalCode === null) {
      try {
        await requireCommand(evidence, herdr, herdrArgs(session, ["server", "stop"]), { env, timeout: 20_000 });
      } catch (error) {
        evidence.add("cleanup-failed", { session, error: String(error) });
      }
      await stopOwnedServer(server);
    }
    evidence.add("cleanup", { session, ownedServerPid: server?.pid ?? null, completed, scope: "named-herdr-session-only" });
  }
  process.stdout.write(`${JSON.stringify(summary)}\n`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const args = parseArgs(process.argv.slice(2));
    if (args.help) process.stdout.write(usage);
    else await runAcceptance(args);
  } catch (error) {
    process.stderr.write(`${String(error)}\n`);
    process.exitCode = 1;
  }
}
