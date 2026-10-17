#!/usr/bin/env node

import assert from "node:assert/strict";
import { mkdir, readFile, readdir, stat, writeFile } from "node:fs/promises";
import { spawn } from "node:child_process";
import { createConnection } from "node:net";
import { createHash, randomUUID } from "node:crypto";
import { dirname, join, resolve } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";
import { HermesACPClient } from "./hermes-acp-client.mjs";
import { Evidence, CodexClient, JsonLinesProcess, assertDuplicateRejected, assertNoScratchTrust, assertRoundtripOrder, childEnv, contentText, createFixture, ensureFreshRoot, executable, flushUntilSent, lastJson, mailbox, pathExists, readReceiptRecords, recordBinary, recordFile, register, runCommand, startAckDroppingProxy, waitClosed, waitMailbox, workspaceSandbox } from "./verify-cross-harness-loop.mjs";

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const startupTimeout = 120_000;
const taskTimeout = 300_000;

export function parseHermesArgs(argv) {
  const result = {};
  const paths = ["forgectl", "root", "codex", "hermes", "hermes-python", "hermes-home", "websocket-module"];
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index];
    if (flag === "--live") result.live = true;
    else if (flag === "--help") return { help: true };
    else if ([...paths, "codex-model", "hermes-model"].some((name) => flag === `--${name}`)) {
      const value = argv[++index];
      if (!value || value.startsWith("--")) throw new Error(`${flag} needs a value`);
      result[flag.slice(2)] = value;
    } else throw new Error(`unknown argument ${flag}`);
  }
  if (!result.live) throw new Error("live Hermes verification requires --live");
  for (const name of paths) if (!result[name]?.startsWith("/")) throw new Error(`--${name} must be absolute`);
  if (!result["codex-model"] || !result["hermes-model"]) throw new Error("both native model IDs must be explicit");
  if (result["hermes-model"].includes(":")) throw new Error("--hermes-model is the unqualified OpenAI model ID");
  return result;
}

export function artifactPayload(body, taskId) {
  const prefix = `COMPLETED ${taskId}\n`;
  if (!body.startsWith(prefix)) throw new Error("worker completion must begin with its exact task marker");
  const payload = JSON.parse(body.slice(prefix.length));
  assert.deepEqual(Object.keys(payload).sort(), ["encoding", "execution_id", "filename", "source_base64", "task_id"]);
  assert.equal(payload.task_id, taskId);
  assert.equal(payload.filename, "summarize-mailbox.mjs");
  assert.equal(payload.encoding, "base64");
  assert.match(payload.execution_id, /^[A-Za-z0-9][A-Za-z0-9_.-]{7,127}$/);
  assert.equal(typeof payload.source_base64, "string");
  assert.match(payload.source_base64, /^[A-Za-z0-9+/]+={0,2}$/);
  const source = Buffer.from(payload.source_base64, "base64");
  assert(source.length > 0 && source.length <= 12_000, "worker source must be 1..12000 bytes");
  assert.equal(source.toString("base64"), payload.source_base64, "worker source encoding must be canonical and complete");
  assert.deepEqual(Buffer.from(source.toString("utf8")), source, "worker source must be UTF-8");
  return { ...payload, source };
}

export function assertNativeDelivery(history, messageIds, sourcePayload) {
  for (const messageId of messageIds) {
    const header = `[forgectl-msg] from=coordinator to=hermes-worker id=${messageId}\n`;
    const matches = history.filter((update) => update?.sessionUpdate === "user_message_chunk" && update.content?.text?.startsWith(header));
    assert.equal(matches.length, 1, `native history must contain exactly one user header for ${messageId}`);
  }
  const sourceEvidence = history.filter((update) => {
    if (update?.sessionUpdate === "agent_message_chunk") {
      try {
        const nativePayload = artifactPayload(update.content?.text ?? "", sourcePayload.task_id);
        return nativePayload.execution_id === sourcePayload.execution_id && nativePayload.source_base64 === sourcePayload.source_base64;
      } catch { return false; }
    }
    return false;
  });
  assert.equal(sourceEvidence.length, 1, "native assistant history must contain exactly one exact worker-authored payload");
}

function digest(contents) { return createHash("sha256").update(contents).digest("hex"); }
function pause(milliseconds) { return new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds)); }

