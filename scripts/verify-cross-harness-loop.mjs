#!/usr/bin/env node

import { lstat, mkdir, readdir, readFile, realpath, stat, writeFile } from "node:fs/promises";
import { appendFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { spawn } from "node:child_process";
import { createConnection, createServer } from "node:net";
import { createHash, randomUUID } from "node:crypto";
import { pathToFileURL } from "node:url";
import { homedir } from "node:os";

const MAX_LOG = 24_000;
const DEFAULT_TIMEOUT = 180_000;
const sensitive = /(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL|COOKIE|AUTH)/i;
let activeEvidence;
let activeCleanup = async () => {};

const usage = `usage: verify-cross-harness-loop.mjs --live --forgectl ABS --root ABS --websocket-module ABS --codex-model ID [--codex ABS] [--pi ABS] [--pi-provider NAME --pi-model ID] [--pi-extension ABS ...]

Runs the opt-in disposable Codex/Pi collaboration acceptance loop. The root
must be an existing empty 0700 directory. The script never copies auth files
or prints inherited secret values.
`;

function fail(message) {
  throw new Error(message);
}

export function parseArgs(argv) {
  const out = { live: false, codex: "/opt/homebrew/bin/codex", pi: "/opt/homebrew/bin/pi", websocketModule: "", piExtensions: [] };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--live") out.live = true;
    else if (arg === "--help" || arg === "-h") return { help: true };
    else if (arg === "--pi-extension") {
      const value = argv[++index];
      if (!value) fail(`${arg} needs a value`);
      out.piExtensions.push(value);
    } else if (["--forgectl", "--root", "--codex", "--codex-model", "--pi", "--websocket-module", "--pi-provider", "--pi-model"].includes(arg)) {
      const value = argv[++index];
      if (!value) fail(`${arg} needs a value`);
      out[arg === "--websocket-module" ? "websocketModule" : arg.slice(2)] = value;
    } else fail(`unknown argument ${arg}`);
  }
  if (!out.live) fail("refusing a live harness run without explicit --live");
  for (const name of ["forgectl", "root", "codex", "pi", "websocketModule"]) {
    if (!out[name] || !out[name].startsWith("/")) fail(`--${name} must be an absolute path`);
  }
  if (out.piExtensions.some((path) => !path.startsWith("/"))) fail("--pi-extension must be an absolute path");
  if (!out["codex-model"]) fail("--codex-model is required to avoid mutable global model defaults");
  if (Boolean(out["pi-provider"]) !== Boolean(out["pi-model"])) fail("--pi-provider and --pi-model must be supplied together");
  return out;
}

function bounded(value) {
  const text = String(value ?? "");
  return text.length > MAX_LOG ? `${text.slice(0, MAX_LOG)}…` : text;
}

function safeEnv(env) {
  return Object.fromEntries(Object.keys(env).filter((key) => !sensitive.test(key)).map((key) => [key, "<inherited>"]));
}

function lastJson(text) {
  const raw = String(text).trim();
  if (!raw) fail("expected JSON output, got empty stdout");
  try { return JSON.parse(raw); } catch (error) {
    fail(`expected one JSON document, got: ${bounded(raw)} (${error})`);
  }
}

function sleep(ms) { return new Promise((resolvePromise) => setTimeout(resolvePromise, ms)); }

function waitClosed(processHandle, timeout = 15_000) {
  if (processHandle.exitCode !== null || processHandle.signalCode !== null) return Promise.resolve();
  return new Promise((resolvePromise, reject) => {
    const timer = setTimeout(() => reject(new Error(`owned process ${processHandle.pid ?? "unknown"} did not exit within ${timeout}ms`)), timeout);
    processHandle.once("close", () => { clearTimeout(timer); resolvePromise(); });
  });
}

async function ensureFreshRoot(root) {
  const linkInfo = await lstat(root).catch(() => undefined);
  if (linkInfo?.isSymbolicLink()) fail("--root must not be a symbolic link");
  const rootInfo = await stat(root).catch(() => undefined);
  if (!rootInfo?.isDirectory()) fail("--root must be an existing directory created for this run");
  if ((rootInfo.mode & 0o077) !== 0) fail("--root must be private (mode 0700 or stricter)");
  const canonical = await realpath(root);
  if (canonical === "/" || canonical === resolve(process.cwd())) fail("--root is ambiguous or unsafe");
  const entries = await readdir(canonical);
  if (entries.length !== 0) fail("--root must be empty; refusing to reuse a scratch directory");
  await writeFile(join(canonical, ".acceptance-running"), `${process.pid}\n`, { mode: 0o600 });
  return canonical;
}

async function executable(path, label) {
  const info = await stat(path).catch(() => undefined);
  if (!info?.isFile()) fail(`${label} is not a regular executable: ${path}`);
  if ((info.mode & 0o111) === 0) fail(`${label} is not executable: ${path}`);
  return await realpath(path);
}

async function extensionFile(path) {
  const info = await stat(path).catch(() => undefined);
  if (!info?.isFile()) fail(`--pi-extension is not a regular file: ${path}`);
  return await realpath(path);
}

async function moduleFile(path) {
  const info = await stat(path).catch(() => undefined);
  if (!info?.isFile()) fail(`--websocket-module is not a regular file: ${path}`);
  return await realpath(path);
}

class Evidence {
  constructor(root) { this.root = root; this.records = []; this.nativeFailure = undefined; }
  add(kind, data = {}) {
    const record = { at: new Date().toISOString(), kind, ...data };
    this.records.push(record);
    appendFileSync(join(this.root, "events.jsonl"), `${JSON.stringify(record)}\n`, { mode: 0o600 });
  }
  async save(name = "evidence.json") {
    await writeFile(join(this.root, name), `${JSON.stringify({ version: 1, records: this.records }, null, 2)}\n`, { mode: 0o600 });
  }
}

function runCommand(evidence, command, args, options = {}) {
  const timeout = options.timeout ?? 30_000;
  const env = { ...(options.env ?? process.env) };
  evidence.add("command-start", { command, args, cwd: options.cwd, env: safeEnv(env) });
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, { cwd: options.cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    const timer = setTimeout(() => child.kill("SIGTERM"), timeout);
    child.once("error", (error) => { clearTimeout(timer); reject(error); });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      const result = { code, signal, stdout, stderr };
      evidence.add("command-result", { command, args, code, signal, stdout: bounded(stdout), stderr: bounded(stderr) });
      resolvePromise(result);
    });
  });
}

