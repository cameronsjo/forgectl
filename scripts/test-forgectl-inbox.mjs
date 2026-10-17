import assert from "node:assert/strict";
import { once } from "node:events";
import { lstat, mkdtemp, readFile, rm } from "node:fs/promises";
import { createConnection } from "node:net";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";
import { spawn } from "node:child_process";

const scriptPath = fileURLToPath(import.meta.url);
const extensionPath = join(dirname(scriptPath), "..", "internal", "surface", "mail", "assets", "forgectl-inbox.ts");

const pause = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

const pathExists = async (path) => {
  try {
    await lstat(path);
    return true;
  } catch (error) {
    if (error.code === "ENOENT") return false;
    throw error;
  }
};

const receiptContents = async (path) => {
  try {
    return await readFile(path, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return "";
    throw error;
  }
};

const exchange = (socketPath, request, timeout = 1500) => new Promise((resolve, reject) => {
  const socket = createConnection(socketPath);
  let buffer = "";
  let errors = "";
  let settled = false;
  const finish = (error, response) => {
    if (settled) return;
    settled = true;
    socket.destroy();
    if (error) reject(error);
    else resolve(response);
  };
  socket.setEncoding("utf8");
  socket.setTimeout(timeout, () => finish(new Error(`socket exchange timed out: ${socketPath}`)));
  socket.on("connect", () => socket.write(`${typeof request === "string" ? request : JSON.stringify(request)}\n`));
  socket.on("data", (chunk) => {
    buffer += chunk;
    const newline = buffer.indexOf("\n");
    if (newline < 0) return;
    try {
      finish(null, JSON.parse(buffer.slice(0, newline)));
    } catch (error) {
      finish(error);
    }
  });
  socket.on("error", (error) => finish(error));
  socket.on("close", () => {
    if (!settled) finish(new Error("socket closed without a response"));
  });
});

const sendAndForget = (socketPath, request) => new Promise((resolve, reject) => {
  const socket = createConnection(socketPath);
  socket.setTimeout(1500, () => {
    socket.destroy();
    reject(new Error("lost-ack request timed out before write"));
  });
  socket.on("connect", () => {
    socket.write(`${JSON.stringify(request)}\n`, () => setTimeout(() => {
      socket.destroy();
      resolve();
    }, 100));
  });
  socket.on("error", reject);
});

const spawnHarness = (directory, socketName, mode, identity = { worker: `pi-${mode}` }) => {
  const socketPath = join(directory, socketName);
  const receiptPath = `${socketPath}.receipts`;
  const identityEnv = {};
  if (identity.worker !== undefined) identityEnv.FORGECTL_WORKER = identity.worker;
  if (identity.coordinator !== undefined) identityEnv.FORGECTL_COORDINATOR = identity.coordinator;
  if (identity.ledger !== undefined) identityEnv.FORGECTL_LEDGER = identity.ledger;
  if (identity.runId !== undefined) identityEnv.FORGECTL_RUN_ID = identity.runId;
  if (identity.drainWorker !== undefined) identityEnv.FORGECTL_DRAIN_WORKER = identity.drainWorker;
  const childEnvironment = { ...process.env };
  for (const key of ["FORGECTL_WORKER", "FORGECTL_COORDINATOR", "FORGECTL_LEDGER", "FORGECTL_RUN_ID", "FORGECTL_DRAIN_WORKER"]) delete childEnvironment[key];
  const child = spawn(process.execPath, ["--experimental-strip-types", scriptPath, "--child", mode], {
    env: {
      ...childEnvironment,
      NODE_NO_WARNINGS: "1",
      FORGECTL_INBOX: socketPath,
      FORGECTL_INBOX_RECEIPTS: receiptPath,
      ...identityEnv,
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  let buffer = "";
  let errors = "";
  const waiters = [];
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    buffer += chunk;
    while (true) {
      const newline = buffer.indexOf("\n");
      if (newline < 0) break;
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      const waiter = waiters.shift();
      if (waiter) waiter(JSON.parse(line));
    }
  });
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk) => {
    errors += chunk;
  });
  const command = (request) => new Promise((resolve, reject) => {
    if (child.exitCode !== null) {
      reject(new Error(`harness exited with ${child.exitCode}`));
      return;
    }
    waiters.push(resolve);
    child.stdin.write(`${JSON.stringify(request)}\n`, (error) => {
      if (error) reject(error);
    });
  });
  const stop = async (signal = "SIGTERM") => {
    if (child.exitCode === null) child.kill(signal);
    if (child.exitCode === null) await once(child, "exit");
  };
  const exchangeRequest = (request) => identity.runId
    ? { ...request, v: 2, run_id: identity.runId, session_id: request.session_id ?? (mode.startsWith("wrong-") ? "session-2" : "session-1") }
    : request;
  const exchangeForHarness = (request, timeout) => exchange(socketPath, exchangeRequest(request), timeout);
  return { child, command, errors: () => errors, receiptPath, socketPath, stop, exchange: exchangeForHarness };
};

