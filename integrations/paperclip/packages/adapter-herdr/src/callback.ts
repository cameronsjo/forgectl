// Loopback listener for the worker's Claude Code Stop hook. One listener per
// server process; each turn registers a run id and a one-time token.
import { createServer, type Server } from "node:http";
import { timingSafeEqual } from "node:crypto";
import { parseStopCallback, type StopCallback } from "./core.js";

interface Pending {
  token: string;
  resolve: (cb: StopCallback) => void;
}

// Paperclip's adapter reload imports this module again, which would start a
// second listener on the same port. The listener and its pending turns live on
// globalThis so every copy of the module shares them.
interface ListenerState {
  pending: Map<string, Pending>;
  server: Server | null;
  boundPort: number | null;
}
const STATE_KEY = Symbol.for("forgectl.paperclip.adapter-herdr.listener");
const holder = globalThis as unknown as Record<symbol, ListenerState | undefined>;
const state: ListenerState = (holder[STATE_KEY] ??= { pending: new Map(), server: null, boundPort: null });
const pending = state.pending;

const MAX_BODY = 16 * 1024;

function tokensMatch(a: string, b: string): boolean {
  const x = Buffer.from(a);
  const y = Buffer.from(b);
  return x.length === y.length && timingSafeEqual(x, y);
}

export async function ensureListener(port: number): Promise<void> {
  if (state.server && state.boundPort === port) return;
  if (state.server) throw new Error(`callback listener already bound to ${state.boundPort}; adapters on one server must share callbackPort, got ${port}`);
  const s = createServer((req, res) => {
    if (req.method !== "POST" || req.url !== "/stop") {
      res.writeHead(404).end();
      return;
    }
    let size = 0;
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => {
      size += c.length;
      if (size > MAX_BODY) req.destroy();
      else chunks.push(c);
    });
    req.on("end", () => {
      let body: unknown = null;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
      } catch {
        res.writeHead(400).end();
        return;
      }
      const cb = parseStopCallback(body);
      const p = cb ? pending.get(cb.runId) : undefined;
      if (!cb || !p || !tokensMatch(p.token, cb.token)) {
        res.writeHead(404).end();
        return;
      }
      pending.delete(cb.runId);
      p.resolve(cb);
      res.writeHead(204).end();
    });
  });
  s.requestTimeout = 10_000;
  // The host process (Paperclip) keeps running on its own; the listener should not hold it open.
  s.unref();
  await new Promise<void>((resolve, reject) => {
    s.once("error", reject);
    s.listen(port, "127.0.0.1", () => resolve());
  });
  state.server = s;
  state.boundPort = port;
}

export type StopOutcome =
  | { kind: "stop"; cb: StopCallback }
  | { kind: "timeout" }
  | { kind: "cancelled" };

export interface StopWait {
  result: Promise<StopOutcome>;
  /** Resolves the wait as cancelled and frees its timer and pending entry; safe to call twice. */
  cancel: () => void;
}

/** Waits for the worker's Stop hook, a timeout, or the run's abort signal, whichever comes first. */
export function awaitStop(runId: string, token: string, timeoutMs: number, signal?: AbortSignal): StopWait {
  let settle: (v: StopOutcome) => void = () => undefined;
  const result = new Promise<StopOutcome>((resolve) => {
    let finished = false;
    const timer = setTimeout(() => done({ kind: "timeout" }), timeoutMs);
    const onAbort = (): void => done({ kind: "cancelled" });
    function done(v: StopOutcome): void {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      if (pending.get(runId)?.token === token) pending.delete(runId);
      resolve(v);
    }
    settle = done;
    if (signal?.aborted) return done({ kind: "cancelled" });
    signal?.addEventListener("abort", onAbort, { once: true });
    pending.set(runId, { token, resolve: (cb) => done({ kind: "stop", cb }) });
  });
  return { result, cancel: () => settle({ kind: "cancelled" }) };
}