function childEnv(base, additions, forgectl) {
  const path = `${dirname(forgectl)}:${base.PATH ?? ""}`;
  return { ...base, ...additions, PATH: path };
}

class JsonLinesProcess {
  constructor(child, evidence, name) {
    this.child = child;
    this.name = name;
    this.evidence = evidence;
    this.lines = [];
    this.waiters = [];
    this.buffer = "";
    this.closed = false;
    this.lastAssistantError = undefined;
    child.stdout.on("data", (chunk) => this.#data(String(chunk)));
    child.stderr.on("data", (chunk) => evidence.add("native-stderr", { name, text: bounded(chunk) }));
    child.once("close", (code, signal) => {
      this.closed = true;
      evidence.add("native-exit", { name, code, signal });
      for (const waiter of this.waiters.splice(0)) waiter.reject(new Error(`${name} exited before its JSONL response`));
    });
  }
  #data(chunk) {
    this.buffer += chunk;
    let nl;
    while ((nl = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, nl).replace(/\r$/, "");
      this.buffer = this.buffer.slice(nl + 1);
      if (!line) continue;
      let value;
      try { value = JSON.parse(line); } catch (error) {
        this.evidence.add("native-invalid-json", { name: this.name, line: bounded(line), error: String(error) });
        continue;
      }
      this.lines.push(value);
      this.evidence.add("native-event", { name: this.name, message: value });
      if (value.type === "message_end" && value.message?.role === "assistant") {
        this.lastAssistantError = ["error", "aborted"].includes(value.message.stopReason)
          ? `${value.message.stopReason}: ${value.message.errorMessage ?? "native model turn failed"}`
          : undefined;
      }
      if (value.type === "agent_settled" && this.lastAssistantError) {
        this.evidence.nativeFailure = `${this.name} ${this.lastAssistantError}`;
        for (const waiter of this.waiters.splice(0)) waiter.reject(new Error(this.evidence.nativeFailure));
      }
      if (this.name === "codex-app-server" && value.jsonrpc === "2.0" && value.method && value.id !== undefined && !Object.hasOwn(value, "result") && !Object.hasOwn(value, "error")) {
        this.send({ jsonrpc: "2.0", id: value.id, result: { decision: "decline" } });
        this.evidence.add("codex-request-declined", { method: value.method });
      }
      const eventIndex = this.lines.length - 1;
      for (const waiter of [...this.waiters]) {
        if (eventIndex < waiter.afterIndex) continue;
        if (waiter.predicate(value)) {
          this.waiters.splice(this.waiters.indexOf(waiter), 1);
          waiter.resolve(value);
        }
      }
    }
  }
  send(value) {
    if (this.closed) fail(`${this.name} is already stopped`);
    this.child.stdin.write(`${JSON.stringify(value)}\n`);
  }
  waitFor(predicate, timeout = DEFAULT_TIMEOUT, afterIndex = 0) {
    if (this.evidence.nativeFailure) return Promise.reject(new Error(this.evidence.nativeFailure));
    const existing = this.lines.slice(afterIndex).find(predicate);
    if (existing) return Promise.resolve(existing);
    return new Promise((resolvePromise, reject) => {
      const waiter = { predicate, resolve: resolvePromise, reject, afterIndex };
      this.waiters.push(waiter);
      const timer = setTimeout(() => {
        const index = this.waiters.indexOf(waiter);
        if (index >= 0) this.waiters.splice(index, 1);
        reject(new Error(`${this.name} JSONL event timed out`));
      }, timeout);
      waiter.resolve = (value) => { clearTimeout(timer); resolvePromise(value); };
      waiter.reject = (error) => { clearTimeout(timer); reject(error); };
    });
  }
  waitForAfter(predicate, afterIndex, timeout = DEFAULT_TIMEOUT) { return this.waitFor(predicate, timeout, afterIndex); }
  async request(value, timeout = 30_000) {
    const id = value.id ?? randomUUID();
    const request = { ...value, id };
    const response = this.waitFor((message) => message.id === id && message.type === "response", timeout);
    this.send(request);
    const result = await response;
    if (result.success === false || result.error) fail(`${this.name} rejected ${value.type}: ${bounded(JSON.stringify(result))}`);
    return result;
  }
  stop(signal = "SIGTERM") { if (!this.closed) this.child.kill(signal); }
}

