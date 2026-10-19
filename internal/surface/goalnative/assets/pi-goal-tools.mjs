import { readFileSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { createConnection } from "node:net";

const TOOL_NAMES = ["goal_exec", "goal_read", "goal_write", "goal_mail", "goal_inbox"];
const TOOL_ATTESTATION = "forgectl_goal_tools";
const MAX_LINE_BYTES = 1 << 20;
const MAX_TEXT_BYTES = 128 * 1024;
const socketPath = requiredEnvironment("FORGECTL_GOAL_TOOL_SOCKET");
const capabilityPath = requiredEnvironment("FORGECTL_GOAL_TOOL_CAPABILITY_FILE");
const capability = loadCapability(capabilityPath);

function requiredEnvironment(name) {
  const value = process.env[name];
  if (typeof value !== "string" || value.length === 0 || !value.startsWith("/")) {
    throw new Error(`${name} is required for the native goal extension`);
  }
  return value;
}

function loadCapability(path) {
  let value;
  try {
    value = readFileSync(path, "utf8");
  } catch (error) {
    throw new Error(`native goal capability could not be read: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!/^[0-9a-f]{64}$/.test(value)) {
    throw new Error("native goal capability is invalid");
  }
  return value;
}

function textResult(value) {
  const text = typeof value === "string" ? value : JSON.stringify(value);
  if (typeof text !== "string" || Buffer.byteLength(text, "utf8") > MAX_TEXT_BYTES) {
    throw new Error("native goal tool response exceeds its bound");
  }
  return { content: [{ type: "text", text }], details: undefined };
}

function request(operation, args, signal) {
  if (!args || typeof args !== "object" || Array.isArray(args)) {
    throw new Error(`${operation} requires an object argument`);
  }
  const requestId = randomBytes(16).toString("hex");
  const payload = JSON.stringify({ version: 1, capability, request_id: requestId, operation, arguments: args });
  if (Buffer.byteLength(payload, "utf8") > MAX_LINE_BYTES) throw new Error("native goal request exceeds its bound");
  return new Promise((resolve, reject) => {
    const connection = createConnection(socketPath);
    let data = "";
    let settled = false;
    const finish = (error, value) => {
      if (settled) return;
      settled = true;
      if (signal) signal.removeEventListener("abort", abort);
      connection.destroy();
      error ? reject(error) : resolve(value);
    };
    const abort = () => finish(new Error("native goal tool call was cancelled"));
    if (signal?.aborted) return abort();
    signal?.addEventListener("abort", abort, { once: true });
    connection.setEncoding("utf8");
    connection.setTimeout(120000, () => finish(new Error("native goal tool call timed out")));
    connection.on("error", (error) => finish(new Error(`native goal tool transport failed: ${error.message}`)));
    connection.on("end", () => finish(new Error("native goal tool result is unconfirmed; do not replay")));
    connection.on("data", (chunk) => {
      data += chunk;
      if (Buffer.byteLength(data, "utf8") > MAX_LINE_BYTES) return finish(new Error("native goal tool response exceeds its bound"));
      const newline = data.indexOf("\n");
      if (newline < 0) return;
      const line = data.slice(0, newline);
      let response;
      try { response = JSON.parse(line); } catch { return finish(new Error("native goal tool response is invalid JSON")); }
      if (!response || response.version !== 1 || response.request_id !== requestId) return finish(new Error("native goal tool response identity mismatch"));
      if (typeof response.error === "string" && response.error.length > 0) return finish(new Error(response.error.slice(0, 2048)));
      if (Object.prototype.hasOwnProperty.call(response, "error") && response.error !== null && response.error !== undefined) return finish(new Error("native goal tool response has an invalid error"));
      if (!Object.prototype.hasOwnProperty.call(response, "result")) return finish(new Error("native goal tool response omitted its result"));
      finish(null, response.result);
    });
    connection.on("connect", () => connection.write(`${payload}\n`));
  });
}

function register(pi, name, description, parameters) {
  pi.registerTool({
    name,
    label: name,
    description,
    parameters,
    executionMode: "sequential",
    async execute(_toolCallId, args, signal) {
      return textResult(await request(name, args, signal));
    },
  });
}

function attestTools(pi) {
  const activeTools = pi.getActiveTools().slice().sort();
  if (activeTools.length !== TOOL_NAMES.length || activeTools.some((name, index) => name !== TOOL_NAMES.slice().sort()[index])) {
    throw new Error("native goal tool allowlist is not exact");
  }
  pi.appendEntry(TOOL_ATTESTATION, { version: 1, tools: activeTools });
}

export default function nativeGoalTools(pi) {
  const definitions = JSON.parse(readFileSync(new URL("./pi-goal-tools.json", import.meta.url), "utf8"));
  if (!Array.isArray(definitions) || definitions.length !== TOOL_NAMES.length || definitions.some((tool, index) => tool.name !== TOOL_NAMES[index])) {
    throw new Error("native goal tool definitions are not exact");
  }
  for (const definition of definitions) register(pi, definition.name, definition.description, definition.inputSchema);
  pi.on("input", async (event) => {
    if (typeof event?.text === "string" && event.text.trimStart().startsWith("/")) return { action: "handled" };
  });
  pi.on("session_start", async () => {
    if (typeof pi.setActiveTools === "function") pi.setActiveTools(TOOL_NAMES);
    attestTools(pi);
  });
  pi.on("turn_start", async () => attestTools(pi));
}
