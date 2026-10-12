// Loopback listener for the worker's Claude Code Stop hook. One listener per
// server process; each turn registers a run id and a one-time token.
import { createServer, type Server } from "node:http";
import { timingSafeEqual } from "node:crypto";
import { parseStopCallback, type StopCallback } from "./core.js";

interface Pending {
  token: string;
  resolve: (cb: StopCallback) => void;
}

const pending = new Map<string, Pending>();
let server: Server | null = null;
let boundPort: number | null = null;

const MAX_BODY = 16 * 1024;

function tokensMatch(a: string, b: string): boolean {
  const x = Buffer.from(a);
  const y = Buffer.from(b);
  return x.length === y.length && timingSafeEqual(x, y);
}

export async function ensureListener(port: number): Promise<void> {
  if (server && boundPort === port) return;
  if (server) throw new Error(`callback listener already bound to ${boundPort}; adapters on one server must share callbackPort, got ${port}`);
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
  await new Promise<void>((resolve, reject) => {
    s.once("error", reject);
    s.listen(port, "127.0.0.1", () => resolve());
  });
  server = s;
  boundPort = port;
}

/** Resolves with the hook's payload, or null when the turn times out or is cancelled. */
export function awaitStop(runId: string, token: string, timeoutMs: number, signal?: AbortSignal): Promise<StopCallback | null> {
  return new Promise((resolve) => {
    const done = (v: StopCallback | null): void => {
      clearTimeout(timer);
      pending.delete(runId);
      resolve(v);
    };
    const timer = setTimeout(() => done(null), timeoutMs);
    signal?.addEventListener("abort", () => done(null), { once: true });
    pending.set(runId, { token, resolve: (cb) => done(cb) });
  });
}
