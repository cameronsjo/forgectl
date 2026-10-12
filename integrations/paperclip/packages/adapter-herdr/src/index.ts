// Paperclip server adapter: each execute() is one turn in a long-lived herdr
// pane that forgectl surface launched, briefed, and reads. Paperclip's
// heartbeat decides when a turn runs; forgectl decides how the pane is
// started, briefed and read.
import { randomBytes } from "node:crypto";
import { mkdir, open, realpath, rm, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import type {
  AdapterEnvironmentTestContext,
  AdapterEnvironmentTestResult,
  AdapterExecutionContext,
  AdapterExecutionResult,
  ServerAdapterModule,
} from "@paperclipai/adapter-utils";
import { awaitStop, ensureListener } from "./callback.js";
import {
  ADAPTER_TYPE,
  composeBrief,
  decodeSession,
  extractIssue,
  parseConfig,
  planTurn,
  sessionFromWorker,
  sumTranscriptUsage,
  workerNameFor,
  type AdapterConfig,
  type ListedWorker,
  type WorkerSession,
} from "./core.js";
import { brief, launchWorker, listWorkers, readReport, runForgectl, waitReady, waitSettled, watchForBlocking } from "./forgectl.js";

function failure(message: string, code: string, session: WorkerSession | null): AdapterExecutionResult {
  return {
    exitCode: 1,
    signal: null,
    timedOut: false,
    errorMessage: message,
    errorCode: code,
    sessionParams: session ? { ...session } : null,
    sessionDisplayId: session?.workerName ?? null,
  };
}

/** forgectl puts worker worktrees under <repo>/.claude/worktrees/; refuse to write anywhere else. */
async function checkedWorktree(cfg: AdapterConfig, w: ListedWorker): Promise<string> {
  if (!w.worktree) throw new Error(`worker ${w.name} has no worktree in forgectl surface list --json`);
  const root = path.join(await realpath(cfg.repo), ".claude", "worktrees") + path.sep;
  const wt = await realpath(w.worktree);
  if (!wt.startsWith(root)) throw new Error(`worker ${w.name} worktree ${wt} is outside ${root}`);
  return wt;
}

async function fileSize(p: string | null): Promise<number> {
  if (!p) return 0;
  try {
    return (await stat(p)).size;
  } catch {
    return 0;
  }
}

async function readFrom(p: string, offset: number): Promise<string[]> {
  const fh = await open(p, "r");
  try {
    const { size } = await fh.stat();
    const len = Math.max(0, size - offset);
    const buf = Buffer.alloc(len);
    await fh.read(buf, 0, len, offset);
    return buf.toString("utf8").split("\n");
  } finally {
    await fh.close();
  }
}

async function execute(ctx: AdapterExecutionContext): Promise<AdapterExecutionResult> {
  const log = (msg: string): Promise<void> => ctx.onLog("stdout", `[herdr-forgectl] ${msg}\n`);
  let cfg: AdapterConfig;
  try {
    cfg = parseConfig(ctx.config);
  } catch (err) {
    return failure((err as Error).message, "invalid_config", null);
  }
  await ensureListener(cfg.callbackPort);

  const stored = decodeSession(ctx.runtime.sessionParams);
  const fallback = workerNameFor(ctx.agent.id, ctx.runtime.taskKey);
  let workers = await listWorkers(cfg);
  const plan = planTurn(stored, fallback, workers);
  let worker: ListedWorker;

  if (plan.kind === "launch") {
    await log(`Attempting launch of a new pane. Worker: ${plan.workerName}, Session: ${cfg.herdrSession}, Profile: ${cfg.profile}`);
    ctx.onDispatch?.();
    const r = await launchWorker(cfg, plan.workerName);
    if (r.code !== 0) {
      return failure(`forgectl surface launch exited ${r.code}: ${r.stderr.trim().slice(0, 800)}`, "launch_failed", stored);
    }
    workers = await listWorkers(cfg);
    const row = workers.find((w) => w.name === plan.workerName);
    if (!row) return failure(`launched ${plan.workerName} but surface list does not show it`, "launch_unlisted", stored);
    worker = row;
    const ready = await waitReady(cfg, worker.name);
    if (ready.code !== 0) {
      return failure(`worker ${worker.name} never reached its prompt: ${String(ready["reason"] ?? ready["blocking"] ?? "unknown")}`, "not_ready", sessionFromWorker(cfg, worker));
    }
    await log(`Successfully launched pane. Worker: ${worker.name}, Pane: ${worker.pane_id}, Workspace: ${worker.workspace_id}`);
  } else {
    worker = plan.worker;
    ctx.onDispatch?.();
    await log(`${plan.reattached ? "Reattached to" : "Reusing"} live pane. Worker: ${worker.name}, Pane: ${worker.pane_id}`);
    // The Stop hook fires before the harness redraws its prompt, so a brief sent
    // right after the previous turn can fail read-back. Wait for the prompt first.
    const ready = await waitReady(cfg, worker.name);
    if (ready.code !== 0) {
      const blocked = typeof ready["blocking"] === "string" && ready["blocking"] !== "";
      return failure(`worker ${worker.name} is not at its prompt: ${String(ready["reason"] ?? ready["blocking"] ?? "unknown")}`, blocked ? "worker_blocked" : "not_ready", sessionFromWorker(cfg, worker));
    }
  }

  const session = sessionFromWorker(cfg, worker);
  let worktree: string;
  try {
    worktree = await checkedWorktree(cfg, worker);
  } catch (err) {
    return failure((err as Error).message, "worktree_rejected", session);
  }

  const token = randomBytes(32).toString("hex");
  const nonce = randomBytes(12).toString("hex");
  const dir = path.join(worktree, ".paperclip");
  await mkdir(dir, { recursive: true, mode: 0o700 });
  const briefRel = `.paperclip/brief-${ctx.runId}.md`;
  await writeFile(path.join(worktree, briefRel), composeBrief(ctx.runId, ctx.agent.name, extractIssue(ctx.context), nonce), { mode: 0o600 });
  await writeFile(
    path.join(dir, "turn.json"),
    JSON.stringify({ runId: ctx.runId, token, callbackUrl: `http://127.0.0.1:${cfg.callbackPort}/stop` }),
    { mode: 0o600 },
  );

  // The turn files are ignored by git but still make `surface close` keep the
  // worktree, and turn.json holds this turn's callback token; remove both on
  // every exit.
  try {
    return await runTurn(ctx, cfg, worker, session, plan.kind === "brief" && plan.reattached, token, briefRel, log);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

async function runTurn(
  ctx: AdapterExecutionContext,
  cfg: AdapterConfig,
  worker: ListedWorker,
  session: WorkerSession,
  reattached: boolean,
  token: string,
  briefRel: string,
  log: (msg: string) => Promise<void>,
): Promise<AdapterExecutionResult> {
  const offset = await fileSize(worker.transcript);
  const stop = cfg.harness === "claude" ? awaitStop(ctx.runId, token, cfg.turnTimeoutSec * 1000, ctx.signal) : null;
  const sent = await brief(cfg, worker.name, `Read ${briefRel} and do what it asks.`);
  if (sent.code !== 0) {
    return failure(`forgectl surface brief refused: ${String(sent["reason"] ?? sent["outcome"] ?? "unknown")}`, "brief_refused", session);
  }
  await log(`Brief sent. Marker: ${String(sent["marker"] ?? "")}; waiting for the ${stop ? "Stop hook" : "turn to settle"}`);

  let claudeSessionId = session.claudeSessionId;
  let transcript = worker.transcript;
  if (stop) {
    const watchDone = new AbortController();
    const outcome = await Promise.race([
      stop.then((cb) => ({ kind: "stop" as const, cb })),
      watchForBlocking(cfg, worker.name, watchDone.signal).then((blocking) => ({ kind: "blocked" as const, blocking })),
    ]);
    watchDone.abort();
    if (outcome.kind === "blocked") {
      // The pane stays open at its dialog; the next wake reattaches once a person answers it.
      return failure(`worker ${worker.name} is waiting at a ${outcome.blocking ?? "dialog"}; answer it in the pane (herdr --session ${cfg.herdrSession})`, "worker_blocked", session);
    }
    const cb = outcome.cb;
    if (!cb) {
      return { ...failure(`no Stop hook within ${cfg.turnTimeoutSec}s`, "turn_timeout", session), timedOut: true };
    }
    await log(`Stop hook received. Claude session: ${cb.sessionId}`);
    claudeSessionId = cb.sessionId ?? claudeSessionId;
    transcript = cb.transcriptPath ?? transcript;
  } else {
    const settled = await waitSettled(cfg, worker.name, cfg.turnTimeoutSec);
    if (settled.code !== 0) {
      const blocked = typeof settled["blocking"] === "string" && settled["blocking"] !== "";
      return { ...failure(`turn did not settle: ${String(settled["reason"] ?? settled["state"] ?? "unknown")}`, blocked ? "worker_blocked" : "turn_timeout", session), timedOut: !blocked };
    }
  }

  const report = await readReport(cfg, worker.name);
  if (!stop && report["found"] !== true) {
    // A settled codex pane with no REPORT line may never have started the turn; forgectl's wait cannot tell.
    return failure("codex turn settled without its REPORT line", "report_missing", session);
  }
  const usage = cfg.harness === "claude" && transcript ? sumTranscriptUsage(await readFrom(transcript, offset)) : null;

  return {
    exitCode: 0,
    signal: null,
    timedOut: false,
    ...(usage
      ? {
          usage: { inputTokens: usage.inputTokens, outputTokens: usage.outputTokens, cachedInputTokens: usage.cachedInputTokens },
          usageBasis: "per_run" as const,
          model: usage.model,
        }
      : {}),
    provider: cfg.harness === "claude" ? "anthropic" : "openai",
    biller: cfg.harness === "claude" ? "anthropic" : "openai",
    billingType: "subscription_included",
    sessionParams: { ...session, claudeSessionId },
    sessionDisplayId: worker.name,
    summary: typeof report["report"] === "string" ? report["report"] : null,
    resultJson: { worker: worker.name, pane: worker.pane_id, report: report["report"] ?? null, reportFound: report["found"] ?? false, reattached },
  };
}

async function testEnvironment(_ctx: AdapterEnvironmentTestContext): Promise<AdapterEnvironmentTestResult> {
  const cfg = parseConfig({ repo: "." });
  const r = await runForgectl(cfg, ["--version"], 10_000);
  return {
    adapterType: ADAPTER_TYPE,
    status: r.code === 0 ? "pass" : "fail",
    checks: [{ code: "forgectl_present", level: r.code === 0 ? "info" : "error", message: r.code === 0 ? r.stdout.trim() : `forgectl --version exited ${r.code}` }],
    testedAt: new Date().toISOString(),
  };
}

export function createServerAdapter(): ServerAdapterModule {
  return {
    type: ADAPTER_TYPE,
    execute,
    testEnvironment,
    sessionCodec: {
      deserialize: (raw) => {
        const s = decodeSession(raw);
        return s ? { ...s } : null;
      },
      serialize: (params) => params,
      getDisplayId: (params) => decodeSession(params)?.workerName ?? null,
    },
    agentConfigurationDoc: "repo (required), harness (claude or codex; default claude), herdrSession (default paperclip), profile (default main), callbackPort (default 3199), turnTimeoutSec (default 1800).",
  };
}