class CodexClient {
  constructor(child, evidence, WebSocket) {
    this.child = child;
    this.evidence = evidence;
    this.WebSocket = WebSocket;
    this.name = "codex-app-server";
    this.nextId = 1;
    this.lines = [];
    this.waiters = [];
    this.buffer = "";
    this.closed = false;
    child.stderr.on("data", (chunk) => evidence.add("native-stderr", { name: this.name, text: bounded(chunk) }));
    child.once("close", (code, signal) => {
      this.closed = true;
      evidence.add("native-exit", { name: this.name, code, signal });
      for (const waiter of this.waiters.splice(0)) waiter.reject(new Error("Codex app-server exited before its JSON-RPC response"));
    });
  }
  async connect(path) {
    if (typeof this.WebSocket !== "function") fail("the supplied WebSocket module did not export a constructor");
    const address = `ws+unix://${path}:/`;
    const socket = new this.WebSocket(address, { perMessageDeflate: false, handshakeTimeout: 10_000 });
    await new Promise((resolvePromise, reject) => {
      let opened = false;
      socket.on("open", () => { opened = true; resolvePromise(); });
      socket.on("error", (error) => {
        this.evidence.add("native-websocket-error", { name: this.name, address, error: String(error) });
        if (!opened) reject(error);
      });
    });
    socket.on("message", (data) => this.#data(Buffer.isBuffer(data) ? data.toString("utf8") : String(data)));
    socket.on("close", () => {
      this.evidence.add("native-socket-close", { name: this.name });
      for (const waiter of this.waiters.splice(0)) waiter.reject(new Error("Codex app-server socket closed"));
    });
    this.socket = socket;
  }
  #data(chunk) {
    const text = String(chunk).trim();
    if (!text) return;
    let value;
    try { value = JSON.parse(text); } catch (error) {
      this.evidence.add("native-invalid-json", { name: this.name, line: bounded(text), error: String(error) });
      return;
    }
    this.lines.push(value);
    this.evidence.add("native-event", { name: this.name, message: value });
    if (value.jsonrpc === "2.0" && value.method && value.id !== undefined && !Object.hasOwn(value, "result") && !Object.hasOwn(value, "error")) {
      this.send({ jsonrpc: "2.0", id: value.id, result: { decision: "decline" } });
      this.evidence.add("codex-request-declined", { method: value.method });
    }
    for (const waiter of [...this.waiters]) {
      if (waiter.predicate(value)) {
        this.waiters.splice(this.waiters.indexOf(waiter), 1);
        waiter.resolve(value);
      }
    }
  }
  send(value) {
    if (this.closed || !this.socket) fail("Codex app-server socket is not connected");
    this.socket.send(JSON.stringify(value));
  }
  waitFor(predicate, timeout = DEFAULT_TIMEOUT) {
    const existing = this.lines.find(predicate);
    if (existing) return Promise.resolve(existing);
    return new Promise((resolvePromise, reject) => {
      const waiter = { predicate, resolve: resolvePromise, reject };
      this.waiters.push(waiter);
      const timer = setTimeout(() => {
        const index = this.waiters.indexOf(waiter);
        if (index >= 0) this.waiters.splice(index, 1);
        reject(new Error("Codex app-server JSON-RPC event timed out"));
      }, timeout);
      waiter.resolve = (value) => { clearTimeout(timer); resolvePromise(value); };
      waiter.reject = (error) => { clearTimeout(timer); reject(error); };
    });
  }
  async rpc(method, params, timeout = 30_000) {
    const id = this.nextId++;
    const response = this.waitFor((message) => message.id === id && (Object.hasOwn(message, "result") || Object.hasOwn(message, "error")), timeout);
    this.send({ jsonrpc: "2.0", id, method, params });
    const result = await response;
    if (result.error) fail(`Codex ${method} failed: ${bounded(JSON.stringify(result.error))}`);
    return result.result;
  }
  notify(method, params) { this.send({ jsonrpc: "2.0", method, params }); }
  async turn(threadId, message, cwd, writableRoot) {
    const result = await this.rpc("turn/start", {
      threadId,
      cwd,
      approvalPolicy: "never",
      sandboxPolicy: { type: "workspaceWrite", networkAccess: false, writableRoots: [writableRoot] },
      input: [{ type: "text", text: message }],
    }, DEFAULT_TIMEOUT);
    const turnId = result?.turn?.id ?? result?.id;
    const completion = await this.waitFor((event) => event.method === "turn/completed" && (!turnId || event.params?.turn?.id === turnId), DEFAULT_TIMEOUT);
    const status = completion.params?.turn?.status;
    if (status !== "completed") fail(`Codex turn ${turnId ?? "unknown"} ended with status ${status ?? "missing"}: ${bounded(completion.params?.turn?.error?.message ?? "no native error detail")}`);
    return { turnId, result, completion };
  }
  stop(signal = "SIGTERM") {
    if (this.socket) this.socket.terminate();
    if (!this.closed) this.child.kill(signal);
  }
}

async function register(evidence, forgectl, name, flags, env, cwd) {
  const result = await runCommand(evidence, forgectl, ["surface", "team", "register", name, "--json", ...flags], { cwd, env });
  if (result.code !== 0) fail(`team registration failed for ${name}`);
  const json = lastJson(result.stdout);
  if (!json || typeof json.ledger !== "string" || !json.member || json.member.name !== name || typeof json.member.harness !== "string" || typeof json.member.worktree !== "string" || json.environment?.FORGECTL_LEDGER !== json.ledger || typeof json.environment.FORGECTL_RUN_ID !== "string") fail(`unexpected registration for ${name}`);
  return json;
}

async function mailbox(evidence, forgectl, ledger, env, cwd) {
  const result = await runCommand(evidence, forgectl, ["surface", "inbox", "--all", "--json"], { cwd, env });
  if (result.code !== 0) fail(`inbox failed: ${bounded(result.stderr)}`);
  const report = lastJson(result.stdout);
  if (!report || report.ledger !== ledger || !Array.isArray(report.messages) || report.messages.some((message) => !message || typeof message !== "object" || typeof message.id !== "string" || typeof message.from !== "string" || typeof message.to !== "string" || typeof message.status !== "string" || typeof message.body !== "string")) fail("inbox returned an unexpected JSON shape");
  return report;
}

async function flushMailbox(evidence, forgectl, env, cwd) {
  const result = await runCommand(evidence, forgectl, ["surface", "flush", "--json"], { cwd, env });
  if (result.code !== 0) fail(`flush failed: ${bounded(result.stderr)}`);
  const report = lastJson(result.stdout);
  const counters = ["sent", "requeued", "failed", "expired", "unconfirmed", "skipped"];
  if (!report || typeof report !== "object" || Array.isArray(report) || counters.some((key) => !Number.isInteger(report[key]) || report[key] < 0)) fail("flush returned an unexpected JSON shape");
  return report;
}

async function waitMailbox(evidence, forgectl, ledger, env, cwd, predicate, timeout = DEFAULT_TIMEOUT) {
  if (env.FORGECTL_LEDGER !== ledger) fail("supervisor ledger does not match the enrolled team");
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (evidence.nativeFailure) fail(evidence.nativeFailure);
    const delivery = await flushMailbox(evidence, forgectl, env, cwd);
    evidence.add("supervisor-flush", { ledger, delivery });
    const report = await mailbox(evidence, forgectl, ledger, env, cwd);
    const found = (report.messages ?? []).find(predicate);
    if (found) return found;
    await sleep(1_000);
  }
  fail("mailbox condition timed out");
}

function startAckDroppingProxy(evidence, listenPath, targetPath) {
  let dropped = false;
  let droppedId;
  const deliveryIds = [];
  const server = createServer((client) => {
    let request = "";
    let requestRecord;
    client.setEncoding("utf8");
    client.on("data", (chunk) => {
      request += chunk;
      if (!request.includes("\n")) return;
      try {
        requestRecord = JSON.parse(request.trim());
        if (requestRecord.type === "deliver" && requestRecord.id) deliveryIds.push(requestRecord.id);
      } catch { }
      const upstream = createConnection(targetPath);
      upstream.setEncoding("utf8");
      upstream.on("error", (error) => client.destroy(error));
      upstream.on("connect", () => upstream.write(request));
      let response = "";
      upstream.on("data", (chunk) => {
        response += chunk;
        if (!response.includes("\n")) return;
        let parsed;
        try { parsed = JSON.parse(response.trim()); } catch { parsed = undefined; }
        if (!dropped && requestRecord?.type === "deliver" && parsed?.id) {
          dropped = true;
          droppedId = parsed.id;
          evidence.add("proxy-drop-first-delivery-response", { id: parsed.id, response: parsed });
          upstream.destroy();
          client.destroy();
          return;
        }
        client.end(response);
        upstream.destroy();
      });
    });
  });
  server.listen(listenPath);
  evidence.add("proxy-start", { listenPath, targetPath, policy: "drop-first-positive-ack-forward-receipts" });
  server.droppedId = () => droppedId;
  server.deliveryIds = () => [...deliveryIds];
  return server;
}

