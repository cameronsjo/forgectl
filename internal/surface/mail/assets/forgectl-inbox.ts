/**
 * forgectl-inbox: lets `forgectl surface send` reach a pi worker.
 *
 * forgectl ships this file inside its binary (mail.PiExtension) and loads it
 * only for pi workers it launches, passing
 *   FORGECTL_INBOX   absolute path of the unix socket to bind (in the 0700 ledger dir)
 *   FORGECTL_WORKER  this worker's roster name
 * Without both it does nothing, so it is inert in a normal pi session.
 *
 * Protocol: one request per connection, a JSON line in and a JSON line back.
 *   {"v":1,"type":"deliver","id":"...","text":"...","priority":"now|next|later"}
 *     -> {"ok":true,"accepted":true,"evidence":"message_start|history","as":"prompt|steer|followUp"}
 *        | {"ok":false,"error":"...","retry"?:true,"unconfirmed"?:true}
 *   {"v":1,"type":"receipt","id":"..."}
 *     -> {"ok":true,"accepted":true,"evidence":"message_start|history"} | {"ok":false,"unconfirmed":true}
 *   {"v":1,"type":"state"} -> {"ok":true,"idle":true|false}
 *
 * Turn boundaries go back to forgectl as `forgectl surface event --harness pi
 * --state busy|idle`, which records the state, sends --watch notices and flushes.
 *
 * Spike S3 (pi 1.0.4) pinned the event names (agent_start, agent_settled)
 * and the sendUserMessage options this file uses.
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, closeSync, fsyncSync, lstatSync, openSync, readFileSync, statSync, writeSync } from "node:fs";
import { createServer, type Socket } from "node:net";
import { isAbsolute } from "node:path";

const MAX_REQUEST = 256 * 1024;
const REQUEST_TIMEOUT_MS = 5_000;
const OBSERVATION_TIMEOUT_MS = 300;

type Request = {
  v?: number;
  type?: string;
  id?: string;
  text?: string;
  priority?: string;
};

type ReceiptState = "pending" | "accepted" | "rejected";

type ReceiptRecord = {
  id: string;
  digest: string;
  state: ReceiptState;
  sessionId?: string;
  as?: string;
  evidence?: "message_start" | "history";
  error?: string;
};

type DeliveryOutcome = {
  ok: boolean;
  session_id?: string;
  accepted?: boolean;
  as?: string;
  evidence?: "message_start" | "history";
  error?: string;
  retry?: boolean;
  unconfirmed?: boolean;
};

type SocketIdentity = {
  dev: number;
  ino: number;
};

const ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/;

const errorCode = (err: unknown): string => {
  if (typeof err !== "object" || err === null || !("code" in err)) return "";
  const code = (err as { code?: unknown }).code;
  return typeof code === "string" ? code : "";
};

const diagnostic = (message: string): void => {
  try {
    process.stderr.write(`forgectl-inbox: ${message.slice(0, 200)}\n`);
  } catch {
    return;
  }
};

const isReceiptRecord = (value: unknown): value is ReceiptRecord => {
  if (typeof value !== "object" || value === null) return false;
  const record = value as Partial<ReceiptRecord>;
  if (typeof record.id !== "string" || !ID_PATTERN.test(record.id)) return false;
  if (typeof record.digest !== "string" || !/^[0-9a-f]{64}$/.test(record.digest)) return false;
  if (record.state !== "pending" && record.state !== "accepted" && record.state !== "rejected") return false;
  if (record.sessionId !== undefined && typeof record.sessionId !== "string") return false;
  if (record.as !== undefined && typeof record.as !== "string") return false;
  if (record.evidence !== undefined && record.evidence !== "message_start" && record.evidence !== "history") return false;
  return record.error === undefined || typeof record.error === "string";
};

const messageDigest = (text: string): string => createHash("sha256").update(text).digest("hex");

export default function forgectlInbox(pi: ExtensionAPI) {
  const socketPath = process.env.FORGECTL_INBOX;
  const worker = process.env.FORGECTL_WORKER;
  if (!socketPath || !worker || !isAbsolute(socketPath)) return;
  const receiptPath = process.env.FORGECTL_INBOX_RECEIPTS || `${socketPath}.receipts`;

  const receipts = new Map<string, ReceiptRecord>();
  try {
    const contents = readFileSync(receiptPath, "utf8");
    for (const line of contents.split("\n")) {
      if (!line.trim()) continue;
      let parsed: unknown;
      try {
        parsed = JSON.parse(line);
      } catch {
        diagnostic("receipt store contains invalid JSON");
        return;
      }
      if (!isReceiptRecord(parsed)) {
        diagnostic("receipt store contains an invalid record");
        return;
      }
      const previous = receipts.get(parsed.id);
      if (previous && (previous.digest !== parsed.digest || previous.sessionId !== parsed.sessionId || (previous.state === "accepted" && parsed.state !== "accepted"))) {
        diagnostic("receipt store contains conflicting records");
        return;
      }
      receipts.set(parsed.id, parsed);
    }
  } catch (err) {
    if (errorCode(err) !== "ENOENT") {
      diagnostic(`receipt store cannot be read (${errorCode(err) || "read error"})`);
      return;
    }
  }

  let idle = true;
  let sessionId: string | undefined;
  const inFlight = new Map<string, Promise<DeliveryOutcome>>();
  const observationWaiters = new Map<string, Set<(accepted: boolean) => void>>();

  const validID = (id: unknown): id is string => typeof id === "string" && ID_PATTERN.test(id);

  const appendReceipt = (record: ReceiptRecord): void => {
    const fd = openSync(receiptPath, "a", 0o600);
    try {
      writeSync(fd, `${JSON.stringify(record)}\n`);
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
    chmodSync(receiptPath, 0o600);
    receipts.set(record.id, record);
  };

  const contentText = (content: unknown): string | undefined => {
    if (typeof content === "string") return content;
    if (!Array.isArray(content)) return undefined;
    const parts = content
      .filter((part): part is { type: "text"; text: string } => typeof part === "object" && part !== null && (part as { type?: unknown }).type === "text" && typeof (part as { text?: unknown }).text === "string")
      .map((part) => part.text);
    return parts.join("");
  };

  const sessionMatches = (record: ReceiptRecord): boolean => Boolean(sessionId && record.sessionId && record.sessionId === sessionId);

  const pendingForDigest = (digest: string): ReceiptRecord | undefined => {
    for (const record of receipts.values()) {
      if (record.state === "pending" && record.digest === digest && sessionMatches(record)) return record;
    }
    return undefined;
  };

  const notifyObservation = (id: string, accepted: boolean): void => {
    const waiters = observationWaiters.get(id);
    if (!waiters) return;
    observationWaiters.delete(id);
    for (const resolve of waiters) resolve(accepted);
  };

  const markAccepted = (record: ReceiptRecord, evidence: "message_start" | "history"): boolean => {
    if (record.state === "accepted") return true;
    if (record.state !== "pending" || !sessionMatches(record)) return false;
    try {
      appendReceipt({
        ...record,
        state: "accepted",
        as: record.as || "prompt",
        evidence,
      });
    } catch {
      diagnostic(`could not persist native acceptance for ${record.id}`);
      return false;
    }
    notifyObservation(record.id, true);
    return true;
  };

  const reconcileHistory = (ctx: { sessionManager?: { getEntries?: () => unknown[]; getSessionId?: () => string } }): void => {
    try {
      sessionId = ctx.sessionManager?.getSessionId?.();
      const entries = ctx.sessionManager?.getEntries?.() || [];
      for (const entry of entries) {
        if (typeof entry !== "object" || entry === null) continue;
        const candidate = entry as { type?: unknown; message?: { role?: unknown; content?: unknown } };
        if (candidate.type !== "message" || candidate.message?.role !== "user") continue;
        const text = contentText(candidate.message.content);
        if (text === undefined) continue;
        const record = pendingForDigest(messageDigest(text));
        if (record) markAccepted(record, "history");
      }
    } catch (err) {
      diagnostic(`session history reconciliation failed (${String(err).slice(0, 160)})`);
    }
  };

  const report = (state: "idle" | "busy") => {
    execFile(
      "forgectl",
      ["surface", "event", "--harness", "pi", `--state=${state}`],
      { env: process.env, timeout: 10_000 },
      () => {
        // Best effort: a failed report must never disturb the session.
      },
    );
  };

  pi.on("agent_start", () => {
    idle = false;
    report("busy");
  });
  // pi stays in its run after agent_end (retries and auto-compaction live in
  // that gap) until agent_settled, its "will not continue" signal. Calling
  // the worker idle at agent_end lost a message delivered in that gap on pi
  // 1.0.4 (spike S3), so idle starts at agent_settled.
  pi.on("agent_settled", () => {
    idle = true;
    report("idle");
  });

  pi.on("message_start", (event: { message?: { role?: unknown; content?: unknown } }) => {
    if (event.message?.role !== "user") return;
    const text = contentText(event.message.content);
    if (text === undefined) return;
    const record = pendingForDigest(messageDigest(text));
    if (record) markAccepted(record, "message_start");
  });

  pi.on("session_start", (_event: unknown, ctx: { sessionManager?: { getEntries?: () => unknown[]; getSessionId?: () => string } }) => {
    reconcileHistory(ctx);
  });

  const waitForNativeObservation = (id: string): Promise<boolean> => new Promise((resolve) => {
    const current = receipts.get(id);
    if (current?.state === "accepted") {
      resolve(true);
      return;
    }
    const waiters = observationWaiters.get(id) || new Set<(accepted: boolean) => void>();
    const finish = (accepted: boolean): void => {
      clearTimeout(timer);
      waiters.delete(finish);
      if (waiters.size === 0) observationWaiters.delete(id);
      resolve(accepted);
    };
    const timer = setTimeout(() => finish(receipts.get(id)?.state === "accepted"), OBSERVATION_TIMEOUT_MS);
    waiters.add(finish);
    observationWaiters.set(id, waiters);
  });

  const deliverFresh = async (req: Request, text: string, digest: string): Promise<DeliveryOutcome> => {
    if (!sessionId) return { ok: false, retry: true, error: "current pi session identity is unavailable" };
    const wasIdle = idle;
    const deliverAs = !wasIdle && req.priority === "later" ? "followUp" : "steer";
    const pendingRecord: ReceiptRecord = { id: req.id as string, digest, state: "pending", sessionId, as: wasIdle ? "prompt" : deliverAs };
    try {
      appendReceipt(pendingRecord);
    } catch {
      diagnostic("receipt store write failed before pi delivery");
      return { ok: false, retry: true, error: "could not persist the pi delivery receipt" };
    }
    try {
      const result = pi.sendUserMessage(text, { deliverAs }) as unknown;
      if (result && typeof (result as PromiseLike<unknown>).then === "function") {
        void Promise.resolve(result).catch((err) => diagnostic(`pi sendUserMessage failed after invocation (${String(err).slice(0, 160)})`));
      }
    } catch (err) {
      try {
        appendReceipt({ ...pendingRecord, state: "rejected", error: String(err).slice(0, 200) });
      } catch {
        diagnostic("receipt store write failed after pi rejection");
        return { ok: false, retry: true, error: "pi rejected the message but the receipt could not be persisted" };
      }
      return { ok: false, retry: true, error: String(err).slice(0, 200) };
    }
    if (!await waitForNativeObservation(req.id as string)) {
      return { ok: false, unconfirmed: true, error: "pi has not observed native consumption of the message" };
    }
    const accepted = receipts.get(req.id as string);
    return {
      ok: true,
      session_id: sessionId,
      accepted: true,
      as: accepted?.as || (wasIdle ? "prompt" : deliverAs),
      evidence: accepted?.evidence || "message_start",
    };
  };

  const deliver = async (req: Request): Promise<DeliveryOutcome> => {
    if (!validID(req.id)) return { ok: false, error: "message id is required" };
    const text = typeof req.text === "string" ? req.text : "";
    if (text.trim() === "") return { ok: false, error: "empty message" };
    const digest = messageDigest(text);
    const previous = receipts.get(req.id);
    if (previous && previous.digest !== digest) return { ok: false, error: "message id was reused with different text" };
    if (previous?.state === "accepted") {
      if (!sessionMatches(previous)) return { ok: false, unconfirmed: true, error: "receipt belongs to a different pi session" };
      return { ok: true, session_id: sessionId, accepted: true, as: previous.as, evidence: previous.evidence };
    }
    if (previous?.state === "pending") return { ok: false, unconfirmed: true, error: "delivery is unconfirmed" };
    const running = inFlight.get(req.id);
    if (running) return running;
    const operation = deliverFresh(req, text, digest);
    inFlight.set(req.id, operation);
    try {
      return await operation;
    } finally {
      inFlight.delete(req.id);
    }
  };

  const receipt = (req: Request): DeliveryOutcome & { accepted?: boolean; known?: boolean } => {
    if (!validID(req.id)) return { ok: false, error: "message id is required" };
    const record = receipts.get(req.id);
    if (!record || record.state === "pending") return { ok: false, unconfirmed: true, error: "receipt is unknown" };
    if (!sessionMatches(record)) return { ok: false, unconfirmed: true, error: "receipt belongs to a different pi session" };
    if (record.state === "accepted") return { ok: true, session_id: sessionId, accepted: true, known: true, as: record.as, evidence: record.evidence };
    return { ok: true, session_id: sessionId, accepted: false, known: true, error: record.error || "pi rejected the message" };
  };

  const server = createServer((sock: Socket) => {
    let buf = "";
    let answered = false;
    const answer = (id: string | undefined, body: object) => {
      if (answered) return;
      answered = true;
      sock.end(JSON.stringify(id ? { id, ...body } : body) + "\n");
    };
    sock.setEncoding("utf8");
    sock.setTimeout(REQUEST_TIMEOUT_MS, () => sock.destroy());
    sock.on("error", () => sock.destroy());
    sock.on("data", (chunk: string) => {
      if (answered) return;
      buf += chunk;
      if (buf.length > MAX_REQUEST) {
        answer(undefined, { ok: false, error: "request too large" });
        return;
      }
      const nl = buf.indexOf("\n");
      if (nl < 0) return;
      const line = buf.slice(0, nl);
      buf = "";
      let parsed: unknown;
      try {
        parsed = JSON.parse(line);
      } catch {
        answer(undefined, { ok: false, error: "not JSON" });
        return;
      }
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
        answer(undefined, { ok: false, error: "request must be an object" });
        return;
      }
      const req = parsed as Request;
      if (req.v !== 1) {
        answer(req.id, { ok: false, error: "unsupported protocol version" });
        return;
      }
      try {
        if (req.type === "deliver") {
          deliver(req).then((body) => answer(req.id, body), (err) => answer(req.id, { ok: false, unconfirmed: true, error: String(err).slice(0, 200) }));
        } else if (req.type === "receipt") answer(req.id, receipt(req));
        else if (req.type === "state") answer(req.id, { ok: true, idle });
        else answer(req.id, { ok: false, error: "unknown request type" });
      } catch (err) {
        answer(req.id, { ok: false, unconfirmed: req.type === "deliver", error: String(err).slice(0, 200) });
      }
    });
  });

  let ownedSocket: SocketIdentity | undefined;
  const start = async () => {
    let existing;
    try {
      existing = lstatSync(socketPath);
    } catch (err) {
      if (errorCode(err) !== "ENOENT") return;
    }
    if (existing) {
      diagnostic("inbox path already exists; register a fresh socket path for this process");
      return;
    }
    server.listen(socketPath, () => {
      try {
        chmodSync(socketPath, 0o600);
        const current = statSync(socketPath);
        ownedSocket = { dev: current.dev, ino: current.ino };
      } catch {
        ownedSocket = undefined;
      }
    });
  };

  server.on("error", (err) => {
    ownedSocket = undefined;
    diagnostic(`inbox socket could not bind (${errorCode(err) || "listen error"})`);
  });

  const cleanup = () => {
    if (!ownedSocket) return;
    let current;
    try {
      current = statSync(socketPath);
    } catch {
      return;
    }
    if (current.dev !== ownedSocket.dev || current.ino !== ownedSocket.ino) return;
    server.close();
    try {
      unlinkSync(socketPath);
    } catch {
      // already gone
    }
  };
  process.once("exit", cleanup);
  void start();
}
