// Tests for the adapter's trust boundary: filesystem writes, the callback
// listener, and the worker-side Stop hook.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { awaitStop, ensureListener } from "./callback.js";
import { readAppended, removeTurnFiles, writeBrief, writeTurnFile } from "./turnstate.js";

const tmp = (): string => mkdtempSync(path.join(os.tmpdir(), "pcb-"));
const RUN = "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b";

test("writeBrief refuses a .paperclip that is a symlink out of the worktree", async () => {
  const wt = tmp();
  const outside = tmp();
  symlinkSync(outside, path.join(wt, ".paperclip"));
  await assert.rejects(writeBrief(wt, RUN, "brief"), /not a plain directory/);
  assert.equal(existsSync(path.join(outside, `brief-${RUN}.md`)), false, "nothing was written through the link");
});

test("writeBrief will not write through a planted brief symlink", async () => {
  const wt = tmp();
  const victim = path.join(tmp(), "victim.txt");
  writeFileSync(victim, "keep");
  mkdirSync(path.join(wt, ".paperclip"));
  symlinkSync(victim, path.join(wt, ".paperclip", `brief-${RUN}.md`));
  await assert.rejects(writeBrief(wt, RUN, "brief"));
  assert.equal(readFileSync(victim, "utf8"), "keep");
});

test("writeBrief writes 0600 and removeTurnFiles cleans up the empty directory", async () => {
  const wt = tmp();
  const b = await writeBrief(wt, RUN, "brief");
  assert.equal(statSync(b.file).mode & 0o777, 0o600);
  await removeTurnFiles([b.file], b.dir);
  assert.equal(existsSync(b.dir), false);
});

test("writeTurnFile keeps the token outside the worktree, mode 0600, keyed by session", async () => {
  const state = tmp();
  process.env["XDG_STATE_HOME"] = state;
  const f = await writeTurnFile("4de34e23-c46c-4ab2-86bb-b4837be5ea89", { runId: RUN, token: "a".repeat(64), callbackUrl: "http://127.0.0.1:3199/stop" });
  assert.ok(f.startsWith(path.join(state, "forgectl-paperclip", "turns")));
  assert.equal(statSync(f).mode & 0o777, 0o600);
  await assert.rejects(writeTurnFile("../escape", { runId: RUN, token: "a", callbackUrl: "x" }), /refusing turn file/);
});

test("readAppended caps its read and refuses a symlink", async () => {
  const d = tmp();
  const f = path.join(d, "t.jsonl");
  writeFileSync(f, "x".repeat(1000));
  const lines = await readAppended(f, 100, 50);
  assert.equal(lines.join("\n").length, 50);
  symlinkSync(f, path.join(d, "link.jsonl"));
  await assert.rejects(readAppended(path.join(d, "link.jsonl"), 0));
});

test("callback listener resolves only on the right run id and token", async () => {
  const port = 41000 + Math.floor(Math.random() * 2000);
  await ensureListener(port);
  const token = "b".repeat(64);
  const wait = awaitStop(RUN, token, 5000);
  const post = (body: unknown): Promise<number> =>
    fetch(`http://127.0.0.1:${port}/stop`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) }).then((r) => r.status);
  assert.equal(await post({ runId: RUN, token: "c".repeat(64), sessionId: "s" }), 404);
  assert.equal(await post({ runId: RUN, token, sessionId: "s" }), 204);
  const out = await wait.result;
  assert.equal(out.kind, "stop");
  assert.equal(out.kind === "stop" && out.cb.sessionId, "s");
});

test("awaitStop reports cancel and timeout as their own outcomes", async () => {
  const a = awaitStop("run-cancel", "t", 5000);
  a.cancel();
  assert.deepEqual(await a.result, { kind: "cancelled" });
  const ac = new AbortController();
  const b = awaitStop("run-abort", "t", 5000, ac.signal);
  ac.abort();
  assert.deepEqual(await b.result, { kind: "cancelled" });
  assert.deepEqual(await awaitStop("run-timeout", "t", 10).result, { kind: "timeout" });
});

test("the Stop hook never calls curl for a tampered callback URL", () => {
  const hook = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "hooks", "paperclip-stop.sh");
  const state = tmp();
  const bin = tmp();
  const log = path.join(bin, "curl.log");
  writeFileSync(path.join(bin, "curl"), `#!/usr/bin/env bash\nprintf '%s\\n' "$*" >> ${JSON.stringify(log)}\n`);
  chmodSync(path.join(bin, "curl"), 0o755);
  const session = "4de34e23-c46c-4ab2-86bb-b4837be5ea89";
  const turns = path.join(state, "forgectl-paperclip", "turns");
  mkdirSync(turns, { recursive: true });
  const run = (url: string): void => {
    writeFileSync(path.join(turns, `${session}.json`), JSON.stringify({ runId: RUN, token: "d".repeat(64), callbackUrl: url }));
    execFileSync("bash", [hook], { input: JSON.stringify({ session_id: session }), env: { PATH: `${bin}:/usr/bin:/bin:/opt/homebrew/bin`, HOME: state, XDG_STATE_HOME: state } });
  };
  run("http://127.0.0.1:1@attacker.example/stop");
  assert.equal(existsSync(log), false, "no request for a userinfo URL");
  run("http://127.0.0.1:3199/stop");
  assert.match(readFileSync(log, "utf8"), /http:\/\/127\.0\.0\.1:3199\/stop/);
});
