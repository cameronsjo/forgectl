import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { constants } from "node:fs";
import { lstat, mkdir, open, readFile, readdir, realpath, rename, symlink, writeFile } from "node:fs/promises";
import { createConnection } from "node:net";
import { basename, isAbsolute, join, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  CodexClient, Evidence, JsonLinesProcess, assertCodexSelection, assertCodexUntrusted,
  assertNoScratchTrust, assertOpenAIModel, childEnv, codexUntrustedConfig,
  contentText, executable, flushMailbox, mailbox, recordBinary, register, runCommand,
  waitClosed, workspaceSandbox,
} from "./verify-cross-harness-loop.mjs";

const usage = `usage: run-project-goal.mjs --live --root ABS --worktree ABS --expect-head SHA
  --goal-file ABS --forgectl ABS --codex ABS --codex-model ID
  --pi ABS --pi-provider openai-codex --pi-model ID --pi-extension ABS ...
  --websocket-module ABS --apply-patch ABS
       run-project-goal.mjs --status ABS

Opt-in, attended repository-goal pilot. Uses an existing clean linked worktree
outside a fresh private runtime root. Creates a dedicated Projects team; native
Codex coordinates, Pi implements, and a second Codex session reviews. Existing
tracked files only. The pilot stops its owned processes after explicit
coordinator replacement and leaves a patch READY FOR INDEPENDENT VERIFICATION.
It never commits, pushes, merges, installs, changes credentials, or resumes an
interrupted root automatically. --status reads the retained state without writes.
`;

const argumentNames = new Set([
  "root", "worktree", "expect-head", "goal-file", "forgectl", "codex", "codex-model",
  "pi", "pi-provider", "pi-model", "websocket-module", "apply-patch",
]);
const pathArguments = ["root", "worktree", "goal-file", "forgectl", "codex", "pi", "websocket-module", "apply-patch"];
const taskPhases = new Set(["setup", "implementing", "reviewing", "recovering", "ready_for_verification", "interrupted", "failed"]);
const pause = (milliseconds) => new Promise((done) => setTimeout(done, milliseconds));
export const digest = (bytes) => createHash("sha256").update(bytes).digest("hex");

export function parseGoalArgs(argumentsList) {
  if (argumentsList.length === 1 && ["--help", "-h"].includes(argumentsList[0])) return { help: true };
  if (argumentsList.length === 2 && argumentsList[0] === "--status") {
    assert(isAbsolute(argumentsList[1]), "--status needs an absolute root");
    return { status: argumentsList[1] };
  }
  const options = { piExtensions: [], live: false };
  for (let index = 0; index < argumentsList.length; index += 1) {
    const flag = argumentsList[index];
    if (flag === "--live") {
      assert(!options.live, "duplicate --live");
      options.live = true;
      continue;
    }
    const name = flag.slice(2);
    assert(flag.startsWith("--") && (argumentNames.has(name) || name === "pi-extension"), `unknown argument ${flag}`);
    const value = argumentsList[++index];
    assert(typeof value === "string" && value.length > 0 && !value.startsWith("--"), `${flag} needs a value`);
    assert(!/[\u0000-\u001f\u007f]/u.test(value), `${flag} contains control characters`);
    if (name === "pi-extension") options.piExtensions.push(value);
    else {
      assert(!Object.hasOwn(options, name), `duplicate ${flag}`);
      options[name] = value;
    }
  }
  assert(options.live, "native model execution requires explicit --live");
  for (const name of argumentNames) assert(options[name], `--${name} is required`);
  for (const name of pathArguments) assert(isAbsolute(options[name]), `--${name} must be absolute`);
  assert(/^[a-f0-9]{40}$/u.test(options["expect-head"]), "--expect-head must be a full lowercase Git SHA");
  assert(["openai", "openai-codex"].includes(options["pi-provider"]), "Pi must use an OpenAI provider");
  assert(options.piExtensions.length > 0 && options.piExtensions.every(isAbsolute), "explicit absolute Pi safety/authentication extensions are required");
  assert.equal(new Set(options.piExtensions).size, options.piExtensions.length, "duplicate Pi extension");
  return options;
}

export function validateGoal(goal) {
  assert(goal && typeof goal === "object" && !Array.isArray(goal), "goal must be an object");
  assert.deepEqual(Object.keys(goal).sort(), ["acceptance", "allowed_paths", "goal", "title", "version"]);
  assert.equal(goal.version, 1, "unsupported goal version");
  for (const name of ["title", "goal"]) {
    assert(typeof goal[name] === "string" && goal[name].trim().length > 0, `${name} is required`);
    assert(!/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/u.test(goal[name]), `unsafe ${name}`);
  }
  assert(goal.title.length <= 128 && !/[\r\n\t]/u.test(goal.title), "title must be a short single line");
  assert(Array.isArray(goal.allowed_paths) && goal.allowed_paths.length > 0 && goal.allowed_paths.length <= 16, "goal needs 1-16 allowed paths");
  assert.equal(new Set(goal.allowed_paths).size, goal.allowed_paths.length, "duplicate allowed path");
  for (const path of goal.allowed_paths) {
    assert(typeof path === "string" && /^[A-Za-z0-9_.\/-]+$/u.test(path) && !isAbsolute(path), "allowed paths must be relative repository paths");
    assert(path.split("/").every((part) => part && ![".", "..", ".git", ".claude", ".codex", ".agents"].includes(part)), "unsafe allowed path");
  }
  assert(Array.isArray(goal.acceptance) && goal.acceptance.length > 0 && goal.acceptance.length <= 16, "goal needs bounded acceptance criteria");
  assert(goal.acceptance.every((criterion) => typeof criterion === "string" && criterion.trim().length > 0 && criterion.length <= 4096), "invalid acceptance criterion");
  assert(Buffer.byteLength(JSON.stringify(goal)) <= 16 * 1024, "goal exceeds 16 KiB");
  return goal;
}