const testCoordinatorIdentityActivates = async (directory) => {
  const harness = spawnHarness(directory, "coordinator.sock", "message-start", { coordinator: "pi-coordinator", ledger: directory, runId: "run-1" });
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await harness.exchange({ type: "deliver", id: "coordinator-1", text: "native" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    await harness.command({ type: "event", name: "message_start", text: "native" });
    const receipt = await harness.exchange({ type: "receipt", id: "coordinator-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, true);
    const state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
  } finally {
    await harness.stop();
  }
};

const testBoundIdentityRefusals = async (directory) => {
  const harness = spawnHarness(directory, "bound-identity.sock", "message-start", { worker: "pi-bound", runId: "run-bound" });
  const request = (type, id, overrides = {}) => ({ v: 2, type, id, run_id: "run-bound", session_id: "session-1", ...overrides });
  try {
    await waitForSocket(harness.socketPath);
    const emptyReceipts = await receiptContents(harness.receiptPath);
    for (const rejected of [
      { v: 1, type: "deliver", id: "bound-v1", text: "legacy" },
      request("deliver", "bound-run", { run_id: "wrong-run", text: "wrong run" }),
      request("deliver", "bound-session", { session_id: "wrong-session", text: "wrong session" }),
    ]) {
      const response = await exchange(harness.socketPath, rejected);
      assert.equal(response.ok, false);
      assert.equal(await receiptContents(harness.receiptPath), emptyReceipts);
    }
    for (const rejected of [
      { v: 1, type: "inspect", id: "bound-inspect-v1" },
      request("inspect", "bound-inspect-run", { run_id: "wrong-run" }),
      request("inspect", "bound-inspect-session", { session_id: "wrong-session" }),
    ]) {
      const response = await exchange(harness.socketPath, rejected);
      assert.equal(response.ok, false);
      assert.equal(await receiptContents(harness.receiptPath), emptyReceipts);
    }
    let state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 0);

    const delivery = await harness.exchange({ type: "deliver", id: "bound-positive", text: "native" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    await harness.command({ type: "event", name: "message_start", text: "native" });
    const receipt = await harness.exchange({ type: "receipt", id: "bound-positive" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.v, 2);
    assert.equal(receipt.run_id, "run-bound");
    assert.equal(receipt.session_id, "session-1");
    const acceptedReceipts = await receiptContents(harness.receiptPath);

    for (const rejected of [
      request("receipt", "bound-positive", { run_id: "wrong-run" }),
      request("receipt", "bound-positive", { session_id: "wrong-session" }),
      request("state", "bound-state", { run_id: "wrong-run" }),
      request("state", "bound-state-session", { session_id: "wrong-session" }),
    ]) {
      const response = await exchange(harness.socketPath, rejected);
      assert.equal(response.ok, false);
      assert.equal(await receiptContents(harness.receiptPath), acceptedReceipts);
    }
    state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
    const currentState = await harness.exchange({ type: "state", id: "bound-state-current" });
    assert.equal(currentState.ok, true);
    assert.equal(currentState.v, 2);
  } finally {
    await harness.stop();
  }
};

const testNativeInspectionReflectsCurrentMetadata = async (directory) => {
  const harness = spawnHarness(directory, "inspect.sock", "inspect", { worker: "pi-inspect", runId: "run-inspect" });
  try {
    await waitForSocket(harness.socketPath);
    const initial = await harness.exchange({ type: "inspect", id: "inspect-1" });
    assert.equal(initial.ok, true);
    assert.equal(initial.known, true);
    assert.equal(initial.run_id, "run-inspect");
    assert.equal(initial.session_id, "session-1");
    assert.equal(initial.provider, "openai-codex");
    assert.equal(initial.model_id, "gpt-initial");
    assert.deepEqual(initial.active_tools, ["read", "bash"]);
    assert.equal(initial.session_path, "/tmp/pi-session-1.jsonl");

    await harness.command({ type: "set-model", provider: "openai-codex", model: "gpt-changed" });
    const changed = await harness.exchange({ type: "inspect", id: "inspect-2" });
    assert.equal(changed.ok, true);
    assert.equal(changed.model_id, "gpt-changed");
    const calls = await harness.command({ type: "calls" });
    assert.equal(calls.calls.length, 0);
  } finally {
    await harness.stop();
  }
};

const testNativeInspectionUnknownMetadata = async (directory) => {
  for (const mode of ["missing-model", "missing-session-path", "missing-tools"]) {
    const harness = spawnHarness(directory, `${mode}.sock`, mode, { worker: `pi-${mode}`, runId: `run-${mode}` });
    try {
      await waitForSocket(harness.socketPath);
      const response = await harness.exchange({ type: "inspect", id: `${mode}-inspect` });
      assert.equal(response.ok, false);
      assert.equal(response.unknown, true);
      const calls = await harness.command({ type: "calls" });
      assert.equal(calls.calls.length, 0);
    } finally {
      await harness.stop();
    }
  }
};

const testSettledFailureIsNeverReportedCompleted = async (directory) => {
  const harness = spawnHarness(directory, "inspect-failure.sock", "settled-failure", { worker: "pi-inspect-failure", runId: "run-inspect-failure" });
  try {
    await waitForSocket(harness.socketPath);
    await harness.command({ type: "event", name: "agent_start" });
    await harness.command({ type: "event", name: "turn_end" });
    await harness.command({ type: "event", name: "agent_settled" });
    const response = await harness.exchange({ type: "inspect", id: "inspect-failure" });
    assert.equal(response.ok, true);
    assert.equal(response.turn.status, "failed");
    assert.equal(response.turn.error, "error");
    assert.equal(response.turn.id, "entry-1");
    assert.notEqual(response.turn.status, "completed");
  } finally {
    await harness.stop();
  }
};

const testNativeInspectionRequiresSettledExactEntries = async (directory) => {
  const harness = spawnHarness(directory, "inspect-entries.sock", "inspect", { worker: "pi-entries", runId: "run-entries" });
  try {
    await waitForSocket(harness.socketPath);
    const before = await receiptContents(harness.receiptPath);
    await harness.command({ type: "event", name: "agent_start" });
    await harness.command({ type: "event", name: "agent_settled" });
    assert.equal((await harness.exchange({ type: "inspect" })).turn, undefined);
    const entryIDs = [];
    for (const expected of ["entry-1", "entry-2"]) {
      await harness.command({ type: "event", name: "agent_start" });
      await harness.command({ type: "event", name: "turn_end", turnIndex: 0 });
      assert.equal((await harness.exchange({ type: "inspect" })).turn, undefined);
      await harness.command({ type: "event", name: "agent_settled" });
      const observed = await harness.exchange({ type: "inspect" });
      assert.equal(observed.turn.id, expected);
      assert.equal(observed.turn.status, "completed");
      assert.ok(Number.isFinite(Date.parse(observed.turn.observed_at)));
      entryIDs.push(observed.turn.id);
    }
    assert.equal(new Set(entryIDs).size, 2);
    for (const stopReason of ["length", "toolUse", "unexpected"]) {
      await harness.command({ type: "event", name: "agent_start" });
      await harness.command({ type: "event", name: "turn_end", stopReason });
      await harness.command({ type: "event", name: "agent_settled" });
      assert.equal((await harness.exchange({ type: "inspect" })).turn, undefined);
    }
    await harness.command({ type: "event", name: "agent_start" });
    await harness.command({ type: "event", name: "turn_end", missingEntry: true });
    await harness.command({ type: "event", name: "agent_settled" });
    assert.equal((await harness.exchange({ type: "inspect" })).turn, undefined);
    await harness.command({ type: "set-model", provider: "openai-codex", model: "gpt\u202e" });
    assert.equal((await harness.exchange({ type: "inspect" })).known, undefined);
    assert.equal(await receiptContents(harness.receiptPath), before);
    assert.equal((await harness.command({ type: "calls" })).calls.length, 0);
  } finally {
    await harness.stop();
  }
};

const testSessionSwitchDuringNativeObservationStaysUnconfirmed = async (directory) => {
  const harness = spawnHarness(directory, "session-switch.sock", "session-switch", { worker: "pi-switch", runId: "run-switch" });
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await harness.exchange({ type: "deliver", id: "session-switch-1", text: "switch while waiting" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    const receipt = await exchange(harness.socketPath, {
      v: 2,
      type: "receipt",
      id: "session-switch-1",
      run_id: "run-switch",
      session_id: "session-2",
    });
    assert.equal(receipt.ok, false);
    assert.equal(receipt.unconfirmed, true);
    const state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
    const records = (await receiptContents(harness.receiptPath)).trim().split("\n").filter(Boolean).map((line) => JSON.parse(line));
    assert.equal(records.at(-1).state, "accepted");
    assert.equal(records.at(-1).sessionId, "session-1");
  } finally {
    await harness.stop();
  }
};

const testRejectedReceiptIdentityRebinding = async (directory) => {
  const first = spawnHarness(directory, "rejected-rebind.sock", "reject", { worker: "pi-rebind", runId: "run-rebind" });
  try {
    await waitForSocket(first.socketPath);
    const firstDelivery = await first.exchange({ type: "deliver", id: "rejected-rebind-1", text: "retryable" });
    assert.equal(firstDelivery.ok, false);
    assert.equal(firstDelivery.retry, true);
    const secondDelivery = await first.exchange({ type: "deliver", id: "rejected-rebind-1", text: "retryable" });
    assert.equal(secondDelivery.ok, false);
    assert.equal(secondDelivery.retry, true);
    const sameRunReceipts = await receiptContents(first.receiptPath);
    const sameRunState = await first.command({ type: "calls" });
    assert.equal(sameRunState.calls.length, 2);
    await first.stop();

    await rm(first.socketPath, { force: true });
    const changedSession = spawnHarness(directory, "rejected-rebind.sock", "wrong-reject", { worker: "pi-rebind", runId: "run-rebind" });
    try {
      await waitForSocket(changedSession.socketPath);
      const response = await changedSession.exchange({ type: "deliver", id: "rejected-rebind-1", text: "retryable" });
      assert.equal(response.ok, false);
      assert.equal(response.unconfirmed, true);
      assert.equal(await receiptContents(changedSession.receiptPath), sameRunReceipts);
      const state = await changedSession.command({ type: "calls" });
      assert.equal(state.calls.length, 0);
    } finally {
      await changedSession.stop();
    }

    await rm(changedSession.socketPath, { force: true });
    const changedRun = spawnHarness(directory, "rejected-rebind.sock", "accepted", { worker: "pi-rebind", runId: "run-new" });
    try {
      await waitForSocket(changedRun.socketPath);
      const response = await changedRun.exchange({ type: "deliver", id: "rejected-rebind-1", text: "retryable" });
      assert.equal(response.ok, false);
      assert.equal(response.unconfirmed, true);
      assert.equal(await receiptContents(changedRun.receiptPath), sameRunReceipts);
      const state = await changedRun.command({ type: "calls" });
      assert.equal(state.calls.length, 0);
    } finally {
      await changedRun.stop();
    }
  } finally {
    if (first.child.exitCode === null && first.child.signalCode === null) await first.stop("SIGKILL");
  }
};

const testWorkerDrainIdentityActivates = async (directory) => {
  const harness = spawnHarness(directory, "worker-drain.sock", "message-start", { worker: "pi-worker", drainWorker: "1" });
  try {
    const initialized = await harness.command({ type: "calls" });
    assert.ok(initialized.handlers.includes("session_start"));
    await waitForSocket(harness.socketPath);
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "worker-drain-1", text: "native" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    await harness.command({ type: "event", name: "message_start", text: "native" });
    const receipt = await exchange(harness.socketPath, { v: 1, type: "receipt", id: "worker-drain-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, true);
  } finally {
    await harness.stop();
  }
};

const testInvalidCoordinatorIdentitiesStayInert = async (directory) => {
  const cases = [
    ["missing-identity", { ledger: directory, runId: "run-1" }],
    ["missing-ledger", { coordinator: "pi-coordinator", runId: "run-1" }],
    ["missing-run", { coordinator: "pi-coordinator", ledger: directory }],
    ["blank-run", { coordinator: "pi-coordinator", ledger: directory, runId: "   " }],
    ["relative-ledger", { coordinator: "pi-coordinator", ledger: "relative-ledger", runId: "run-1" }],
    ["coordinator-worker", { coordinator: "pi-coordinator", worker: "pi-worker", ledger: directory, runId: "run-1" }],
    ["coordinator-drain-worker", { coordinator: "pi-coordinator", ledger: directory, runId: "run-1", drainWorker: "1" }],
  ];
  for (const [mode, identity] of cases) {
    const harness = spawnHarness(directory, `${mode}.sock`, mode, identity);
    try {
      const state = await harness.command({ type: "calls" });
      assert.deepEqual(state.calls, []);
      assert.deepEqual(state.handlers, []);
      await assertSocketUnavailable(harness.socketPath);
      assert.equal(await pathExists(harness.receiptPath), false);
    } finally {
      await harness.stop();
    }
  }
};

const waitForSocket = async (socketPath) => {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    try {
      await exchange(socketPath, { v: 1, type: "state", id: "ready" }, 250);
      return;
    } catch {
      await pause(25);
    }
  }
  throw new Error(`socket did not become ready: ${socketPath}`);
};

const assertSocketUnavailable = async (socketPath) => {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    try {
      await exchange(socketPath, { v: 1, type: "state", id: "unexpected" }, 100);
    } catch {
      return;
    }
    await pause(25);
  }
  throw new Error(`stale socket unexpectedly became available: ${socketPath}`);
};

const waitForPendingReceipt = async (socketPath, id) => {
  for (let attempt = 0; attempt < 80; attempt += 1) {
    try {
      const receipt = await exchange(socketPath, { v: 1, type: "receipt", id }, 250);
      if (receipt.ok === false && receipt.unconfirmed === true) return;
    } catch {
      // The child may still be starting its socket listener.
    }
    await pause(25);
  }
  throw new Error(`receipt did not become pending: ${id}`);
};

const childMain = async (mode) => {
  const calls = [];
  const handlers = new Map();
  let currentSession = mode === "missing-session" ? undefined : mode.startsWith("wrong-") ? "session-2" : "session-1";
  let currentModel = mode === "missing-model" ? undefined : { provider: "openai-codex", id: "gpt-initial" };
  let currentTools = mode === "missing-tools" ? undefined : ["read", "bash"];
  let currentSessionFile = mode === "missing-session-path" ? undefined : "/tmp/pi-session-1.jsonl";
  let leafEntry;
  let entryNumber = 0;
  const sessionManager = {
    getSessionId: () => currentSession,
    getSessionFile: () => currentSessionFile,
    getLeafEntry: () => leafEntry,
    getEntries: () => (mode === "history" || mode === "wrong-history") ? [{ type: "message", message: { role: "user", content: "may have arrived" } }] : [],
  };
  const eventContext = { sessionManager };
  Object.defineProperty(eventContext, "model", { get: () => currentModel });
  const pi = {
    on(event, handler) {
      handlers.set(event, handler);
    },
    sendUserMessage(text, options) {
      calls.push({ text, options });
      if (mode === "pending") return new Promise(() => undefined);
      if (mode === "reject") throw new Error("definite pi rejection");
      if (mode === "runtime-failure") return Promise.reject(new Error("runtime send failure"));
      if (mode === "session-switch") {
        setTimeout(() => {
          handlers.get("message_start")?.({ type: "message_start", message: { role: "user", content: text } }, eventContext);
          currentSession = "session-2";
          handlers.get("session_start")?.({ type: "session_start", reason: "switch" }, eventContext);
        }, 10);
      }
      return undefined;
    },
    getActiveTools() {
      if (!currentTools) throw new Error("active tools unavailable");
      return currentTools;
    },
  };
  const extension = await import(pathToFileURL(extensionPath).href);
  extension.default(pi);
  await handlers.get("session_start")?.({ type: "session_start", reason: "startup" }, eventContext);
  let input = "";
  process.stdin.setEncoding("utf8");
  process.stdin.on("data", async (chunk) => {
    input += chunk;
    while (true) {
      const newline = input.indexOf("\n");
      if (newline < 0) break;
      const line = input.slice(0, newline);
      input = input.slice(newline + 1);
      const request = JSON.parse(line);
      if (request.type === "calls") process.stdout.write(`${JSON.stringify({ calls, handlers: [...handlers.keys()] })}\n`);
      else if (request.type === "set-model") {
        currentModel = { provider: request.provider, id: request.model };
        process.stdout.write(`${JSON.stringify({ ok: true })}\n`);
      }
      else if (request.type === "event") {
        const handler = handlers.get(request.name);
        const event = request.name === "message_start"
          ? { type: request.name, message: { role: "user", content: request.text } }
          : request.name === "turn_end"
            ? { type: request.name, turnIndex: request.turnIndex ?? 7, message: { role: "assistant", stopReason: request.stopReason ?? (mode === "settled-failure" ? "error" : "stop"), errorMessage: mode === "settled-failure" ? "native failure" : undefined } }
            : { type: request.name };
        if (request.name === "turn_end") {
          entryNumber += 1;
          leafEntry = request.missingEntry ? undefined : { type: "message", id: `entry-${entryNumber}`, timestamp: new Date().toISOString(), message: event.message };
        }
        await Promise.resolve(handler?.(event, eventContext));
        process.stdout.write(`${JSON.stringify({ ok: true })}\n`);
      } else if (request.type === "exit") process.exit(0);
    }
  });
  process.stdin.resume();
};

const testVoidSendStaysPending = async (directory) => {
  const harness = spawnHarness(directory, "void.sock", "void");
  try {
    try {
      await waitForSocket(harness.socketPath);
    } catch (error) {
      throw new Error(`${error.message}; child stderr: ${harness.errors()}`);
    }
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "void-1", text: "first" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    const receipt = await exchange(harness.socketPath, { v: 1, type: "receipt", id: "void-1" });
    assert.equal(receipt.ok, false);
    assert.equal(receipt.unconfirmed, true);
    const duplicate = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "void-1", text: "first" });
    assert.equal(duplicate.ok, false);
    assert.equal(duplicate.unconfirmed, true);
    const changed = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "void-1", text: "changed" });
    assert.equal(changed.ok, false);
    const state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
  } finally {
    await harness.stop("SIGKILL");
  }
};

const testAsyncRuntimeFailureStaysPending = async (directory) => {
  const harness = spawnHarness(directory, "runtime-failure.sock", "runtime-failure");
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "runtime-1", text: "failure" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    const receipt = await exchange(harness.socketPath, { v: 1, type: "receipt", id: "runtime-1" });
    assert.equal(receipt.ok, false);
    assert.equal(receipt.unconfirmed, true);
  } finally {
    await harness.stop();
  }
};

const testNativeMessageStartAcceptance = async (directory) => {
  const harness = spawnHarness(directory, "message-start.sock", "message-start");
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "native-1", text: "native" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    await harness.command({ type: "event", name: "message_start", text: "native" });
    const receipt = await exchange(harness.socketPath, { v: 1, type: "receipt", id: "native-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, true);
    assert.equal(receipt.evidence, "message_start");
    const duplicate = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "native-1", text: "native" });
    assert.equal(duplicate.ok, true);
    assert.equal(duplicate.accepted, true);
    const state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
  } finally {
    await harness.stop();
  }
};