async function flushUntilSent(evidence, forgectl, ledger, env, cwd, id, timeout = 60_000) {
  const deadline = Date.now() + timeout;
  let latest;
  while (Date.now() < deadline) {
    latest = await flushMailbox(evidence, forgectl, env, cwd);
    const report = await mailbox(evidence, forgectl, ledger, env, cwd);
    const message = (report.messages ?? []).find((candidate) => candidate.id === id);
    if (message?.status === "sent") return { message, flush: latest };
    await sleep(1_000);
  }
  fail(`message ${id} did not reconcile to sent after repeated flush attempts`);
}

async function createFixture(root) {
  const fixture = join(root, "mailbox-fixture.jsonl");
  const now = new Date().toISOString();
  const messages = ["sent", "queued", "failed", "unconfirmed"].map((status, index) => {
    const id = `fixture-${index + 1}`;
    return [
      { op: "msg", at: now, msg: { v: 1, id, from: "coordinator", to: "pi-worker", body: `fixture ${id}`, priority: "next", created_at: now } },
      { op: "status", at: now, id, status },
    ];
  }).flat();
  await writeFile(fixture, `${messages.map((line) => JSON.stringify(line)).join("\n")}\n`, { mode: 0o600 });
  return fixture;
}

async function readReceiptRecords(path) {
  const contents = await readFile(path, "utf8").catch(() => "");
  return contents.split("\n").filter(Boolean).map((line) => JSON.parse(line));
}

export function assertRoundtripOrder(messageOrder, roundtripIds) {
  const positions = roundtripIds.map((id) => messageOrder.indexOf(id));
  if (positions.some((position, index) => position < 0 || (index > 0 && position <= positions[index - 1]))) fail("delegation, clarification, answer, and completion did not occur in causal mailbox order");
  return positions;
}

function contentText(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.map((part) => typeof part === "string" ? part : part?.type === "text" && typeof part.text === "string" ? part.text : "").join("");
}

async function recordBinary(evidence, label, executablePath, env, cwd) {
  const versionResult = await runCommand(evidence, executablePath, ["--version"], { cwd, env });
  if (versionResult.code !== 0) fail(`${label} --version failed: ${bounded(versionResult.stderr)}`);
  const digest = createHash("sha256").update(await readFile(executablePath)).digest("hex");
  evidence.add("binary", { label, path: executablePath, sha256: digest, version: bounded(versionResult.stdout) });
}

async function assertNoScratchTrust(evidence, root) {
  const configPath = join(process.env.CODEX_HOME || join(homedir(), ".codex"), "config.toml");
  const contents = await readFile(configPath, "utf8").catch((error) => {
    if (error.code === "ENOENT") return "";
    throw error;
  });
  if (contents.includes(root)) fail("native Codex persisted the scratch directory in its global configuration");
  evidence.add("codex-global-config-check", { scratchTrustAbsent: true, sha256: createHash("sha256").update(contents).digest("hex") });
}

async function recordFile(evidence, label, filePath) {
  const digest = createHash("sha256").update(await readFile(filePath)).digest("hex");
  evidence.add("file", { label, path: filePath, sha256: digest });
}