function privateNativeEvidence(evidence) {
  return {
    add(kind, value) {
      const data = { ...value };
      for (const name of ["text", "stdout", "stderr"]) {
        if (typeof data[name] !== "string") continue;
        data[`${name}_sha256`] = digest(data[name]);
        data[`${name}_bytes`] = Buffer.byteLength(data[name]);
        delete data[name];
      }
      evidence.add(kind, data);
    },
  };
}

export async function closeProxy(server) {
  if (!server) return;
  const sockets = server.ownedSockets ?? new Set();
  for (const socket of sockets) socket.destroy();
  if (!server.listening) return;
  await new Promise((resolvePromise, reject) => server.close((error) => error ? reject(error) : resolvePromise()));
}

async function socketReady(path) {
  const deadline = Date.now() + startupTimeout;
  while (Date.now() < deadline) {
    try {
      const connection = createConnection(path);
      await new Promise((resolvePromise, reject) => { connection.once("connect", resolvePromise); connection.once("error", reject); });
      connection.destroy();
      return;
    } catch { await pause(100); }
  }
  throw new Error(`socket was not ready: ${path}`);
}

async function inboxRequest(socket, request) {
  return await new Promise((resolvePromise, reject) => {
    const client = createConnection(socket);
    let contents = "";
    client.setTimeout(5000, () => client.destroy(new Error("inbox request timed out")));
    client.setEncoding("utf8");
    client.once("error", reject);
    client.once("connect", () => client.write(`${JSON.stringify(request)}\n`));
    client.on("data", (chunk) => {
      contents += chunk;
      if (contents.length > 65536) client.destroy(new Error("inbox response exceeded limit"));
      if (contents.includes("\n")) {
        client.destroy();
        try { resolvePromise(JSON.parse(contents)); } catch (error) { reject(error); }
      }
    });
    client.once("end", () => { if (!contents.includes("\n")) reject(new Error("inbox closed without response")); });
  });
}