const testMissingSessionIdentityStaysRetryable = async (directory) => {
  const harness = spawnHarness(directory, "missing-session.sock", "missing-session");
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "missing-session-1", text: "cannot bind" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.retry, true);
    const state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 0);
  } finally {
    await harness.stop();
  }
};

const testPendingRestart = async (directory) => {
  const pending = spawnHarness(directory, "pending.sock", "pending");
  try {
    await waitForSocket(pending.socketPath);
    await sendAndForget(pending.socketPath, { v: 1, type: "deliver", id: "pending-1", text: "may have arrived" });
    await waitForPendingReceipt(pending.socketPath, "pending-1");
  } finally {
    await pending.stop("SIGKILL");
  }
  await rm(pending.socketPath, { force: true });
  const restarted = spawnHarness(directory, "pending.sock", "accepted");
  try {
    await waitForSocket(restarted.socketPath);
    const receipt = await exchange(restarted.socketPath, { v: 1, type: "receipt", id: "pending-1" });
    assert.equal(receipt.ok, false);
    assert.equal(receipt.unconfirmed, true);
    const duplicate = await exchange(restarted.socketPath, { v: 1, type: "deliver", id: "pending-1", text: "may have arrived" });
    assert.equal(duplicate.ok, false);
    assert.equal(duplicate.unconfirmed, true);
    const state = await restarted.command({ type: "calls" });
    assert.equal(state.calls.length, 0);
  } finally {
    await restarted.stop();
  }
};