async function verifyPiFlags(evidence, pi, env, cwd) {
  const help = await runCommand(evidence, pi, ["--no-extensions", "--help"], { cwd, env: { ...env, PI_OFFLINE: "1" } });
  if (help.code !== 0) fail(`pi --help failed: ${bounded(help.stderr)}`);
  const requiredFlags = ["--mode", "--no-extensions", "--extension", "--session", "--session-dir", "--session-id", "--tools"];
  const missingFlags = requiredFlags.filter((flag) => !help.stdout.includes(flag));
  if (missingFlags.length > 0) fail(`installed Pi does not advertise required isolated RPC flags: ${missingFlags.join(", ")}`);
  evidence.add("pi-cli-contract", { requiredFlags, verified: true });
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  if (args.help) { process.stdout.write(usage); return; }
  const root = await ensureFreshRoot(args.root);
  const evidence = new Evidence(root);
  activeEvidence = evidence;
  const forgectl = await executable(args.forgectl, "--forgectl");
  const forgectlCommand = `'${forgectl.replaceAll("'", "'\\''")}'`;
  const codex = await executable(args.codex, "--codex");
  const pi = await executable(args.pi, "--pi");
  const websocketModulePath = await moduleFile(args.websocketModule);
  const websocketModule = await import(pathToFileURL(websocketModulePath).href);
  const WebSocket = websocketModule.default?.WebSocket ?? websocketModule.default ?? websocketModule.WebSocket;
  if (typeof WebSocket !== "function") fail("--websocket-module did not export a WebSocket constructor");
  const piExtensions = [];
  for (const path of args.piExtensions) piExtensions.push(await extensionFile(path));
  const ledger = join(root, "ledger");
  const coordinatorRoot = join(root, "coordinator");
  const piRoot = join(root, "pi");
  const piSessions = join(root, "pi-sessions");
  const codexSocket = join(root, "codex.sock");
  const piSocket = join(root, "pi.sock");
  const piRestartSocket = join(root, "pi-restart.sock");
  const piReceiptPath = join(root, "pi-receipts.jsonl");
  const piProxySocket = join(root, "pi-proxy.sock");
  const replacementSocket = join(root, "codex-replacement.sock");
  const codexServerArgs = (socket) => ["app-server", "--config", `projects.${JSON.stringify(coordinatorRoot)}.trust_level="untrusted"`, "--listen", `unix://${socket}`];
  for (const socketPath of [codexSocket, piSocket, piRestartSocket, piProxySocket, replacementSocket]) {
    if (socketPath.length > 103) fail(`Unix socket path exceeds macOS limit: ${socketPath}`);
  }
  await Promise.all([mkdir(ledger, { mode: 0o700 }), mkdir(coordinatorRoot, { mode: 0o700 }), mkdir(piRoot, { mode: 0o700 }), mkdir(piSessions, { mode: 0o700 })]);
  const inheritedEnv = { ...process.env };
  for (const key of ["FORGECTL_LEDGER", "FORGECTL_WORKER", "FORGECTL_COORDINATOR", "FORGECTL_RUN_ID", "FORGECTL_INBOX", "FORGECTL_INBOX_RECEIPTS", "FORGECTL_DRAIN_WORKER"]) delete inheritedEnv[key];
  const baseEnv = childEnv(inheritedEnv, {}, forgectl);
  await assertNoScratchTrust(evidence, root);
  await recordBinary(evidence, "forgectl", forgectl, baseEnv, coordinatorRoot);
  await recordBinary(evidence, "codex", codex, baseEnv, coordinatorRoot);
  await recordBinary(evidence, "pi", pi, baseEnv, piRoot);
  await recordFile(evidence, "websocket-module", websocketModulePath);
  await verifyPiFlags(evidence, pi, baseEnv, piRoot);
  const coordinator = await register(evidence, forgectl, "coordinator", ["--ledger", ledger, "--harness", "codex", "--worktree", coordinatorRoot, "--coordinator", "--socket", codexSocket], baseEnv, coordinatorRoot);
  let coordinatorEnv = childEnv(baseEnv, coordinator.environment, forgectl);
  let worker = await register(evidence, forgectl, "pi-worker", ["--ledger", ledger, "--harness", "pi", "--worktree", piRoot, "--socket", piProxySocket], baseEnv, piRoot);
  const extensionResult = await runCommand(evidence, forgectl, ["surface", "team", "extension", "--ledger", ledger, "--json"], { cwd: piRoot, env: baseEnv });
  if (extensionResult.code !== 0) fail(`extension export failed: ${bounded(extensionResult.stderr)}`);
  const extensionPath = lastJson(extensionResult.stdout)?.path;
  if (!extensionPath || extensionPath !== join(ledger, "forgectl-inbox.ts")) fail("extension export did not return the ledger-local extension path");
  const extensionInfo = await stat(extensionPath);
  if (!extensionInfo.isFile() || (extensionInfo.mode & 0o077) !== 0) fail("exported extension is not a private regular file");
  if (piExtensions.includes(extensionPath)) fail("--pi-extension must not repeat the exported forgectl extension");
  let proxy;
  let piChild;
  let piRpc;
  let codexChild;
  let codexRpc;
  let restartedPiChild;
  let replacementChild;
  let cleaned = false;
  activeCleanup = async () => {
    if (cleaned) return;
    cleaned = true;
    if (proxy) proxy.close();
    if (piRpc) piRpc.stop(); else if (piChild && !piChild.killed) piChild.kill("SIGTERM");
    if (codexRpc) codexRpc.stop(); else if (codexChild && !codexChild.killed) codexChild.kill("SIGTERM");
    if (restartedPiChild && !restartedPiChild.killed && restartedPiChild.exitCode === null) restartedPiChild.kill("SIGTERM");
    if (replacementChild && !replacementChild.killed && replacementChild.exitCode === null) replacementChild.kill("SIGTERM");
    if (piChild) await waitClosed(piChild);
    if (codexChild) await waitClosed(codexChild);
    if (restartedPiChild) await waitClosed(restartedPiChild);
    if (replacementChild) await waitClosed(replacementChild);
  };
  proxy = startAckDroppingProxy(evidence, piProxySocket, piSocket);
  const piSessionId = randomUUID();
  const piModelArgs = args["pi-provider"] ? ["--provider", args["pi-provider"], "--model", args["pi-model"]] : [];
  const piArgs = ["--mode", "rpc", ...piModelArgs, "--no-extensions", "-e", extensionPath, ...piExtensions.flatMap((path) => ["-e", path]), "--session-dir", piSessions, "--session-id", piSessionId, "--tools", "read,bash,find,grep"];
  let piEnv = childEnv(baseEnv, { ...worker.environment, FORGECTL_INBOX: piSocket, FORGECTL_INBOX_RECEIPTS: piReceiptPath }, forgectl);
  piChild = spawn(pi, piArgs, { cwd: piRoot, env: piEnv, stdio: ["pipe", "pipe", "pipe"] });
  piRpc = new JsonLinesProcess(piChild, evidence, "pi-rpc");
  const initialPiState = await piRpc.request({ type: "get_state" }, 30_000);
  const initialPiModel = initialPiState?.data?.model;
  const piSessionFile = initialPiState?.data?.sessionFile;
  if (!initialPiState?.data || !initialPiModel || typeof initialPiModel.provider !== "string" || typeof initialPiModel.id !== "string" || !Array.isArray(initialPiModel.input) || !Number.isInteger(initialPiModel.contextWindow) || !Number.isInteger(initialPiModel.maxTokens) || initialPiState.data.sessionId !== piSessionId || typeof piSessionFile !== "string" || !piSessionFile.startsWith(piSessions)) fail("Pi native get_state did not prove the requested session, session file, and model capabilities");
  if (piModelArgs.length && (initialPiModel.provider !== args["pi-provider"] || initialPiModel.id !== args["pi-model"])) fail("Pi silently selected a different provider or model");
  evidence.add("pi-native-state", { provider: initialPiModel.provider, modelId: initialPiModel.id, reasoning: Boolean(initialPiModel.reasoning), input: initialPiModel.input, contextWindow: initialPiModel.contextWindow, maxTokens: initialPiModel.maxTokens, sessionId: initialPiState.data.sessionId, sessionFile: piSessionFile, explicitExtensions: piExtensions });
  await piRpc.request({ type: "prompt", id: "pi-role", message: `You are the Pi worker in an explicitly enrolled forgectl team. Do not create files or run tools from this setup prompt. Wait for a [forgectl-msg] from the coordinator; when one arrives, follow its task instructions and ask for clarification before doing any artifact work. Reply only through ${forgectlCommand} surface send; use that exact absolute executable, not a shell alias or an installed copy.` }, DEFAULT_TIMEOUT);
  await piRpc.waitFor((event) => event.type === "agent_settled", DEFAULT_TIMEOUT);
  const codexEnv = coordinatorEnv;
  codexChild = spawn(codex, codexServerArgs(codexSocket), { cwd: coordinatorRoot, env: codexEnv, stdio: ["ignore", "ignore", "pipe"] });
  codexRpc = new CodexClient(codexChild, evidence, WebSocket);
  const socketReady = async (path) => {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      try { const socket = createConnection(path); await new Promise((resolvePromise, reject) => { socket.once("connect", resolvePromise); socket.once("error", reject); }); socket.destroy(); return; } catch { await sleep(100); }
    }
    fail(`native socket did not become ready: ${path}`);
  };
  await socketReady(piSocket);
  await socketReady(codexSocket);
  await codexRpc.connect(codexSocket);
  const init = await codexRpc.rpc("initialize", { clientInfo: { name: "forgectl-cross-harness-verifier", version: "1" }, capabilities: { experimentalApi: false } });
  evidence.add("codex-initialized", { protocol: init?.protocolVersion ?? init?.serverInfo?.version });
  codexRpc.notify("initialized", {});
  const thread = await codexRpc.rpc("thread/start", { cwd: coordinatorRoot, model: args["codex-model"], approvalPolicy: "never", sandbox: "read-only", ephemeral: true, threadSource: "startup" });
  const threadId = thread?.thread?.id ?? thread?.id;
  if (!threadId) fail("Codex thread/start returned no thread id");
  if (thread.model !== args["codex-model"]) fail("Codex silently selected a different model");
  await assertNoScratchTrust(evidence, root);
  evidence.add("codex-thread", { threadId, model: thread.model, cwd: coordinatorRoot, writableRoot: root, approvalPolicy: "never", initialSandbox: "read-only", turnSandbox: "workspace-write" });
  const rebound = await register(evidence, forgectl, "coordinator", ["--ledger", ledger, "--harness", "codex", "--worktree", coordinatorRoot, "--coordinator", "--socket", codexSocket, "--thread", threadId, "--run-id", coordinator.member.run_id, "--replace"], baseEnv, coordinatorRoot);
  if (rebound.member.run_id !== coordinator.member.run_id || rebound.member.thread_id !== threadId) fail("coordinator replacement changed the process incarnation or thread unexpectedly");
  const fixture = await createFixture(root);
  const utility = join(piRoot, "summarize-mailbox.mjs");
  const taskId = `cross-harness-${randomUUID()}`;
  const delegation = `You are the coordinator of an explicitly enrolled forgectl team. Delegate task ${taskId} to pi-worker by running exactly the available native command forgectl surface send pi-worker '<task text>'; your shell inherits FORGECTL_LEDGER=${ledger}, FORGECTL_COORDINATOR=coordinator, and FORGECTL_RUN_ID for this registered process. Do not create the worker artifact yourself. Tell Pi to ask one meaningful clarification before any artifact work, and require the clarification body to contain exactly CLARIFICATION ${taskId}. Then stop this turn and wait. The next coordinator turn will be started by the worker's queued reply; answer that clarification directly through forgectl surface send pi-worker with a body containing exactly ANSWER ${taskId}, preserving task ID ${taskId}. The worker must create ${utility} with Node builtins only, read fixture ${fixture} in forgectl mailbox JSONL shape, fold final statuses including unconfirmed, reject duplicate message-record IDs (duplicate op:msg IDs, not status records), append exactly one task execution record to ${join(piRoot, "worker-executions.jsonl")} shaped {"taskId":"${taskId}","executionId":"<unique>"}, and reply with a body containing exactly COMPLETED ${taskId}, its artifact path, and execution ID. The standalone utility stdout must be exactly one JSON object shaped {"counts":{"sent":1,"queued":1,"failed":1,"unconfirmed":1},"ids":["fixture-1","fixture-2","fixture-3","fixture-4"]} with no extra output. A successful delegation is transport acceptance, not proof of a completed turn; do not claim completion until the worker reply is visible in the inbox.`;
  const exactBinaryDelegation = delegation.replaceAll("forgectl surface", `${forgectlCommand} surface`);
  await codexRpc.turn(threadId, `${exactBinaryDelegation} Use that exact absolute executable; a shell alias or login profile may select an older installed copy. Your network-disabled sandbox may enqueue the message with exit 75 because it cannot connect to the local receiver socket. The task supervisor automatically flushes this private team mailbox after your turn. If queued, do not resend or expand permissions; end the turn and wait for the native reply.`, coordinatorRoot, root);
  await assertNoScratchTrust(evidence, root);
  const delegated = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "coordinator" && message.to === "pi-worker" && message.body.includes(taskId) && ["sent", "unconfirmed"].includes(message.status), 60_000);
  const droppedId = proxy.droppedId();
  const deliveryJournal = await readReceiptRecords(join(ledger, "mailbox.jsonl"));
  const uncertainAttempt = deliveryJournal.some((record) => record.op === "status" && record.id === delegated.id && record.status === "unconfirmed");
  if (droppedId !== delegated.id || !uncertainAttempt) fail(`proxy did not drop a durably tracked coordinator-to-Pi acknowledgement for ${delegated.id}`);
  evidence.add("lost-ack-target", { id: delegated.id, droppedId, observedStatus: delegated.status, uncertainAttempt });
  const delegatedReconciled = await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, delegated.id);
  evidence.add("lost-ack-reconciled", { id: delegated.id, result: delegatedReconciled });
  const delegatedReceiptRecords = (await readReceiptRecords(piReceiptPath)).filter((record) => record.id === delegated.id);
  const delegatedStates = delegatedReceiptRecords.map((record) => record.state);
  if (proxy.deliveryIds().filter((id) => id === delegated.id).length !== 1 || delegatedStates.filter((state) => state === "pending").length !== 1 || delegatedStates.filter((state) => state === "accepted").length !== 1) fail(`lost-ack delivery was not exactly one Pi API delivery with pending and accepted receipt: ${JSON.stringify({ deliveries: proxy.deliveryIds(), receipts: delegatedReceiptRecords })}`);
  const piMessagesResponse = await piRpc.request({ type: "get_messages", id: "messages-after-delegation" }, 30_000);
  const nativeMessages = piMessagesResponse?.data?.messages;
  if (!Array.isArray(nativeMessages)) fail("Pi get_messages returned an unexpected JSON shape");
  const nativeDelegationCount = nativeMessages.filter((message) => message?.role === "user" && contentText(message.content).includes(`id=${delegated.id}`)).length;
  if (nativeDelegationCount !== 1) fail(`Pi native session history contains ${nativeDelegationCount} copies of delegated message ${delegated.id}`);
  evidence.add("native-delivery-exactly-once", { id: delegated.id, proxyDeliveryCount: 1, receiptStates: delegatedStates, nativeHeaderCount: nativeDelegationCount });
  const clarification = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "pi-worker" && message.to === "coordinator" && ["sent", "unconfirmed"].includes(message.status) && message.body.includes(`CLARIFICATION ${taskId}`), 180_000);
  evidence.add("clarification-received", { id: clarification.id });
  const answer = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "coordinator" && message.to === "pi-worker" && message.status === "sent" && message.body.includes(`ANSWER ${taskId}`) && message.body !== delegated.body, 180_000);
  evidence.add("clarification-answered", { clarificationId: clarification.id, answerId: answer.id, mechanism: "codex-queue-started-native-next-turn" });
  const completed = await waitMailbox(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, (message) => message.from === "pi-worker" && message.to === "coordinator" && message.status === "sent" && message.body.includes(`COMPLETED ${taskId}`), 240_000);
  const messageOrder = (await readReceiptRecords(join(ledger, "mailbox.jsonl"))).filter((record) => record.op === "msg").map((record) => record.msg.id);
  const roundtripIds = [delegated.id, clarification.id, answer.id, completed.id];
  const roundtripPositions = assertRoundtripOrder(messageOrder, roundtripIds);
  evidence.add("task-roundtrip-order", { ids: roundtripIds, positions: roundtripPositions });
  const executionLog = join(piRoot, "worker-executions.jsonl");
  const executionRecords = (await readFile(executionLog, "utf8")).trim().split("\n").filter(Boolean).map((line) => JSON.parse(line));
  const executions = executionRecords.filter((record) => record.taskId === taskId);
  if (executions.length !== 1 || typeof executions[0].executionId !== "string" || Object.keys(executions[0]).sort().join(",") !== "executionId,taskId" || new Set(executions.map((record) => record.executionId)).size !== executions.length) fail(`worker execution log did not prove exactly-once execution for ${taskId}`);
  evidence.add("execution-log-verified", { taskId, executionId: executions[0].executionId, count: executions.length });
  const utilityResult = await runCommand(evidence, process.execPath, [utility, fixture], { cwd: piRoot, env: baseEnv, timeout: 30_000 });
  if (utilityResult.code !== 0) fail(`independent utility check failed: ${bounded(utilityResult.stderr)}`);
  const summary = lastJson(utilityResult.stdout);
  if (!summary || Object.keys(summary).sort().join(",") !== "counts,ids" || !summary.counts || Object.keys(summary.counts).sort().join(",") !== "failed,queued,sent,unconfirmed" || summary.counts.sent !== 1 || summary.counts.queued !== 1 || summary.counts.failed !== 1 || summary.counts.unconfirmed !== 1 || JSON.stringify(summary.ids) !== JSON.stringify(["fixture-1", "fixture-2", "fixture-3", "fixture-4"])) fail(`utility result did not prove final-status folding and exactly-once IDs: ${bounded(utilityResult.stdout)}`);
  const corrupt = join(root, "mailbox-corrupt.jsonl");
  const fixtureText = await readFile(fixture, "utf8");
  await writeFile(corrupt, `${fixtureText}${fixtureText.split("\n")[0]}\n`, { mode: 0o600 });
  const negative = await runCommand(evidence, process.execPath, [utility, corrupt], { cwd: piRoot, env: baseEnv, timeout: 30_000 });
  evidence.add("negative-control", { code: negative.code, rejected: negative.code !== 0, stderr: negative.stderr });
  if (negative.code === 0) fail("corrupted duplicate-ID fixture was accepted");
  const busyStartIndex = piRpc.lines.length;
  const busyToken = `busy-${randomUUID()}`;
  await piRpc.request({ type: "prompt", id: "busy-probe", message: `Use the bash tool to run sleep 5 for the busy-delivery probe. Do not read the mailbox, modify files, or send messages until the sleep finishes.` }, 30_000);
  await piRpc.waitForAfter((event) => event.type === "agent_start", busyStartIndex, 30_000);
  const busySend = await runCommand(evidence, forgectl, ["surface", "send", "pi-worker", `busy delivery probe ${busyToken}`, "--priority", "next", "--json"], { cwd: coordinatorRoot, env: coordinatorEnv });
  const busyReport = lastJson(busySend.stdout);
  if (!busyReport || !["sent", "unconfirmed"].includes(busyReport.status)) fail(`busy receiver did not accept a delivery: ${bounded(busySend.stdout)} ${bounded(busySend.stderr)}`);
  await piRpc.waitForAfter((event) => event.type === "agent_settled", busyStartIndex, DEFAULT_TIMEOUT);
  const busyReconciled = await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, busyReport.id, 60_000);
  evidence.add("busy-receiver", { id: busyReport.id, status: busyReport.status, reconciled: busyReconciled.message.status, token: busyToken });
  piRpc.stop();
  await waitClosed(piChild);
  proxy.close();
  const directWorker = await register(evidence, forgectl, "pi-worker", ["--ledger", ledger, "--harness", "pi", "--worktree", piRoot, "--socket", piRestartSocket, "--replace"], baseEnv, piRoot);
  worker = directWorker;
  piEnv = childEnv(baseEnv, { ...worker.environment, FORGECTL_INBOX_RECEIPTS: piReceiptPath }, forgectl);
  const stoppedProbe = await runCommand(evidence, forgectl, ["surface", "send", "pi-worker", `recovery probe ${taskId}`, "--priority", "next", "--json"], { cwd: coordinatorRoot, env: coordinatorEnv });
  const stoppedReport = lastJson(stoppedProbe.stdout);
  if (stoppedProbe.code !== 75 || !stoppedReport || stoppedReport.status !== "queued") fail(`stopped receiver was not queued with exit 75: ${bounded(stoppedProbe.stdout)} ${bounded(stoppedProbe.stderr)}`);
  evidence.add("stopped-receiver", { status: stoppedReport.status, code: stoppedProbe.code, id: stoppedReport.id });
  const piRestartArgs = ["--mode", "rpc", ...piModelArgs, "--no-extensions", "-e", extensionPath, ...piExtensions.flatMap((path) => ["-e", path]), "--session-dir", piSessions, "--session", piSessionFile, "--tools", "read,bash,find,grep"];
  restartedPiChild = spawn(pi, piRestartArgs, { cwd: piRoot, env: piEnv, stdio: ["pipe", "pipe", "pipe"] });
  piRpc = new JsonLinesProcess(restartedPiChild, evidence, "pi-rpc-restart");
  await socketReady(piRestartSocket);
  const restartedPiState = await piRpc.request({ type: "get_state" }, 30_000);
  if (restartedPiState?.data?.sessionId !== piSessionId || restartedPiState?.data?.sessionFile !== piSessionFile) fail("Pi restart did not reopen the saved native session file");
  const restartedMessages = (await piRpc.request({ type: "get_messages", id: "messages-after-restart" }, 30_000))?.data?.messages;
  const restartedDelegationCount = Array.isArray(restartedMessages) ? restartedMessages.filter((message) => message?.role === "user" && contentText(message.content).includes(`id=${delegated.id}`)).length : 0;
  if (restartedDelegationCount !== 1) fail(`Pi restart history did not preserve exactly one delegated header for ${delegated.id}`);
  evidence.add("pi-native-restart-state", { sessionId: restartedPiState.data.sessionId, sessionFile: restartedPiState.data.sessionFile, modelId: restartedPiState.data.model?.id, provider: restartedPiState.data.model?.provider, delegatedHeaderCount: restartedDelegationCount });
  const restartedFlush = await flushUntilSent(evidence, forgectl, ledger, coordinatorEnv, coordinatorRoot, stoppedReport.id);
  const recoveredProbe = restartedFlush.message;
  evidence.add("receiver-restart", { id: recoveredProbe.id, flush: restartedFlush });
  codexRpc.stop();
  await waitClosed(codexChild);
  const replacement = await register(evidence, forgectl, "coordinator", ["--ledger", ledger, "--harness", "codex", "--worktree", coordinatorRoot, "--coordinator", "--socket", replacementSocket, "--replace"], baseEnv, coordinatorRoot);
  const staleEvent = await runCommand(evidence, forgectl, ["surface", "event", "--harness", "codex", JSON.stringify({ type: "agent-turn-complete", "thread-id": threadId, "input-messages": ["not a title"] })], { cwd: coordinatorRoot, env: coordinatorEnv });
  if (staleEvent.code === 0) fail("retired coordinator run was allowed to report an event");
  const staleSend = await runCommand(evidence, forgectl, ["surface", "send", "pi-worker", `retired coordinator probe ${taskId}`, "--json"], { cwd: coordinatorRoot, env: coordinatorEnv });
  if (staleSend.code === 0) fail("retired coordinator run was allowed to send a message");
  evidence.add("retired-coordinator-rejected", { eventCode: staleEvent.code, sendCode: staleSend.code });
  replacementChild = spawn(codex, codexServerArgs(replacementSocket), { cwd: coordinatorRoot, env: childEnv(baseEnv, replacement.environment, forgectl), stdio: ["ignore", "ignore", "pipe"] });
  codexRpc = new CodexClient(replacementChild, evidence, WebSocket);
  await socketReady(replacementSocket);
  await codexRpc.connect(replacementSocket);
  await codexRpc.rpc("initialize", { clientInfo: { name: "forgectl-cross-harness-verifier-replacement", version: "1" }, capabilities: { experimentalApi: false } });
  codexRpc.notify("initialized", {});
  const replacementThread = await codexRpc.rpc("thread/start", { cwd: coordinatorRoot, model: args["codex-model"], approvalPolicy: "never", sandbox: "read-only", ephemeral: true, threadSource: "startup" });
  const replacementThreadId = replacementThread?.thread?.id ?? replacementThread?.id;
  if (!replacementThreadId) fail("replacement Codex thread/start returned no thread id");
  if (replacementThread.model !== args["codex-model"]) fail("replacement Codex silently selected a different model");
  const replacementRebound = await register(evidence, forgectl, "coordinator", ["--ledger", ledger, "--harness", "codex", "--worktree", coordinatorRoot, "--coordinator", "--socket", replacementSocket, "--thread", replacementThreadId, "--run-id", replacement.member.run_id, "--replace"], baseEnv, coordinatorRoot);
  if (replacementRebound.member.run_id !== replacement.member.run_id || replacementRebound.member.thread_id !== replacementThreadId) fail("replacement coordinator did not bind its new thread and run");
  evidence.add("coordinator-replacement", { oldRunId: coordinator.member.run_id, runId: replacement.member.run_id, oldThreadId: threadId, threadId: replacementThreadId, socket: replacementSocket });
  const replacementToken = `replacement-${randomUUID()}`;
  await codexRpc.turn(replacementThreadId, `You are the replacement coordinator. Read the current ledger and delegate the independent recovery check ${replacementToken} to pi-worker with ${forgectlCommand} surface send. Use that exact absolute executable, not a shell alias or an installed copy. Tell Pi to reply with exactly that token and do not modify files. This turn proves the newly registered coordinator thread, not the retired one, can deliver work. Your network-disabled sandbox may return queued with exit 75; the task supervisor automatically flushes this private mailbox after your turn, so do not resend or expand permissions.`, coordinatorRoot, root);
  const replacementEnv = childEnv(baseEnv, replacement.environment, forgectl);
  coordinatorEnv = replacementEnv;
  const replacementReply = await waitMailbox(evidence, forgectl, ledger, replacementEnv, coordinatorRoot, (message) => message.from === "pi-worker" && message.to === "coordinator" && message.status === "sent" && message.body.includes(replacementToken), 180_000);
  evidence.add("replacement-roundtrip", { token: replacementToken, replyId: replacementReply.id, threadId: replacementThreadId, runId: replacement.member.run_id });
  const report = await mailbox(evidence, forgectl, ledger, replacementEnv, coordinatorRoot);
  const taskMessages = (report.messages ?? []).filter((message) => message.body.includes(taskId));
  const ids = taskMessages.map((message) => message.id);
  if (new Set(ids).size !== ids.length) fail("task mailbox contains duplicate message IDs");
  const acceptedOrVisible = taskMessages.filter((message) => ["sent", "unconfirmed"].includes(message.status));
  evidence.add("acceptance-summary", { taskId, threadId, replacementThreadId, taskMessageIds: ids, visibleDeliveryStatuses: acceptedOrVisible.map((message) => ({ id: message.id, status: message.status })) });
  await activeCleanup();
  await assertNoScratchTrust(evidence, root);
  await evidence.save();
  await writeFile(join(root, "ACCEPTED"), `${JSON.stringify({ taskId, threadId, replacementThreadId, utility: utilityResult.code, negativeControl: negative.code }, null, 2)}\n`, { mode: 0o600 });
  process.stdout.write(`${JSON.stringify({ root, ledger, taskId, threadId, replacementThreadId, utility: "verified", negativeControl: "rejected", note: "native turn completion, mailbox delivery, worker claim, and independent verification are recorded separately" })}\n`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch(async (error) => {
    if (activeEvidence) activeEvidence.add("failure", { error: bounded(error.stack ?? error) });
    await activeCleanup().catch((cleanupError) => activeEvidence?.add("cleanup-failure", { error: bounded(String(cleanupError)) }));
    if (activeEvidence) await assertNoScratchTrust(activeEvidence, activeEvidence.root).catch((configError) => activeEvidence.add("configuration-check-failure", { error: bounded(String(configError)) }));
    if (activeEvidence) await activeEvidence.save().catch(() => undefined);
    process.stderr.write(`cross-harness acceptance failed: ${error.stack ?? error}\n`);
    process.exitCode = 1;
  });
}