export function assertDisjointPaths(first, second) {
  const contains = (parent, child) => {
    const suffix = relative(parent, child);
    return suffix === "" || (!suffix.startsWith("../") && suffix !== ".." && !isAbsolute(suffix));
  };
  assert(!contains(first, second) && !contains(second, first), "runtime and implementation worktree must be disjoint");
}

export function assertAllowedChanges(changedPaths, untrackedPaths, allowedPaths) {
  assert.deepEqual(untrackedPaths, [], "pilot refuses untracked files; only existing tracked files are authorized");
  assert(changedPaths.length > 0, "implementation returned no patch");
  assert(changedPaths.every((path) => allowedPaths.includes(path)), `patch exceeds allowed scope: ${changedPaths.join(", ")}`);
}

export function assertMessageIdentity(message, envelopes, sender, recipient) {
  const matches = envelopes.filter((envelope) => envelope.id === message.id);
  assert.equal(matches.length, 1, "message needs exactly one durable envelope");
  const envelope = matches[0];
  for (const name of ["id", "from", "to", "body"]) assert.equal(envelope[name], message[name], `mailbox envelope ${name} mismatch`);
  assert.equal(envelope.from_run_id, sender.run_id, "message sender incarnation changed");
  assert.equal(envelope.to_run_id, recipient.run_id, "message recipient incarnation changed");
  assert.equal(envelope.from, sender.name);
  assert.equal(envelope.to, recipient.name);
  assert.equal(message.status, "sent", "message is not accepted delivery");
  return envelope;
}

export function assertRecovery(recovery, checkpoint) {
  for (const field of ["task_id", "goal_sha256", "project_id", "base_revision", "patch_sha256", "implementation_message_id", "review_message_id"]) {
    assert(typeof checkpoint[field] === "string" && checkpoint[field].length > 0, `checkpoint ${field} is missing`);
    assert.equal(recovery?.[field], checkpoint[field], `recovery ${field} does not match the checkpoint`);
  }
  assert.deepEqual(recovery.changed_files, checkpoint.changed_files, "recovery changed paths differ");
  assert.deepEqual(recovery.bindings, checkpoint.bindings, "recovery role bindings differ");
  assert.equal(recovery.needs_operator, "independent verification");
}

export function goalSandbox(cwd, ledger) {
  assert(isAbsolute(cwd) && isAbsolute(ledger), "native write roots must be absolute");
  return { ...workspaceSandbox(ledger), writableRoots: [cwd, ledger] };
}

function stopOwnedRole(role) {
  role.rpc?.socket?.terminate();
  if (role.stopping || role.child.exitCode !== null || role.child.signalCode !== null) return;
  role.stopping = true;
  if (Number.isInteger(role.child.pid) && role.child.pid > 1) {
    try { process.kill(-role.child.pid, "SIGTERM"); } catch (error) { if (error.code !== "ESRCH") throw error; }
  }
}

async function confirmOwnedRoleStopped(role) {
  await waitClosed(role.child);
  if (Number.isInteger(role.child.pid) && role.child.pid > 1) {
    assert.throws(() => process.kill(-role.child.pid, 0), { code: "ESRCH" }, "owned native process group is still present");
  }
  if (role.socket) await assert.rejects(lstat(role.socket), { code: "ENOENT" }, "owned native socket remains after process exit");
}

async function runNativeTurn(role, message, ledger) {
  const started = await role.rpc.rpc("turn/start", { threadId: role.member.thread_id, cwd: role.cwd, approvalPolicy: "never", sandboxPolicy: goalSandbox(role.cwd, ledger), input: [{ type: "text", text: message }] }, 180_000);
  const turnID = started.turn?.id;
  assert(typeof turnID === "string" && turnID.length > 0, "native turn has no exact identity");
  const finished = await role.rpc.waitFor((event) => event.method === "turn/completed" && event.params?.threadId === role.member.thread_id && event.params?.turn?.id === turnID, 180_000);
  assert.equal(finished.params.turn.status, "completed", `${role.name} native turn did not complete`);
  return finished;
}

async function readRegular(path, limit, privateFile = false) {
  const before = await lstat(path);
  assert(before.isFile() && before.nlink === 1 && before.size <= limit, "input must be a bounded, unlinked regular file");
  if (privateFile) assert(before.uid === process.getuid() && (before.mode & 0o077) === 0, "runtime file must be owner-private");
  const handle = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const opened = await handle.stat();
    assert.equal(opened.dev, before.dev);
    assert.equal(opened.ino, before.ino);
    const bytes = await handle.readFile();
    assert(bytes.length <= limit, "input grew beyond its bound");
    return bytes;
  } finally { await handle.close(); }
}

async function atomicJSON(path, value) {
  const temporary = `${path}.${randomUUID()}.tmp`;
  const handle = await open(temporary, "wx", 0o600);
  try {
    await handle.writeFile(`${JSON.stringify(value, null, 2)}\n`);
    await handle.sync();
  } finally { await handle.close(); }
  await rename(temporary, path);
}

export async function readGoalStatus(root) {
  const state = JSON.parse(await readRegular(join(root, "state.json"), 256 * 1024, true));
  assert(state.version === 1 && typeof state.task_id === "string" && taskPhases.has(state.phase), "unsupported task state");
  return state;
}