const testPendingHistoryReconcile = async (directory) => {
  const pending = spawnHarness(directory, "history.sock", "void");
  try {
    await waitForSocket(pending.socketPath);
    await sendAndForget(pending.socketPath, { v: 1, type: "deliver", id: "history-1", text: "may have arrived" });
    await waitForPendingReceipt(pending.socketPath, "history-1");
  } finally {
    await pending.stop("SIGKILL");
  }
  await rm(pending.socketPath, { force: true });
  const restarted = spawnHarness(directory, "history.sock", "history");
  try {
    await waitForSocket(restarted.socketPath);
    const receipt = await exchange(restarted.socketPath, { v: 1, type: "receipt", id: "history-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, true);
    assert.equal(receipt.evidence, "history");
    const duplicate = await exchange(restarted.socketPath, { v: 1, type: "deliver", id: "history-1", text: "may have arrived" });
    assert.equal(duplicate.ok, true);
    assert.equal(duplicate.accepted, true);
    const state = await restarted.command({ type: "calls" });
    assert.equal(state.calls.length, 0);
  } finally {
    await restarted.stop();
  }
};

const testHistoryFromWrongSessionStaysUnconfirmed = async (directory) => {
  const pending = spawnHarness(directory, "wrong-history.sock", "void");
  try {
    await waitForSocket(pending.socketPath);
    await sendAndForget(pending.socketPath, { v: 1, type: "deliver", id: "wrong-history-1", text: "may have arrived" });
    await waitForPendingReceipt(pending.socketPath, "wrong-history-1");
  } finally {
    await pending.stop("SIGKILL");
  }
  await rm(pending.socketPath, { force: true });
  const restarted = spawnHarness(directory, "wrong-history.sock", "wrong-history");
  try {
    await waitForSocket(restarted.socketPath);
    const receipt = await exchange(restarted.socketPath, { v: 1, type: "receipt", id: "wrong-history-1" });
    assert.equal(receipt.ok, false);
    assert.equal(receipt.unconfirmed, true);
    const duplicate = await exchange(restarted.socketPath, { v: 1, type: "deliver", id: "wrong-history-1", text: "may have arrived" });
    assert.equal(duplicate.ok, false);
    assert.equal(duplicate.unconfirmed, true);
    const state = await restarted.command({ type: "calls" });
    assert.equal(state.calls.length, 0);
  } finally {
    await restarted.stop();
  }
};

const testAcceptedReceiptFromWrongSessionStaysUnconfirmed = async (directory) => {
  const accepted = spawnHarness(directory, "wrong-session.sock", "message-start");
  try {
    await waitForSocket(accepted.socketPath);
    const delivery = await exchange(accepted.socketPath, { v: 1, type: "deliver", id: "wrong-session-1", text: "native" });
    assert.equal(delivery.ok, false);
    await accepted.command({ type: "event", name: "message_start", text: "native" });
    const receipt = await exchange(accepted.socketPath, { v: 1, type: "receipt", id: "wrong-session-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.session_id, "session-1");
  } finally {
    await accepted.stop("SIGKILL");
  }
  await rm(accepted.socketPath, { force: true });
  const restarted = spawnHarness(directory, "wrong-session.sock", "wrong-session");
  try {
    await waitForSocket(restarted.socketPath);
    const receipt = await exchange(restarted.socketPath, { v: 1, type: "receipt", id: "wrong-session-1" });
    assert.equal(receipt.ok, false);
    assert.equal(receipt.unconfirmed, true);
    const duplicate = await exchange(restarted.socketPath, { v: 1, type: "deliver", id: "wrong-session-1", text: "native" });
    assert.equal(duplicate.ok, false);
    assert.equal(duplicate.unconfirmed, true);
    const state = await restarted.command({ type: "calls" });
    assert.equal(state.calls.length, 0);
  } finally {
    await restarted.stop();
  }
};

const testSocketOwnershipAndMalformedRequests = async (directory) => {
  const owner = spawnHarness(directory, "ownership.sock", "accepted");
  try {
    await waitForSocket(owner.socketPath);
    const contender = spawnHarness(directory, "ownership.sock", "accepted");
    try {
      await pause(650);
      const state = await exchange(owner.socketPath, { v: 1, type: "state", id: "owner" });
      assert.equal(state.ok, true);
    } finally {
      await contender.stop("SIGKILL");
    }
    for (const malformed of ["null", "[]"]) {
      const response = await exchange(owner.socketPath, malformed);
      assert.equal(response.ok, false);
      assert.match(response.error, /object/);
    }
    const state = await exchange(owner.socketPath, { v: 1, type: "state", id: "still-alive" });
    assert.equal(state.ok, true);
  } finally {
    await owner.stop("SIGKILL");
  }
  const restarted = spawnHarness(directory, "ownership.sock", "accepted");
  try {
    await assertSocketUnavailable(restarted.socketPath);
  } finally {
    await restarted.stop();
  }
};

const testDefiniteRejection = async (directory) => {
  const harness = spawnHarness(directory, "reject.sock", "reject");
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "reject-1", text: "rejected" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.retry, true);
    const receipt = await exchange(harness.socketPath, { v: 1, type: "receipt", id: "reject-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, false);
    assert.equal(receipt.known, true);
  } finally {
    await harness.stop();
  }
};

const testRejectedReceiptRestartsAndRetries = async (directory) => {
  const rejected = spawnHarness(directory, "reject-restart.sock", "reject");
  try {
    await waitForSocket(rejected.socketPath);
    const delivery = await exchange(rejected.socketPath, { v: 1, type: "deliver", id: "reject-restart-1", text: "retry after restart" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.retry, true);
    const receipt = await exchange(rejected.socketPath, { v: 1, type: "receipt", id: "reject-restart-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, false);
    assert.equal(receipt.known, true);
  } finally {
    await rejected.stop("SIGKILL");
  }
  await rm(rejected.socketPath, { force: true });
  const retried = spawnHarness(directory, "reject-restart.sock", "message-start");
  try {
    await waitForSocket(retried.socketPath);
    const delivery = await exchange(retried.socketPath, { v: 1, type: "deliver", id: "reject-restart-1", text: "retry after restart" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    await retried.command({ type: "event", name: "message_start", text: "retry after restart" });
    const receipt = await exchange(retried.socketPath, { v: 1, type: "receipt", id: "reject-restart-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, true);
    assert.equal(receipt.evidence, "message_start");
    const state = await retried.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
  } finally {
    await retried.stop();
  }
};

const main = async () => {
  const directory = await mkdtemp(join(tmpdir(), "forgectl-inbox-test-"));
  try {
    await testCoordinatorIdentityActivates(directory);
    await testBoundIdentityRefusals(directory);
    await testNativeInspectionReflectsCurrentMetadata(directory);
    await testNativeInspectionUnknownMetadata(directory);
    await testSettledFailureIsNeverReportedCompleted(directory);
    await testNativeInspectionRequiresSettledExactEntries(directory);
    await testSessionSwitchDuringNativeObservationStaysUnconfirmed(directory);
    await testRejectedReceiptIdentityRebinding(directory);
    await testWorkerDrainIdentityActivates(directory);
    await testInvalidCoordinatorIdentitiesStayInert(directory);
    await testVoidSendStaysPending(directory);
    await testAsyncRuntimeFailureStaysPending(directory);
    await testNativeMessageStartAcceptance(directory);
    await testMissingSessionIdentityStaysRetryable(directory);
    await testPendingRestart(directory);
    await testPendingHistoryReconcile(directory);
    await testHistoryFromWrongSessionStaysUnconfirmed(directory);
    await testAcceptedReceiptFromWrongSessionStaysUnconfirmed(directory);
    await testSocketOwnershipAndMalformedRequests(directory);
    await testDefiniteRejection(directory);
    await testRejectedReceiptRestartsAndRetries(directory);
    process.stdout.write("forgectl-inbox behavioral tests passed\n");
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
};

if (process.argv[2] === "--child") await childMain(process.argv[3]);
else await main();