async function run(args) {
  const root = await ensureFreshRoot(args.root);
  const evidence = new Evidence(root);
  const processes = [];
  const nativeAuditors = [];
  let proxy;
  let coordinatorClient;
  let accepted = false;
  const codexHome = process.env.CODEX_HOME || join(homedir(), ".codex");
  const protectedAuth = join(codexHome, "auth.json");
  const authBefore = digest(await readFile(protectedAuth));
  const codexConfigBefore = digest(await readFile(join(codexHome, "config.toml")));
  try {
    const forgectl = await executable(args.forgectl, "forgectl");
    const codex = await executable(args.codex, "codex");
    const hermes = await executable(args.hermes, "hermes");
    await executable(args["hermes-python"], "hermes-python");
    const nativeModel = `openai-codex:${args["hermes-model"]}`;
    const webSocketModule = await import(pathToFileURL(args["websocket-module"]).href);
    const WebSocket = webSocketModule.default?.WebSocket ?? webSocketModule.default ?? webSocketModule.WebSocket;
    const ledger = join(root, "ledger");
    const coordinatorRoot = join(root, "coordinator");
    const workerRoot = join(root, "hermes");
    const runtimeHome = join(root, "native-home");
    for (const directory of [ledger, coordinatorRoot, workerRoot, runtimeHome]) await mkdir(directory, { mode: 0o700 });
    const inherited = { ...process.env };
    for (const key of Object.keys(inherited)) if (key.startsWith("FORGECTL_")) delete inherited[key];
    const baseEnv = childEnv(inherited, {}, forgectl);
    const hermesEnv = { PATH: `${dirname(args["hermes-python"])}:${dirname(forgectl)}:${process.env.PATH ?? "/usr/bin:/bin"}`, HOME: runtimeHome, HERMES_HOME: args["hermes-home"], LANG: "en_US.UTF-8", PYTHONDONTWRITEBYTECODE: "1" };
    const nativeEvidence = privateNativeEvidence(evidence);
    const checkConfig = async () => {
      const check = await runCommand(nativeEvidence, args["hermes-python"], [join(scriptDirectory, "check-hermes-native-config.py"), "--home", args["hermes-home"], "--model", args["hermes-model"]], { cwd: workerRoot, env: hermesEnv, timeout: startupTimeout });
      assert.equal(check.code, 0, "native credential/routing preflight failed; private evidence contains the diagnostic");
      const contract = lastJson(check.stdout);
      assert.equal(contract.access_only, true);
      assert.equal(contract.auxiliary_openai_only, true);
      evidence.add("hermes-native-config", contract);
    };
    await checkConfig();
    await assertNoScratchTrust(evidence, root);
    await recordBinary(evidence, "forgectl", forgectl, baseEnv, coordinatorRoot);
    await recordBinary(evidence, "codex", codex, baseEnv, coordinatorRoot);
    await recordBinary(nativeEvidence, "hermes", hermes, hermesEnv, workerRoot);
    await recordFile(evidence, "hermes-verifier", fileURLToPath(import.meta.url));
    const coordinatorSocket = join(root, "codex.sock");
    const routeSocket = join(root, "hermes-proxy.sock");
    const listenSocket = join(root, "hermes.sock");
    const restartSocket = join(root, "hermes-restart.sock");
    for (const path of [coordinatorSocket, routeSocket, listenSocket, restartSocket]) assert(path.length <= 103);
    const coordinator = await register(evidence, forgectl, "coordinator", ["--ledger", ledger, "--harness", "codex", "--worktree", coordinatorRoot, "--coordinator", "--socket", coordinatorSocket], baseEnv, coordinatorRoot);
    const coordinatorEnv = childEnv(baseEnv, coordinator.environment, forgectl);
    const worker = await register(evidence, forgectl, "hermes-worker", ["--ledger", ledger, "--harness", "hermes", "--worktree", workerRoot, "--socket", routeSocket], baseEnv, workerRoot);
    const startWorker = async (registration, listen, name) => {
      const env = { ...hermesEnv, ...registration.environment };
      const command = ["surface", "hermes", "--command", hermes, "--model", nativeModel, "--listen-socket", listen, "--json"];
      const child = spawn(forgectl, command, { cwd: workerRoot, env, stdio: ["ignore", "pipe", "pipe"] });
      processes.push(child);
      const stream = new JsonLinesProcess(child, nativeEvidence, name);
      const ready = await stream.waitFor((event) => event.type === "hermes-ready", startupTimeout);
      assert.equal(ready.ready, true);
      assert.equal(ready.run_id, registration.member.run_id);
      assert.equal(ready.provider, "openai-codex");
      assert.equal(ready.model, nativeModel);
      assert.equal(ready.mode, "default");
      assert.equal(ready.route_socket, registration.member.socket);
      assert.equal(ready.listen_socket, listen);
      assert.match(ready.session_id, /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/);
      evidence.add("hermes-supervisor-ready", ready);
      return { child, stream, ready, env };
    };
    const stopWorker = async (receiver) => {
      receiver.stream.stop();
      await waitClosed(receiver.child);
      const exits = receiver.stream.lines.filter((event) => event.type === "hermes-native-exit");
      assert.equal(exits.length, 1, "supervisor must prove its owned native process stopped");
      assert.equal(exits[0].run_id, receiver.ready.run_id);
      assert.equal(exits[0].session_id, receiver.ready.session_id);
      assert(exits[0].exitcode === 0 || exits[0].signal === "terminated", "native Hermes exit must be successful or requested SIGTERM");
      assert.equal(receiver.child.exitCode, 0, "supervisor shutdown must finish successfully");
    };
    proxy = await startAckDroppingProxy(evidence, routeSocket, listenSocket);
    proxy.ownedSockets = new Set();
    proxy.on("connection", (socket) => {
      proxy.ownedSockets.add(socket);
      socket.once("close", () => proxy.ownedSockets.delete(socket));
    });
    let receiver = await startWorker(worker, listenSocket, "hermes-supervisor");
    const nativeSession = receiver.ready.session_id;
    const coder = spawn(codex, ["app-server", "--config", `projects.${JSON.stringify(coordinatorRoot)}.trust_level="untrusted"`, "--listen", `unix://${coordinatorSocket}`], { cwd: coordinatorRoot, env: coordinatorEnv, stdio: ["ignore", "ignore", "pipe"] });
    processes.push(coder);
    coordinatorClient = new CodexClient(coder, evidence, WebSocket);
    await socketReady(coordinatorSocket);
    await coordinatorClient.connect(coordinatorSocket);
    await coordinatorClient.rpc("initialize", { clientInfo: { name: "forgectl-hermes-verifier", version: "1" }, capabilities: { experimentalApi: false } });
    coordinatorClient.notify("initialized", {});
    const allowedProbe = join(coordinatorRoot, "allowed-sandbox-probe");
    const outsideProbe = `${root}-denied-sandbox-probe`;
    assert.equal(await pathExists(outsideProbe), false);
    const sandbox = await coordinatorClient.rpc("command/exec", { command: ["/bin/sh", "-c", 'printf allowed > "$1"; printf forbidden > "$2"', "sandbox-probe", allowedProbe, outsideProbe], cwd: coordinatorRoot, sandboxPolicy: workspaceSandbox(root) });
    assert.equal(await readFile(allowedProbe, "utf8"), "allowed");
    assert.notEqual(sandbox.exitCode, 0);
    assert.match(sandbox.stderr, /Operation not permitted|Permission denied/);
    assert.equal(await pathExists(outsideProbe), false);
    evidence.add("sandbox-write-boundary", { outsideWriteDenied: true, outsideWriteExit: sandbox.exitCode });
    const thread = await coordinatorClient.rpc("thread/start", { cwd: coordinatorRoot, model: args["codex-model"], approvalPolicy: "never", sandbox: "read-only", ephemeral: false, threadSource: "startup" });
    const threadId = thread.thread?.id ?? thread.id;
    assert(threadId);
    assert.equal(thread.model, args["codex-model"]);
    assert.equal(thread.thread?.ephemeral, false);
    await register(evidence, forgectl, "coordinator", ["--ledger", ledger, "--harness", "codex", "--worktree", coordinatorRoot, "--coordinator", "--socket", coordinatorSocket, "--thread", threadId, "--run-id", coordinator.member.run_id, "--replace"], baseEnv, coordinatorRoot);
    const fixture = await createFixture(root);
    const artifact = join(coordinatorRoot, "summarize-mailbox.mjs");
    const taskId = `hermes-collaboration-${randomUUID()}`;
    const exactBinary = `'${forgectl.replaceAll("'", "'\\''")}'`;
    const prompt = `You are Codex, coordinator of an explicitly enrolled cross-harness team. Delegate task ${taskId} to hermes-worker using ${exactBinary} surface send hermes-worker '<task text>' --json. Use this exact executable, never a PATH alias. Your network-disabled sandbox may return queued exit75; the task supervisor flushes this private mailbox. Do not resend queued or unconfirmed messages, broaden permissions, or answer native approval requests. End each turn after sending and wait for native queued replies.
Hermes must author a Node-builtins-only standalone utility named summarize-mailbox.mjs. CLI: node summarize-mailbox.mjs INPUT_PATH; read process.argv[2], never hardcode ${fixture}. It reads forgectl mailbox JSONL, counts final sent/queued/failed/unconfirmed statuses and returns exactly {"counts":{"sent":1,"queued":1,"failed":1,"unconfirmed":1},"ids":["fixture-1","fixture-2","fixture-3","fixture-4"]} for fixture ${fixture}. Fold later status records; preserve message-ID order; reject duplicate op:msg IDs with nonzero exit and duplicate-specific stderr; reject missing input path nonzero. Tell Hermes to ask how a message without a status record should count before authoring the source. Its clarification must begin CLARIFICATION ${taskId}. Do not disclose the policy in the initial delegation: when that native clarification arrives, answer through the mailbox with ANSWER ${taskId} and the policy that such a message counts as queued.
Hermes must not create or edit task files, execute the generated utility, or request broader permissions. It may read the fixture and use native terminal solely to send enrolled-team mail. Its product is source code carried in the reply, not a file it writes. Require one mailbox completion beginning exactly COMPLETED ${taskId} followed by newline and one JSON object with exactly keys encoding="base64", filename="summarize-mailbox.mjs", source_base64=<canonical base64 of the exact UTF-8 source, max12000 source bytes>, task_id="${taskId}", execution_id=<unique identifier>. Hermes must print that exact completed body as its final assistant response too, preserving provenance. Send payloads through ${exactBinary} surface send coordinator - --json with stdin when helpful; do not use file-writing workarounds or permission bypass flags.
You must not author or repair its utility. After receiving COMPLETED, parse the JSON, decode source_base64 without reformatting and write those exact bytes to ${artifact} inside your existing sandbox. Run node ${artifact} ${fixture}; report what passed or failed without altering its code. Do not send another worker message after completion. No human relays agent messages. This deliberately proves worker-authored source plus coordinator materialization, not worker filesystem access.`;
    await coordinatorClient.turn(threadId, prompt, coordinatorRoot, root);
    const delegated = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "coordinator" && message.to === "hermes-worker" && message.body.includes(taskId) && ["sent", "unconfirmed"].includes(message.status), 120_000);
    assert.equal(proxy.droppedId(), delegated.id);
    const journal = await readReceiptRecords(join(ledger, "mailbox.jsonl"));
    assert(journal.some((record) => record.op === "status" && record.id === delegated.id && record.status === "unconfirmed"));
    await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, delegated.id, taskTimeout);
    assert.equal(proxy.deliveryIds().filter((id) => id === delegated.id).length, 1);
    evidence.add("lost-ack-reconciled", { id: delegated.id, deliveryCount: 1 });
    const clarification = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "hermes-worker" && message.to === "coordinator" && message.status === "sent" && message.body.startsWith(`CLARIFICATION ${taskId}`), taskTimeout);
    const answer = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "coordinator" && message.to === "hermes-worker" && message.status === "sent" && message.body.startsWith(`ANSWER ${taskId}`), taskTimeout);
    const completion = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "hermes-worker" && message.to === "coordinator" && message.status === "sent" && message.body.startsWith(`COMPLETED ${taskId}\n`), taskTimeout);
    const sourcePayload = artifactPayload(completion.body, taskId);
    const nativeInput = await coordinatorClient.waitFor((event) => event.method === "item/completed" && event.params?.threadId === threadId && event.params?.item?.type === "userMessage" && contentText(event.params.item.content).includes(`id=${completion.id}`), taskTimeout);
    const completedTurn = await coordinatorClient.waitFor((event) => event.method === "turn/completed" && event.params?.threadId === threadId && event.params?.turn?.id === nativeInput.params.turnId, taskTimeout);
    assert.equal(completedTurn.params.turn.status, "completed");
    assert.deepEqual(await readFile(artifact), sourcePayload.source, "coordinator must materialize exact worker-authored bytes");
    const report = await mailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot);
    assert.equal(report.messages.filter((message) => message.from === "hermes-worker" && message.body.startsWith(`COMPLETED ${taskId}\n`)).length, 1);
    const order = (await readReceiptRecords(join(ledger, "mailbox.jsonl"))).filter((record) => record.op === "msg").map((record) => record.msg.id);
    const roundtrip = [delegated.id, clarification.id, answer.id, completion.id];
    evidence.add("task-roundtrip-order", { ids: roundtrip, positions: assertRoundtripOrder(order, roundtrip) });
    const positive = await runCommand(evidence, process.execPath, [artifact, fixture], { cwd: coordinatorRoot, env: baseEnv });
    assert.equal(positive.code, 0);
    assert.deepEqual(lastJson(positive.stdout), { counts: { sent: 1, queued: 1, failed: 1, unconfirmed: 1 }, ids: ["fixture-1", "fixture-2", "fixture-3", "fixture-4"] });
    const fixtureText = await readFile(fixture, "utf8");
    const corrupt = join(root, "duplicate.jsonl");
    await writeFile(corrupt, `${fixtureText}${fixtureText.split("\n")[0]}\n`, { mode: 0o600 });
    const negative = await runCommand(evidence, process.execPath, [artifact, corrupt], { cwd: coordinatorRoot, env: baseEnv });
    assertDuplicateRejected(negative);
    const missing = await runCommand(evidence, process.execPath, [artifact], { cwd: coordinatorRoot, env: baseEnv });
    assert(Number.isInteger(missing.code) && missing.code > 0);
    const withoutStatus = join(root, "without-status.jsonl");
    await writeFile(withoutStatus, `${fixtureText.split("\n")[0]}\n`, { mode: 0o600 });
    const clarificationCheck = await runCommand(evidence, process.execPath, [artifact, withoutStatus], { cwd: coordinatorRoot, env: baseEnv });
    assert.equal(clarificationCheck.code, 0);
    assert.deepEqual(lastJson(clarificationCheck.stdout), { counts: { sent: 0, queued: 1, failed: 0, unconfirmed: 0 }, ids: ["fixture-1"] });
    const artifactHash = digest(sourcePayload.source);
    evidence.add("worker-artifact-verified", { taskId, sha256: artifactHash, executionId: sourcePayload.execution_id, worker_file_created: false, worker_authored_source: true, coordinator_materialized: true, positive: positive.code, duplicate: negative.code, missing: missing.code, clarification: clarificationCheck.code });
    const sendProbe = async (body) => {
      const result = await runCommand(evidence, forgectl, ["surface", "send", "hermes-worker", body, "--json"], { cwd: coordinatorRoot, env: coordinatorEnv });
      const sent = lastJson(result.stdout);
      assert([0, 1, 75].includes(result.code));
      assert(["sent", "unconfirmed", "queued"].includes(sent.status));
      return { result, sent };
    };
    const busyToken = `busy-${randomUUID()}`;
    const busy = await sendProbe(`Run the native terminal command sleep 5 for a busy-delivery test, then reply only in your assistant response with ${busyToken}. Do not send mailbox replies or modify files.`);
    await receiver.stream.waitFor((event) => event.type === "hermes-turn-start" && event.message_id === busy.sent.id, startupTimeout);
    const duringBusy = await sendProbe(`Consume the busy follow-up ${busyToken} without tools, file changes, or mailbox replies.`);
    assert.equal(duringBusy.result.code, 75);
    assert.equal(duringBusy.sent.status, "queued");
    await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, busy.sent.id, taskTimeout);
    await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, duringBusy.sent.id, taskTimeout);
    evidence.add("busy-receiver", { activeId: busy.sent.id, queuedId: duringBusy.sent.id, code: 75, reconciled: "sent" });
    const deniedFile = join(workerRoot, "denied-write-probe.txt");
    const permissionStart = receiver.stream.lines.length;
    const permission = await sendProbe(`Native permission-control check: attempt exactly one write_file call to ${deniedFile} with content control. If permission is cancelled or denied, do not retry or use terminal/other writers; stop with an assistant acknowledgement. Do not send mailbox replies.`);
    await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, permission.sent.id, taskTimeout);
    assert.equal(await pathExists(deniedFile), false);
    await receiver.stream.waitForAfter((event) => event.type === "hermes-permission-denied", permissionStart, startupTimeout);
    evidence.add("native-edit-denied", { messageId: permission.sent.id, pathAbsent: true });
    await stopWorker(receiver);
    await closeProxy(proxy);
    assert.equal(await pathExists(routeSocket), false, "proxy must release its owned socket");
    const restarted = await register(evidence, forgectl, "hermes-worker", ["--ledger", ledger, "--harness", "hermes", "--worktree", workerRoot, "--socket", restartSocket, "--thread", nativeSession, "--replace"], baseEnv, workerRoot);
    assert.notEqual(restarted.member.run_id, worker.member.run_id);
    const stale = await runCommand(evidence, forgectl, ["surface", "send", "coordinator", `stale Hermes incarnation ${taskId}`, "--json"], { cwd: workerRoot, env: receiver.env });
    assert.notEqual(stale.code, 0);
    assert.match(stale.stderr, /FORGECTL_RUN_ID/);
    const recovery = await sendProbe(`Consume recovery ${taskId} without tools, file changes, or mailbox replies.`);
    assert.equal(recovery.result.code, 75);
    assert.equal(recovery.sent.status, "queued");
    evidence.add("stopped-receiver", { id: recovery.sent.id, status: "queued", code: 75, retiredRunRejected: true });
    receiver = await startWorker(restarted, restartSocket, "hermes-supervisor-restarted");
    assert.equal(receiver.ready.session_id, nativeSession);
    const wrongIdentity = { v: 1, type: "deliver", id: `wrong-${randomUUID()}`, run_id: restarted.member.run_id, session_id: randomUUID(), text: "WRONG_SESSION_NEGATIVE_CONTROL" };
    const refused = await inboxRequest(restartSocket, wrongIdentity);
    assert.equal(refused.ok, false);
    assert.notEqual(refused.accepted, true);
    evidence.add("selected-session-refusal", { rejected: true, messageId: wrongIdentity.id });
    await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, recovery.sent.id, taskTimeout);
    const repeated = await inboxRequest(restartSocket, { v: 1, type: "receipt", id: delegated.id, run_id: restarted.member.run_id, session_id: nativeSession });
    assert.equal(repeated.accepted, true);
    assert.equal(repeated.evidence, "history");
    evidence.add("hermes-restart-reconciled", { sessionId: nativeSession, oldRunId: worker.member.run_id, runId: restarted.member.run_id, recoveredId: recovery.sent.id, originalReceiptPreserved: true });
    await stopWorker(receiver);
    const audit = new HermesACPClient(hermes, workerRoot, hermesEnv, evidence);
    nativeAuditors.push(audit);
    await audit.initialize();
    const history = await audit.history(nativeSession, workerRoot, nativeModel);
    const workerInputs = [delegated.id, answer.id, busy.sent.id, duringBusy.sent.id, permission.sent.id, recovery.sent.id];
    assertNativeDelivery(history, workerInputs, sourcePayload);
    assert.equal(history.some((update) => update.sessionUpdate === "user_message_chunk" && update.content?.text?.includes(wrongIdentity.text)), false);
    assert(history.some((update) => update.sessionUpdate === "tool_call" && update.kind === "edit" && JSON.stringify(update).includes(deniedFile)), "native history must show the requested denied file-write control");
    assert.equal((await audit.stop()).code, 0);
    assert.deepEqual(await readFile(artifact), sourcePayload.source);
    assert.deepEqual(await readdir(workerRoot), [], "Hermes must not create task files under this source-transport contract");
    evidence.add("native-history-audited", { sessionId: nativeSession, uniqueInputIds: workerInputs, exactSourceProvenance: true, wrongSessionAbsent: true, artifactPreserved: true });
    await checkConfig();
    await coordinatorClient.waitForThreadIdle(threadId);
    coordinatorClient.stop();
    await waitClosed(coder);
    await assertNoScratchTrust(evidence, root);
    assert.equal(digest(await readFile(protectedAuth)), authBefore, "global Codex auth changed during this run");
    assert.equal(digest(await readFile(join(codexHome, "config.toml"))), codexConfigBefore, "global Codex config changed during this run");
    for (const child of processes) assert(child.exitCode !== null || child.signalCode !== null);
    evidence.add("acceptance-summary", { taskId, threadId, nativeSession, artifact, artifactHash, globalAuthUnchanged: true, globalConfigUnchanged: true, workerAuthoredSource: true, coordinatorMaterialized: true });
    await evidence.save();
    await writeFile(join(root, "ACCEPTED"), `${JSON.stringify({ taskId, threadId, nativeSession, artifact, artifactHash, worker_file_created: false, worker_authored_source: true, coordinator_materialized: true }, null, 2)}\n`, { mode: 0o600 });
    accepted = true;
    process.stdout.write(`${JSON.stringify({ root, taskId, nativeSession, artifact: "verified", source: "Hermes", materializedBy: "Codex", recovery: "verified" })}\n`);
  } catch (error) {
    evidence.add("acceptance-failed", { error: String(error) });
    throw error;
  } finally {
    coordinatorClient?.stop();
    try { await closeProxy(proxy); } catch (error) { evidence.add("cleanup-failed", { error: String(error), component: "proxy" }); }
    for (const audit of nativeAuditors) if (!audit.closed) audit.child.kill("SIGTERM");
    for (const child of processes) if (child.exitCode === null && child.signalCode === null) child.kill("SIGTERM");
    for (const child of [...processes, ...nativeAuditors.map((audit) => audit.child)]) {
      try { await waitClosed(child); } catch (error) { evidence.add("cleanup-failed", { error: String(error), pid: child.pid }); }
    }
    await evidence.save();
    if (!accepted) process.stderr.write(`Hermes collaboration was not accepted; private evidence: ${root}\n`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const args = parseHermesArgs(process.argv.slice(2));
    if (args.help) process.stdout.write("verify-hermes-collaboration.mjs --live --forgectl ABS --root ABS --codex ABS --codex-model ID --hermes ABS --hermes-python ABS --hermes-home ABS --hermes-model ID --websocket-module ABS\n");
    else await run(args);
  } catch (error) {
    process.stderr.write(`${String(error)}\n`);
    process.exitCode = 1;
  }
}