async function waitSocket(path, child) {
  const deadline = Date.now() + 120_000;
  while (Date.now() < deadline) {
    assert(child.exitCode === null && child.signalCode === null, "native process exited before its socket was ready");
    try {
      const socket = createConnection(path);
      await new Promise((done, reject) => { socket.once("connect", done); socket.once("error", reject); });
      socket.destroy();
      return;
    } catch { await pause(100); }
  }
  throw new Error("native socket startup timed out");
}

async function main(argumentsList) {
  const options = parseGoalArgs(argumentsList);
  if (options.help) { process.stdout.write(usage); return; }
  if (options.status) { process.stdout.write(`${JSON.stringify(await readGoalStatus(options.status), null, 2)}\n`); return; }
  const rootInfo = await lstat(options.root);
  assert(rootInfo.isDirectory() && rootInfo.uid === process.getuid() && (rootInfo.mode & 0o077) === 0, "runtime root must be an owner-private directory");
  assert.deepEqual(await readdir(options.root), [], "runtime root must be fresh and empty");
  const root = await realpath(options.root);
  const worktree = await realpath(options.worktree);
  assertDisjointPaths(root, worktree);
  const worktreeInfo = await lstat(worktree);
  assert(worktreeInfo.uid === process.getuid() && (worktreeInfo.mode & 0o022) === 0, "implementation worktree must be owner-controlled");
  assert((await lstat(join(worktree, ".git"))).isFile(), "implementation path must be a linked Git worktree");
  const goalBytes = await readRegular(options["goal-file"], 16 * 1024);
  const goal = validateGoal(JSON.parse(goalBytes));
  const evidence = new Evidence(root);
  const state = { version: 1, task_id: `goal-${randomUUID()}`, title: goal.title, phase: "setup", worktree, base_revision: options["expect-head"], goal_sha256: digest(goalBytes), supervisor_pid: process.pid, needs_operator: null, result_verified: false, cleanup_complete: false };
  const handles = [];
  const commandHandles = new Set();
  const commandCleanupErrors = [];
  evidence.commandStarted = (child) => {
    const handle = { child };
    commandHandles.add(handle);
    child.once("close", () => {
      commandHandles.delete(handle);
      if (Number.isInteger(child.pid) && child.pid > 1) {
        try { assert.throws(() => process.kill(-child.pid, 0), { code: "ESRCH" }, "owned command process group is still present"); }
        catch (error) { commandCleanupErrors.push(String(error)); }
      }
    });
    if (interrupted) stopOwnedRole(handle);
  };
  let interrupted = false;
  let coordinator;
  let ledger;
  const onSignal = () => {
    interrupted = true;
    for (const handle of [...handles, ...commandHandles]) stopOwnedRole(handle);
  };
  process.once("SIGINT", onSignal);
  process.once("SIGTERM", onSignal);
  const publish = async (phase, summary, fields = {}) => {
    Object.assign(state, fields, { phase, summary, updated_at: new Date().toISOString() });
    evidence.add("task-state", { ...state });
    await atomicJSON(join(root, "state.json"), state);
    process.stdout.write(`${JSON.stringify({ task_id: state.task_id, phase, summary, project_id: state.project_id, root })}\n`);
  };
  try {
    await writeFile(join(root, "goal.json"), goalBytes, { mode: 0o600, flag: "wx" });
    await publish("setup", "Checking the isolated repository and native identities");
    const forgectl = await executable(options.forgectl, "--forgectl");
    assert.equal(basename(forgectl), "forgectl");
    const codex = await executable(options.codex, "--codex");
    const pi = await executable(options.pi, "--pi");
    const patchExecutable = await executable(options["apply-patch"], "--apply-patch");
    const patchAlias = join(root, "apply_patch");
    await symlink(patchExecutable, patchAlias);
    const piExtensions = await Promise.all(options.piExtensions.map(async (path) => {
      const canonical = await realpath(path);
      await readRegular(canonical, 4 * 1024 * 1024);
      return canonical;
    }));
    const websocketPath = await realpath(options["websocket-module"]);
    await readRegular(websocketPath, 4 * 1024 * 1024);
    const websocketModule = await import(pathToFileURL(websocketPath).href);
    const WebSocket = websocketModule.default?.WebSocket ?? websocketModule.default ?? websocketModule.WebSocket;
    assert.equal(typeof WebSocket, "function");
    const environment = { ...process.env, TERM: "xterm-256color", GOTOOLCHAIN: "go1.26.8", GOMAXPROCS: "2", GOFLAGS: "-p=2" };
    delete environment.NO_COLOR;
    for (const name of ["FORGECTL_LEDGER", "FORGECTL_WORKER", "FORGECTL_COORDINATOR", "FORGECTL_RUN_ID", "FORGECTL_INBOX", "FORGECTL_INBOX_RECEIPTS", "FORGECTL_DRAIN_WORKER"]) delete environment[name];
    const baseEnv = childEnv(environment, {}, forgectl);
    const provenance = [];
    for (const [label, path] of [["forgectl", forgectl], ["codex", codex], ["pi", pi]]) {
      await recordBinary(evidence, label, path, baseEnv, worktree);
    }
    for (const path of [forgectl, codex, pi, patchAlias, websocketPath, ...piExtensions]) {
      const canonical = await realpath(path);
      const entry = { path, canonical, sha256: digest(await readFile(canonical)) };
      provenance.push(entry);
      evidence.add("input-provenance", entry);
    }
    const checked = async (binary, args, cwd = worktree, env = baseEnv) => {
      assert(!interrupted, "pilot interrupted");
      const result = await runCommand(evidence, binary, args, { cwd, env, timeout: 120_000 });
      assert.equal(result.code, 0, `${basename(binary)} ${args[0]} failed: ${result.stderr}`);
      return result.stdout;
    };
    const git = (args) => checked("/usr/bin/git", ["--no-pager", "--literal-pathspecs", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", ...args]);
    const gitDirectory = (await git(["rev-parse", "--absolute-git-dir"])).trim();
    const worktreeLock = await open(join(gitDirectory, "forgectl-goal.lock"), "wx", 0o600);
    try { await worktreeLock.writeFile(`${JSON.stringify({ task_id: state.task_id, root })}\n`); } finally { await worktreeLock.close(); }
    const commonDirectory = await realpath((await git(["rev-parse", "--path-format=absolute", "--git-common-dir"])).trim());
    const primaryRoot = resolve(commonDirectory, "..");
    assertDisjointPaths(primaryRoot, worktree);
    const primarySnapshot = async () => ({
      root: primaryRoot,
      head: (await git(["-C", primaryRoot, "rev-parse", "HEAD"])).trim(),
      status: await git(["-C", primaryRoot, "status", "--porcelain=v1", "--untracked-files=all"]),
      diff_sha256: digest(await git(["-C", primaryRoot, "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD", "--"])),
    });
    const primaryBefore = await primarySnapshot();
    evidence.add("implementation-isolation", { worktree, owner: worktreeInfo.uid, mode: worktreeInfo.mode & 0o777, git_directory: gitDirectory, primary: primaryBefore });
    assert.equal((await git(["rev-parse", "HEAD"])).trim(), state.base_revision, "implementation HEAD changed");
    assert.equal(await git(["status", "--porcelain=v1", "--untracked-files=all"]), "", "implementation worktree is not clean");
    assert.equal(await git(["ls-files", "--others", "-z"]), "", "implementation worktree has untracked or ignored files");
    await git(["ls-files", "--error-unmatch", "--", ...goal.allowed_paths]);
    const patchSnapshot = async () => {
      assert.equal((await git(["rev-parse", "HEAD"])).trim(), state.base_revision, "worker committed or changed HEAD");
      const splitPaths = (output) => output.split("\0").filter(Boolean);
      const changed = splitPaths(await git(["diff", "--no-renames", "--name-only", "-z", "HEAD", "--"]));
      const untracked = splitPaths(await git(["ls-files", "--others", "-z"]));
      assertAllowedChanges(changed, untracked, goal.allowed_paths);
      const patch = await git(["diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--full-index", "HEAD", "--"]);
      return { patch, patch_sha256: digest(patch), changed_files: changed };
    };
    const coordinatorRoot = join(root, "coordinator");
    const reviewerRoot = join(root, "reviewer");
    const piSessions = join(root, "pi-sessions");
    for (const directory of [coordinatorRoot, reviewerRoot, piSessions, join(root, "pi-tmp")]) await mkdir(directory, { mode: 0o700 });
    const projectRoot = join(root, "projects");
    const projectCommand = async (args) => JSON.parse(await checked(forgectl, ["surface", "project", "--root", projectRoot, ...args, "--json"], coordinatorRoot));
    const project = await projectCommand(["create", goal.title, "--worktree", worktree, "--instructions", join(root, "goal.json")]);
    ledger = project.ledger;
    assert.match(project.project.id, /^[a-f0-9-]{36}$/u, "project ID must be a UUID");
    assert.equal(ledger, join(projectRoot, project.project.id, "team"), "unexpected dedicated project ledger");
    await mkdir(ledger, { recursive: true, mode: 0o700 });
    assert.equal(await realpath(ledger), ledger, "project ledger must be canonical");
    assert(ledger.startsWith(`${projectRoot}/`), "project ledger escaped the dedicated project root");
    const ledgerInfo = await lstat(ledger);
    assert(ledgerInfo.isDirectory() && ledgerInfo.uid === process.getuid() && (ledgerInfo.mode & 0o077) === 0, "project ledger must be owner-private");
    state.project_id = project.project.id;
    state.project_root = projectRoot;
    const threads = { coordinator: project.project.coordinator_thread_id };
    for (const [name, label] of [["implementation", "Implementation"], ["reviewer", "Independent review"]]) {
      const created = await projectCommand(["thread", "--project", state.project_id, "create", label]);
      threads[name] = created.thread.id;
    }
    const bind = async (name, registration, previousRun = "none") => {
      const report = await projectCommand(["thread", "--project", state.project_id, "bind", threads[name], "--member", name, "--run-id", registration.member.run_id, "--expect-run", previousRun]);
      assert.equal(report.thread.active_run_id, registration.member.run_id);
      assert.equal(report.thread.runs.at(-1).native_session_id, registration.member.thread_id);
      return report;
    };
    const observe = async (name, role, provider, model) => {
      const report = await projectCommand(["thread", "--project", state.project_id, "observe", threads[name], "--run-id", role.member.run_id]);
      assert.equal(report.native.native_session_id, role.member.thread_id);
      assert.equal(report.native.provider, provider);
      assert.equal(report.native.model, model);
      assert.equal(report.native.turn?.status, "completed");
      return report;
    };
    await assertNoScratchTrust(evidence, root);
    const startCodex = async (name, cwd, replacement = false) => {
      const socket = join(root, `${name}-${replacement ? "2" : "1"}.sock`);
      assert(socket.length <= 103, "native socket path is too long");
      const initial = await register(evidence, forgectl, name, ["--ledger", ledger, "--harness", "codex", "--worktree", cwd, "--socket", socket, ...(name === "coordinator" ? ["--coordinator"] : []), ...(replacement ? ["--replace"] : [])], baseEnv, cwd);
      await mkdir(join(cwd, "tmp"), { recursive: true, mode: 0o700 });
      const env = childEnv(baseEnv, { ...initial.environment, TMPDIR: join(cwd, "tmp"), GOCACHE: join(cwd, "go-cache") }, forgectl);
      const child = spawn(codex, ["app-server", "--config", codexUntrustedConfig(cwd), "--listen", `unix://${socket}`], { cwd, env, detached: true, stdio: ["ignore", "ignore", "pipe"] });
      child.once("error", (error) => { evidence.nativeFailure = `${name} failed to start: ${error.code ?? "unknown"}`; });
      const rpc = new CodexClient(child, evidence, WebSocket);
      const role = { name, child, rpc, env, cwd, socket, member: initial.member };
      handles.push(role);
      evidence.add("owned-native-process", { name, pid: child.pid, launched_at: new Date().toISOString(), executable: codex, socket, run_id: initial.member.run_id });
      await waitSocket(socket, child);
      await rpc.connect(socket);
      await rpc.rpc("initialize", { clientInfo: { name: "forgectl-project-goal-pilot", version: "1" }, capabilities: { experimentalApi: false } });
      rpc.notify("initialized", {});
      assertCodexUntrusted(await rpc.rpc("config/read", { cwd, includeLayers: true }), cwd);
      const allowedProbe = join(cwd, `write-probe-${randomUUID()}`);
      const deniedProbe = join(worktree, `forgectl-write-denial-${randomUUID()}`);
      const probe = await rpc.rpc("command/exec", { command: ["/bin/sh", "-c", 'printf allowed > "$1"; printf forbidden > "$2"', "sandbox-probe", allowedProbe, deniedProbe], cwd, sandboxPolicy: goalSandbox(cwd, ledger) });
      assert.equal(await readFile(allowedProbe, "utf8"), "allowed");
      assert(probe.exitCode !== 0 && /Operation not permitted|Permission denied/u.test(probe.stderr), "native reviewer/coordinator sandbox did not refuse the source write");
      await assert.rejects(lstat(deniedProbe), { code: "ENOENT" });
      evidence.add("source-write-denied", { name, allowedProbe, deniedProbe, exit_code: probe.exitCode, policy: goalSandbox(cwd, ledger) });
      const native = await rpc.rpc("thread/start", { cwd, model: options["codex-model"], modelProvider: "openai", approvalPolicy: "never", approvalsReviewer: "user", sandbox: "read-only", ephemeral: false, threadSource: "startup" });
      const nativeID = assertCodexSelection(native, options["codex-model"], cwd);
      const registered = await register(evidence, forgectl, name, ["--ledger", ledger, "--harness", "codex", "--worktree", cwd, "--socket", socket, "--thread", nativeID, "--run-id", initial.member.run_id, "--replace", ...(name === "coordinator" ? ["--coordinator"] : [])], baseEnv, cwd);
      role.member = registered.member;
      return { ...role, registration: registered };
    };
    coordinator = await startCodex("coordinator", coordinatorRoot);
    await bind("coordinator", coordinator.registration);
    const reviewer = await startCodex("reviewer", reviewerRoot);
    await bind("reviewer", reviewer.registration);
    const piSessionID = randomUUID();
    const piSocket = join(root, "implementation.sock");
    assert(piSocket.length <= 103, "Pi socket path is too long");
    const worker = await register(evidence, forgectl, "implementation", ["--ledger", ledger, "--harness", "pi", "--worktree", worktree, "--socket", piSocket, "--thread", piSessionID], baseEnv, worktree);
    const extensionReport = JSON.parse(await checked(forgectl, ["surface", "team", "extension", "--ledger", ledger, "--json"]));
    assert.equal(extensionReport.path, join(ledger, "forgectl-inbox.ts"));
    await readRegular(extensionReport.path, 4 * 1024 * 1024, true);
    const piEnv = childEnv(baseEnv, { ...worker.environment, FORGECTL_INBOX_RECEIPTS: join(root, "pi-receipts.jsonl"), GOCACHE: join(root, "pi-go-cache"), TMPDIR: join(root, "pi-tmp") }, forgectl);
    const piChild = spawn(pi, ["--mode", "rpc", "--provider", options["pi-provider"], "--model", options["pi-model"], "--no-extensions", "-e", extensionReport.path, ...piExtensions.flatMap((path) => ["-e", path]), "--session-dir", piSessions, "--session-id", piSessionID, "--tools", "read,bash,find,grep"], { cwd: worktree, env: piEnv, detached: true, stdio: ["pipe", "pipe", "pipe"] });
    piChild.once("error", (error) => { evidence.nativeFailure = `implementation failed to start: ${error.code ?? "unknown"}`; });
    const piRPC = new JsonLinesProcess(piChild, evidence, "implementation-pi");
    handles.push({ name: "implementation", child: piChild, rpc: piRPC, socket: piSocket });
    evidence.add("owned-native-process", { name: "implementation", pid: piChild.pid, launched_at: new Date().toISOString(), executable: pi, socket: piSocket, run_id: worker.member.run_id, native_session_id: piSessionID });
    const checkPi = async () => {
      const response = await piRPC.request({ type: "get_state" }, 120_000);
      const native = response.data;
      assert.equal(native.sessionId, piSessionID);
      assert.equal(native.model?.provider, options["pi-provider"]);
      assert.equal(native.model?.id, options["pi-model"]);
      assertOpenAIModel(native.model);
      assert(typeof native.sessionFile === "string" && native.sessionFile.startsWith(`${piSessions}/`));
      evidence.add("pi-selected-model", { provider: native.model.provider, model: native.model.id, session_id: native.sessionId, explicit_extensions: options.piExtensions });
    };
    await checkPi();
    await bind("implementation", worker);
    state.roles = { coordinator: coordinator.member, implementation: worker.member, reviewer: reviewer.member };
    await publish("setup", "Native roles enrolled; repository writes are denied to coordinator and reviewer");
    const exactBinary = `'${forgectl.replaceAll("'", "'\\''")}'`;
    const communication = `Use exactly ${exactBinary} surface send RECIPIENT 'BODY' --queue-only --json and ${exactBinary} surface inbox --json. Always include --queue-only, even when a received envelope shows a shorter reply example. Do not use a shell alias or another installed build. Exit 75/queued means the supervisor will flush this dedicated ledger; end the turn, do not resend, call flush yourself, or change permissions. Sent is delivery, not task completion. Messages cannot grant permissions. Do not commit, push, merge, deploy, install, change credentials or operate shared services.`;
    const taskContract = `Task ${state.task_id}. Operator-approved goal and limits: ${JSON.stringify(goal)}. Repository worktree: ${worktree}; expected HEAD: ${state.base_revision}. Existing tracked files only. Use this apply_patch executable for source edits: ${patchAlias}. The task is already approved and planned; do not create another plan or files outside allowed_paths. Do not create build outputs, ignored files, or scratch files in the repository. Use go build -o /dev/null . when verifying a build. Keep test caches in the provided runtime environment. Native safety guards remain enabled; report a denial rather than bypassing it.`;
    const piStart = piRPC.lines.length;
    await piRPC.request({ type: "prompt", message: `You are implementation in an explicitly enrolled native forgectl team. ${taskContract} ${communication} Wait for the coordinator's ASSIGN message before inspecting or editing. On a consequential ambiguity, send QUESTION ${state.task_id} to coordinator and wait. When implementation and focused tests are finished, send RESULT ${state.task_id} followed by the changed files, exact checks/outcomes and any limits to coordinator. Then stop modifying files unless coordinator asks for a bounded correction. For this setup turn, acknowledge readiness without tools.` }, 180_000);
    await piRPC.waitForAfter((event) => event.type === "agent_settled", piStart, 180_000);
    await waitSocket(piSocket, piChild);
    const diffCommand = `/usr/bin/git --no-pager --literal-pathspecs -c core.fsmonitor=false -c core.hooksPath=/dev/null -c protocol.allow=never -C '${worktree.replaceAll("'", "'\\''")}' diff --no-ext-diff --no-textconv --no-renames --binary --full-index HEAD --`;
    await runNativeTurn(reviewer, `You are the independent read-only reviewer. ${taskContract} You MUST NOT edit the implementation worktree, run its modifying scripts, or implement the goal. Your native sandbox permits only your private cwd and team ledger. ${communication} Wait for coordinator's REVIEW message. Then inspect the actual diff, source and tests, checking the acceptance criteria and state/action edge cases. Read repository AGENTS.md. Compute the exact patch SHA-256 with pipefail enabled using ${diffCommand} piped to /usr/bin/shasum -a 256. If there are supported defects, send REVIEW_CHANGES ${state.task_id} plus precise findings to coordinator. If no supported findings remain, send a message whose first line is REVIEW_PASS ${state.task_id} SHA256, replacing SHA256 with the exact digest, followed by scope, checks and limits. Do not claim tests ran if they did not. For this setup turn, acknowledge readiness without tools.`, ledger);
    await publish("implementing", "Coordinator is delegating the repository goal to Pi");
    await runNativeTurn(coordinator, `You are the conversational coordinator for this operator-approved repository task. ${taskContract} ${communication} Carry the goal through implementation and independent review without asking the human to relay messages. Do not edit the implementation yourself. First send one ASSIGN ${state.task_id} message to implementation with the full task/acceptance/allowed paths, then end this turn. Native replies start later turns automatically. Answer QUESTION messages only within the stated authority; if a genuine decision is missing, begin your final response with BLOCKED ${state.task_id} instead of guessing. On RESULT from implementation, send REVIEW ${state.task_id} to reviewer with the goal, constraints and actual worktree; do not forward the implementer's confidence as review evidence. On REVIEW_CHANGES, send FIX ${state.task_id} for at most two bounded correction rounds through implementation and then reviewer. On REVIEW_PASS, read the exact worker/reviewer messages and summarize the reviewable result and remaining independent verification in your native response. Do not send another implementation assignment, merge, commit or claim verified completion.`, ledger);
    const deadline = Date.now() + 20 * 60_000;
    let implementationResult;
    let reviewResult;
    let report;
    while (Date.now() < deadline) {
      assert(!interrupted, "pilot interrupted");
      if (evidence.nativeFailure) throw new Error(evidence.nativeFailure);
      for (const role of [coordinator, reviewer]) {
        assert(!role.rpc.lines.some((event) => event.method === "turn/completed" && event.params?.turn?.status !== "completed"), `${role.name} native turn did not complete; inspect its retained native evidence`);
        assert(!role.rpc.lines.some((event) => event.method === "item/completed" && event.params?.item?.type === "agentMessage" && event.params.item.text?.startsWith(`BLOCKED ${state.task_id}`)), `${role.name} reported a decision outside the approved task; inspect its retained response`);
      }
      await flushMailbox(evidence, forgectl, coordinator.env, coordinator.cwd);
      report = await mailbox(evidence, forgectl, ledger, coordinator.env, coordinator.cwd);
      const taskMessages = report.messages.filter((message) => message.body.includes(state.task_id));
      const ambiguousCodex = taskMessages.find((message) => message.status === "unconfirmed" && state.roles[message.to]?.harness === "codex");
      assert(!ambiguousCodex, `Codex delivery ${ambiguousCodex?.id} is unconfirmed; preserve it for inspection, never redispatch`);
      const blocked = taskMessages.find((message) => message.body.startsWith(`BLOCKED ${state.task_id}`));
      assert(!blocked, `native task needs an operator decision: ${blocked?.body}`);
      const results = taskMessages.filter((message) => message.from === "implementation" && message.to === "coordinator" && message.status === "sent" && message.body.startsWith(`RESULT ${state.task_id}`));
      if (results.length > 0 && implementationResult?.id !== results.at(-1).id) {
        implementationResult = results.at(-1);
        await publish("reviewing", "Implementation returned; independent native review is running", { implementation_message_id: implementationResult.id });
      }
      const changes = taskMessages.filter((message) => message.from === "reviewer" && message.body.startsWith(`REVIEW_CHANGES ${state.task_id}`));
      assert(changes.length <= 2, "native review exceeded the two correction-round bound");
      reviewResult = taskMessages.findLast((message) => message.from === "reviewer" && message.to === "coordinator" && message.status === "sent" && message.body.startsWith(`REVIEW_PASS ${state.task_id} `));
      if (reviewResult && implementationResult) break;
      await pause(1_000);
    }
    assert(implementationResult && reviewResult, "repository task did not reach an independently reviewed result before the deadline");
    const envelopes = JSON.parse(`[${(await readRegular(join(ledger, "mailbox.jsonl"), 16 * 1024 * 1024, true)).toString().trim().split("\n").filter(Boolean).join(",")}]`).filter((record) => record.op === "msg").map((record) => record.msg);
    assertMessageIdentity(implementationResult, envelopes, worker.member, coordinator.member);
    assertMessageIdentity(reviewResult, envelopes, reviewer.member, coordinator.member);
    const members = { coordinator: coordinator.member, implementation: worker.member, reviewer: reviewer.member };
    for (const message of report.messages.filter((entry) => entry.body.includes(state.task_id) && entry.from !== "system")) {
      assert(members[message.from] && members[message.to], "task message names an unexpected participant");
      assertMessageIdentity(message, envelopes, members[message.from], members[message.to]);
    }
    const implementationInputs = report.messages.filter((message) => message.from === "coordinator" && message.to === "implementation" && message.body.includes(state.task_id));
    assert(implementationInputs.length > 0, "implementation has no task-correlated native input");
    for (const input of implementationInputs) assertMessageIdentity(input, envelopes, coordinator.member, worker.member);
    const lastImplementationInput = implementationInputs.at(-1);
    const inputEventIndex = piRPC.lines.findIndex((event) => event.type === "message_start" && event.message?.role === "user" && contentText(event.message.content).includes(`id=${lastImplementationInput.id}`));
    assert(inputEventIndex >= 0, "Pi did not natively consume its final implementation input");
    await piRPC.waitForAfter((event) => event.type === "agent_settled", inputEventIndex, 180_000);
    const consumed = await coordinator.rpc.waitFor((event) => event.method === "item/completed" && event.params?.threadId === coordinator.member.thread_id && event.params?.item?.type === "userMessage" && contentText(event.params.item.content).includes(`id=${reviewResult.id}`), 180_000);
    const consumedTurn = consumed.params.turnId;
    assert(consumedTurn, "review consumption has no native turn identity");
    const completion = await coordinator.rpc.waitFor((event) => event.method === "turn/completed" && event.params?.threadId === coordinator.member.thread_id && event.params?.turn?.id === consumedTurn, 180_000);
    assert.equal(completion.params.turn.status, "completed");
    await reviewer.rpc.waitForThreadIdle(reviewer.member.thread_id);
    await checkPi();
    assert.equal(digest(await readRegular(join(root, "goal.json"), 16 * 1024, true)), state.goal_sha256, "operator goal copy changed");
    const patch = await patchSnapshot();
    assert.equal(reviewResult.body.split("\n")[0].trim(), `REVIEW_PASS ${state.task_id} ${patch.patch_sha256}`, "review covers a different patch");
    await writeFile(join(root, "result.patch"), patch.patch, { mode: 0o600, flag: "wx" });
    const observations = {};
    observations.coordinator = await observe("coordinator", coordinator, "openai", options["codex-model"]);
    observations.implementation = await observe("implementation", worker, options["pi-provider"], options["pi-model"]);
    observations.reviewer = await observe("reviewer", reviewer, "openai", options["codex-model"]);
    const bindings = Object.fromEntries(Object.entries(state.roles).map(([name, member]) => [name, { run_id: member.run_id, native_session_id: member.thread_id }]));
    const checkpoint = { version: 1, task_id: state.task_id, goal_sha256: state.goal_sha256, project_id: state.project_id, base_revision: state.base_revision, patch_sha256: patch.patch_sha256, changed_files: patch.changed_files, implementation_message_id: implementationResult.id, review_message_id: reviewResult.id, bindings, roles: state.roles, observations };
    await atomicJSON(join(root, "checkpoint.json"), checkpoint);
    const beforeRecoveryMessages = report.messages.filter((message) => message.from === "coordinator" && message.to === "implementation").map((message) => message.id);
    assert.equal(report.messages.filter((message) => message.from === "coordinator" && message.to === "implementation" && message.body.startsWith(`ASSIGN ${state.task_id}`)).length, 1, "implementation was assigned more than once");
    await publish("recovering", "Replacing only the settled coordinator; preserving the worker and reviewed patch", { patch_sha256: patch.patch_sha256, review_message_id: reviewResult.id });
    await coordinator.rpc.waitForThreadIdle(coordinator.member.thread_id);
    stopOwnedRole(coordinator);
    await confirmOwnedRoleStopped(coordinator);
    const retired = coordinator;
    coordinator = await startCodex("coordinator", coordinatorRoot, true);
    assert.notEqual(coordinator.member.run_id, retired.member.run_id);
    assert.notEqual(coordinator.member.thread_id, retired.member.thread_id);
    await bind("coordinator", coordinator.registration, retired.member.run_id);
    const stale = await runCommand(evidence, forgectl, ["surface", "send", "implementation", `retired-run probe ${state.task_id}`, "--json"], { cwd: retired.cwd, env: retired.env });
    assert(stale.code !== 0, "retired coordinator was permitted to send");
    const recoveryPath = join(coordinatorRoot, "recovery.json");
    await runNativeTurn(coordinator, `You are the replacement coordinator for an existing repository goal, not a new implementation request. Read ${join(root, "goal.json")}, ${join(root, "checkpoint.json")} and your durable inbox with exactly ${exactBinary} surface inbox --json. Recover the task/result/review identities from those records. Do not send any message, repeat implementation, edit source, or claim tests passed. Write ${recoveryPath} using apply_patch with JSON fields task_id, goal_sha256, project_id, base_revision, patch_sha256, implementation_message_id, review_message_id, changed_files and bindings from the checkpoint, and needs_operator equal to "independent verification". Explain in your final native response what is finished and what remains. ${communication}`, ledger);
    const recovery = JSON.parse(await readRegular(recoveryPath, 16 * 1024));
    assertRecovery(recovery, checkpoint);
    await flushMailbox(evidence, forgectl, coordinator.env, coordinator.cwd);
    const afterRecovery = await mailbox(evidence, forgectl, ledger, coordinator.env, coordinator.cwd);
    assert.deepEqual(afterRecovery.messages.filter((message) => message.from === "coordinator" && message.to === "implementation").map((message) => message.id), beforeRecoveryMessages, "recovery dispatched new implementation work");
    assert.equal((await patchSnapshot()).patch_sha256, checkpoint.patch_sha256, "recovery changed the reviewed patch");
    await checkPi();
    assert.equal(digest(await readRegular(join(root, "goal.json"), 16 * 1024, true)), state.goal_sha256, "recovery changed the operator goal copy");
    await observe("coordinator", coordinator, "openai", options["codex-model"]);
    const primaryAfter = await primarySnapshot();
    assert.deepEqual(primaryAfter, primaryBefore, "implementation changed the primary checkout");
    evidence.add("primary-checkout-unchanged", primaryAfter);
    await assertNoScratchTrust(evidence, root);
    for (const entry of provenance) {
      assert.equal(await realpath(entry.path), entry.canonical, "pilot input path was replaced");
      assert.equal(digest(await readFile(entry.canonical)), entry.sha256, "pilot executable/extension/module bytes changed");
    }
    await atomicJSON(join(root, "recovery-evidence.json"), { task_id: state.task_id, old_run: retired.member.run_id, new_run: coordinator.member.run_id, old_native_session: retired.member.thread_id, new_native_session: coordinator.member.thread_id, patch_unchanged: true, implementation_messages_unchanged: true, recovery });
    await publish("ready_for_verification", "Implementation, independent review and coordinator recovery finished; executable and terminal acceptance still required", { roles: { ...state.roles, coordinator: coordinator.member }, needs_operator: "independent verification", changed_files: patch.changed_files, patch_sha256: patch.patch_sha256 });
  } catch (error) {
    await publish(interrupted ? "interrupted" : "failed", String(error?.message ?? error), { needs_operator: "inspect retained evidence; do not blindly redispatch" });
    throw error;
  } finally {
    const cleanupErrors = [...commandCleanupErrors];
    const cleanupHandles = [...handles, ...commandHandles];
    for (const handle of cleanupHandles) {
      try { stopOwnedRole(handle); } catch (error) { cleanupErrors.push(String(error)); }
    }
    for (const handle of cleanupHandles) {
      try { await confirmOwnedRoleStopped(handle); } catch (error) { cleanupErrors.push(String(error)); }
    }
    evidence.add("owned-process-cleanup", { complete: cleanupErrors.length === 0, errors: cleanupErrors });
    process.removeListener("SIGINT", onSignal);
    process.removeListener("SIGTERM", onSignal);
    if (cleanupErrors.length > 0) {
      await publish("failed", "Owned native process cleanup is unresolved", { needs_operator: "inspect the exact owned process receipts", cleanup_errors: cleanupErrors });
      await evidence.save();
      throw new Error("owned process cleanup did not complete");
    }
    state.cleanup_complete = true;
    await atomicJSON(join(root, "state.json"), state);
    await evidence.save();
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).catch((error) => { process.stderr.write(`${error.message}\n`); process.exitCode = 1; });
}
