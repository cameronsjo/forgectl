import assert from "node:assert/strict";
import { once } from "node:events";
import { lstat, mkdtemp, rm } from "node:fs/promises";
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
  return { child, command, errors: () => errors, receiptPath, socketPath, stop };
};

const testCoordinatorIdentityActivates = async (directory) => {
  const harness = spawnHarness(directory, "coordinator.sock", "message-start", { coordinator: "pi-coordinator", ledger: directory, runId: "run-1" });
  try {
    await waitForSocket(harness.socketPath);
    const delivery = await exchange(harness.socketPath, { v: 1, type: "deliver", id: "coordinator-1", text: "native" });
    assert.equal(delivery.ok, false);
    assert.equal(delivery.unconfirmed, true);
    await harness.command({ type: "event", name: "message_start", text: "native" });
    const receipt = await exchange(harness.socketPath, { v: 1, type: "receipt", id: "coordinator-1" });
    assert.equal(receipt.ok, true);
    assert.equal(receipt.accepted, true);
    const state = await harness.command({ type: "calls" });
    assert.equal(state.calls.length, 1);
  } finally {
    await harness.stop();
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
  const currentSession = mode === "missing-session" ? undefined : mode.startsWith("wrong-") ? "session-2" : "session-1";
  const sessionManager = {
    getSessionId: () => currentSession,
    getEntries: () => (mode === "history" || mode === "wrong-history") ? [{ type: "message", message: { role: "user", content: "may have arrived" } }] : [],
  };
  const eventContext = { sessionManager };
  const pi = {
    on(event, handler) {
      handlers.set(event, handler);
    },
    sendUserMessage(text, options) {
      calls.push({ text, options });
      if (mode === "pending") return new Promise(() => undefined);
      if (mode === "reject") throw new Error("definite pi rejection");
      if (mode === "runtime-failure") return Promise.reject(new Error("runtime send failure"));
      return undefined;
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
      else if (request.type === "event") {
        const handler = handlers.get(request.name);
        const event = request.name === "message_start" ? { type: request.name, message: { role: "user", content: request.text } } : { type: request.name };
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
