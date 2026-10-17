import { spawn } from "node:child_process";
import { createHash } from "node:crypto";

function digest(value) { return createHash("sha256").update(value).digest("hex"); }

export class HermesACPClient {
  constructor(command, cwd, env, evidence, name = "hermes-native-audit") {
    this.evidence = evidence;
    this.name = name;
    this.pending = new Map();
    this.updates = [];
    this.requestId = 0;
    this.buffer = "";
    this.closed = false;
    this.failure = undefined;
    this.child = spawn(command, [], { cwd, env, stdio: ["pipe", "pipe", "pipe"] });
    this.exit = new Promise((resolvePromise) => {
      this.child.once("close", (code, signal) => {
        this.closed = true;
        this.evidence.add("native-exit", { name, code, signal });
        this.rejectPending(new Error(`${name} exited ${code}/${signal}`));
        resolvePromise({ code, signal });
      });
    });
    this.child.once("error", (error) => this.fail(error));
    this.child.stdout.on("data", (chunk) => this.read(String(chunk)));
    this.child.stderr.on("data", (chunk) => this.evidence.add("native-stderr", { name, bytes: chunk.length, sha256: digest(chunk) }));
  }

  rejectPending(error) {
    for (const request of this.pending.values()) request.reject(error);
    this.pending.clear();
  }

  fail(error) {
    this.failure = error;
    this.rejectPending(error);
  }

  read(chunk) {
    this.buffer += chunk;
    if (this.buffer.length > 8 * 1024 * 1024) {
      this.fail(new Error("native ACP frame exceeded limit"));
      return;
    }
    while (this.buffer.includes("\n")) {
      const boundary = this.buffer.indexOf("\n");
      const line = this.buffer.slice(0, boundary);
      this.buffer = this.buffer.slice(boundary + 1);
      if (!line.trim()) continue;
      let message;
      try { message = JSON.parse(line); } catch {
        this.fail(new Error("native ACP stdout was not JSON-RPC"));
        return;
      }
      if (!message || typeof message !== "object" || message.jsonrpc !== "2.0") {
        this.fail(new Error("native ACP frame was not JSON-RPC 2.0"));
        return;
      }
      this.evidence.add("native-event", { name: this.name, message: { jsonrpc: message.jsonrpc, id: message.id, method: message.method, sessionId: message.params?.sessionId, updateKind: message.params?.update?.sessionUpdate, sha256: digest(line) } });
      if (message.method && Object.hasOwn(message, "id")) {
        const answer = message.method === "session/request_permission"
          ? { result: { outcome: { outcome: "cancelled" } } }
          : { error: { code: -32601, message: "unsupported client operation" } };
        this.child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id: message.id, ...answer })}\n`);
        this.evidence.add("hermes-client-request-denied", { method: message.method, name: this.name });
      } else if (Object.hasOwn(message, "id")) {
        const request = this.pending.get(message.id);
        if (!request) continue;
        this.pending.delete(message.id);
        if (message.error) request.reject(new Error(`native ACP ${request.method} failed with code ${Number.isInteger(message.error.code) ? message.error.code : "unknown"}`));
        else if (!Object.hasOwn(message, "result")) request.reject(new Error("native ACP reply lacks result"));
        else request.resolve(message.result);
      } else if (message.method === "session/update") {
        this.updates.push(message.params);
      }
    }
  }

  async rpc(method, params, timeout = 120_000) {
    if (this.failure) throw this.failure;
    if (this.closed) throw new Error("native ACP process is closed");
    const id = ++this.requestId;
    let timer;
    try {
      return await new Promise((resolvePromise, reject) => {
        timer = setTimeout(() => {
          this.pending.delete(id);
          reject(new Error(`native ACP ${method} timed out`));
        }, timeout);
        this.pending.set(id, { method, resolve: resolvePromise, reject });
        this.child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
      });
    } finally { clearTimeout(timer); }
  }

  async initialize(provider = "openai-codex") {
    const result = await this.rpc("initialize", { protocolVersion: 1, clientInfo: { name: "forgectl-hermes-verifier", version: "1" }, clientCapabilities: {} });
    if (result?.agentInfo?.name !== "hermes-agent" || typeof result.agentInfo.version !== "string" || !result.agentCapabilities?.loadSession || !result.authMethods?.some((method) => method.id === provider)) throw new Error("native Hermes handshake did not prove the expected provider and session loading");
    await this.rpc("authenticate", { methodId: provider });
    this.evidence.add("hermes-native-initialized", { name: this.name, version: result.agentInfo.version, provider });
    return result;
  }

  async history(sessionId, cwd, model) {
    const start = this.updates.length;
    const loaded = await this.rpc("session/load", { cwd, sessionId, mcpServers: [] });
    if (loaded?.models?.currentModelId !== model || loaded?.modes?.currentModeId !== "default") throw new Error("native Hermes load did not preserve the requested model and default permissions");
    return this.updates.slice(start).filter((update) => update?.sessionId === sessionId).map((update) => update.update);
  }

  async stop() {
    if (!this.closed) this.child.stdin.end();
    let timer;
    try {
      return await Promise.race([
        this.exit,
        new Promise((resolvePromise, reject) => { timer = setTimeout(() => { this.child.kill("SIGTERM"); reject(new Error("native ACP shutdown timed out")); }, 120_000); }),
      ]);
    } finally { clearTimeout(timer); }
  }
}
