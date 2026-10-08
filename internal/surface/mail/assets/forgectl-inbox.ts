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
 *     -> {"ok":true,"as":"prompt|steer|followUp"} | {"ok":false,"error":"...","retry"?:true}
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
  // pi stays in its run after agent_end (retries and auto-compaction live in
  // that gap) until agent_settled, its "will not continue" signal. Calling
  // the worker idle at agent_end lost a message delivered in that gap on pi
  // 1.0.4 (spike S3), so idle starts at agent_settled.
  pi.on("agent_settled", () => {
    idle = true;
    report("idle");
  });

  // Every send names a mode. A bare sendUserMessage during a run does not
  // throw to the extension on pi 1.0.4: pi rejects it later, as an extension
  // error, after forgectl has recorded the message sent. A steer sent while
  // idle starts a normal turn (spike S3), so idle and busy take the same call.
  const deliver = async (req: Request): Promise<{ ok: boolean; as?: string; error?: string; retry?: boolean }> => {
    const text = typeof req.text === "string" ? req.text : "";
    if (text.trim() === "") return { ok: false, error: "empty message" };
    const wasIdle = idle;
    const deliverAs = !wasIdle && req.priority === "later" ? "followUp" : "steer";
    try {
      await pi.sendUserMessage(text, { deliverAs });
    } catch (err) {
      // pi could not take it now; forgectl keeps the message queued and
      // retries it.
      return { ok: false, retry: true, error: String(err).slice(0, 200) };
    }
    return { ok: true, as: wasIdle ? "prompt" : deliverAs };
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
        if (req.type === "deliver") {
          deliver(req).then(answer, (err) => answer({ ok: false, retry: true, error: String(err).slice(0, 200) }));
        } else if (req.type === "state") answer({ ok: true, idle });
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
