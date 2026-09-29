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
 *     -> {"ok":true,"as":"prompt|steer|followUp"} | {"ok":false,"error":"..."}
 *   {"v":1,"type":"state"} -> {"ok":true,"idle":true|false}
 *
 * Turn boundaries go back to forgectl as `forgectl surface event --harness pi
 * --state busy|idle`, which records the state, sends --watch notices and flushes.
 *
 * Spike S3 pins the event names (agent_start / agent_end) and sendUserMessage
 * options against the installed pi before this ships.
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { execFile } from "node:child_process";
import { chmodSync, existsSync, unlinkSync } from "node:fs";
import { createServer, type Socket } from "node:net";
import { isAbsolute } from "node:path";

const MAX_REQUEST = 256 * 1024;
const REQUEST_TIMEOUT_MS = 5_000;

type Request = {
  v?: number;
  type?: string;
  id?: string;
  text?: string;
  priority?: string;
};

export default function forgectlInbox(pi: ExtensionAPI) {
  const socketPath = process.env.FORGECTL_INBOX;
  const worker = process.env.FORGECTL_WORKER;
  if (!socketPath || !worker || !isAbsolute(socketPath)) return;

  let idle = true;

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
  pi.on("agent_end", () => {
    idle = true;
    report("idle");
  });

  const deliver = (req: Request): { ok: boolean; as?: string; error?: string } => {
    const text = typeof req.text === "string" ? req.text : "";
    if (text.trim() === "") return { ok: false, error: "empty message" };
    const later = req.priority === "later";
    if (idle) {
      try {
        pi.sendUserMessage(text);
        return { ok: true, as: "prompt" };
      } catch {
        // pi started a run since the last agent_start we saw; queue instead.
        idle = false;
      }
    }
    const deliverAs = later ? "followUp" : "steer";
    pi.sendUserMessage(text, { deliverAs });
    return { ok: true, as: deliverAs };
  };

  const server = createServer((sock: Socket) => {
    let buf = "";
    let answered = false;
    const answer = (body: object) => {
      if (answered) return;
      answered = true;
      sock.end(JSON.stringify(body) + "\n");
    };
    sock.setEncoding("utf8");
    sock.setTimeout(REQUEST_TIMEOUT_MS, () => sock.destroy());
    sock.on("error", () => sock.destroy());
    sock.on("data", (chunk: string) => {
      if (answered) return;
      buf += chunk;
      if (buf.length > MAX_REQUEST) {
        answer({ ok: false, error: "request too large" });
        return;
      }
      const nl = buf.indexOf("\n");
      if (nl < 0) return;
      const line = buf.slice(0, nl);
      buf = "";
      let req: Request;
      try {
        req = JSON.parse(line) as Request;
      } catch {
        answer({ ok: false, error: "not JSON" });
        return;
      }
      if (req.v !== 1) {
        answer({ ok: false, error: "unsupported protocol version" });
        return;
      }
      try {
        if (req.type === "deliver") answer(deliver(req));
        else if (req.type === "state") answer({ ok: true, idle });
        else answer({ ok: false, error: "unknown request type" });
      } catch (err) {
        answer({ ok: false, error: String(err).slice(0, 200) });
      }
    });
  });

  // A socket file left by a crashed run would make listen fail with EADDRINUSE.
  if (existsSync(socketPath)) {
    try {
      unlinkSync(socketPath);
    } catch {
      return;
    }
  }
  server.on("error", () => {
    // Leave pi running; forgectl sees the socket refuse and keeps the message queued.
  });
  server.listen(socketPath, () => {
    try {
      chmodSync(socketPath, 0o600);
    } catch {
      // The ledger dir is 0700 already.
    }
  });

  const cleanup = () => {
    server.close();
    try {
      unlinkSync(socketPath);
    } catch {
      // already gone
    }
  };
  process.once("exit", cleanup);
}
