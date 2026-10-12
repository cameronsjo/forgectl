// Paperclip server adapter: each execute() is one turn in a long-lived herdr
// pane that forgectl surface launched, briefed, and reads. Paperclip's
// heartbeat decides when a turn runs; forgectl decides how the pane is
// started, briefed and read.
//
// Trust boundary: the worker runs as the operator, can write anywhere in its
// worktree, and reads untrusted issue text. The adapter therefore writes
// nothing into the worktree (the turn's token, callback and brief live in a
// private state dir), reads usage only from the transcript path forgectl
// recorded, and accepts a Stop callback only for the worker's own session.
import { randomBytes } from "node:crypto";
import { realpath } from "node:fs/promises";
import path from "node:path";
import type {
  AdapterEnvironmentTestContext,
  AdapterEnvironmentTestResult,
  AdapterExecutionContext,
  AdapterExecutionResult,
  ServerAdapterModule,
} from "@paperclipai/adapter-utils";
import { awaitStop, ensureListener, type StopWait } from "./callback.js";
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
import { readAppended, removeTurnFiles, sizeOf, writeBrief, writeTurnFile } from "./turnstate.js";

type Log = (msg: string) => Promise<void>;

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

/** forgectl puts worker worktrees under <repo>/.claude/worktrees/; refuse a worker whose row says otherwise. */
async function checkedWorktree(cfg: AdapterConfig, w: ListedWorker): Promise<string> {
  if (!w.worktree) throw new Error(`worker ${w.name} has no worktree in forgectl surface list --json`);
  const root = path.join(await realpath(cfg.repo), ".claude", "worktrees") + path.sep;
  const wt = await realpath(w.worktree);
  if (!wt.startsWith(root)) throw new Error(`worker ${w.name} worktree ${wt} is outside ${root}`);
  return wt;
}

function verdictReason(v: Record<string, unknown>): string {
  return String(v["reason"] ?? v["blocking"] ?? v["outcome"] ?? "unknown");
}

function isBlocked(v: Record<string, unknown>): boolean {
  return typeof v["blocking"] === "string" && v["blocking"] !== "";
}

async function execute(ctx: AdapterExecutionContext): Promise<AdapterExecutionResult> {
  const log: Log = (msg) => ctx.onLog("stdout", `[herdr-forgectl] ${msg}\n`);
  let cfg: AdapterConfig;
  try {
    cfg = parseConfig(ctx.config);
  } catch (err) {
    return failure((err as Error).message, "invalid_config", null);
  }
  const stored = decodeSession(ctx.runtime.sessionParams);
  let known: WorkerSession | null = stored;
  try {
    await ensureListener(cfg.callbackPort);
    const fallback = workerNameFor(ctx.agent.id, ctx.runtime.taskKey);
    let workers = await listWorkers(cfg);
    const plan = planTurn(stored, fallback, workers);
    let worker: ListedWorker;

    if (plan.kind === "launch") {
      await log(`Attempting launch of a new pane. Worker: ${plan.workerName}, Session: ${cfg.herdrSession}, Harness: ${cfg.harness}, Profile: ${cfg.profile}`);
      ctx.onDispatch?.();
      const r = await launchWorker(cfg, plan.workerName);
      if (r.code !== 0) return failure(`forgectl surface launch exited ${r.code}: ${r.stderr.trim().slice(0, 800)}`, "launch_failed", stored);
      workers = await listWorkers(cfg);
      const row = workers.find((w) => w.name === plan.workerName);
      if (!row) return failure(`launched ${plan.workerName} but surface list does not show it`, "launch_unlisted", stored);
      worker = row;
      known = sessionFromWorker(cfg, worker);
      await log(`Successfully launched pane. Worker: ${worker.name}, Pane: ${worker.pane_id}, Workspace: ${worker.workspace_id}`);
    } else {
      worker = plan.worker;
      known = sessionFromWorker(cfg, worker);
      ctx.onDispatch?.();
      await log(`${plan.reattached ? "Briefing stored-session" : "Reusing"} live pane. Worker: ${worker.name}, Pane: ${worker.pane_id}`);
    }

    // A fresh pane, and a pane whose Stop hook just fired, may not be at its
    // prompt yet; a brief typed before then fails read-back.
    const ready = await waitReady(cfg, worker.name);
    if (ready.code !== 0) {
      return failure(`worker ${worker.name} is not at its prompt: ${verdictReason(ready)}`, isBlocked(ready) ? "worker_blocked" : "not_ready", known);
    }

    // The adapter writes nothing into the worktree, but a worker whose ledger row
    // points outside <repo>/.claude/worktrees/ is not one forgectl launched for this repo.
    try {
      await checkedWorktree(cfg, worker);
    } catch (err) {
      return failure((err as Error).message, "worktree_rejected", known);
    }
    return await runTurn(ctx, cfg, worker, known, plan.kind === "brief" && plan.reattached, log);
  } catch (err) {
    // Anything unexpected still returns a coded result that keeps the session, so the next wake can find the pane.
    await log(`Encountered ${(err as Error).name} while running the turn: ${(err as Error).message}`);
    return failure(`adapter error: ${(err as Error).message}`, "adapter_error", known);
  }
}

async function runTurn(
  ctx: AdapterExecutionContext,
  cfg: AdapterConfig,
  worker: ListedWorker,
  session: WorkerSession,
  reattached: boolean,
  log: Log,
): Promise<AdapterExecutionResult> {
  if (cfg.harness === "claude" && !worker.session_id) {
    return failure(`worker ${worker.name} has no Claude session id in forgectl's ledger; its Stop hook cannot be matched`, "no_session_id", session);
  }
  const token = randomBytes(32).toString("hex");
  const nonce = randomBytes(12).toString("hex");
  const written: string[] = [];
  let stop: StopWait | null = null;
  try {
    const briefFile = await writeBrief(ctx.runId, composeBrief(ctx.runId, ctx.agent.name, extractIssue(ctx.context), nonce));
    written.push(briefFile);
    if (cfg.harness === "claude" && worker.session_id) {
      written.push(await writeTurnFile(worker.session_id, { runId: ctx.runId, token, callbackUrl: `http://127.0.0.1:${cfg.callbackPort}/stop` }));
      stop = awaitStop(ctx.runId, token, cfg.turnTimeoutSec * 1000, ctx.signal);
    }
    const offset = await sizeOf(worker.transcript);

    const sent = await brief(cfg, worker.name, `Read ${briefFile} and do what it asks.`);
    if (sent.code !== 0) return failure(`forgectl surface brief refused: ${verdictReason(sent)}`, "brief_refused", session);
    await log(`Brief sent. Marker: ${String(sent["marker"] ?? "")}; waiting for the ${stop ? "Stop hook" : "turn to settle"}`);

    if (stop) {
      const watchDone = new AbortController();
      const outcome = await Promise.race([
        stop.result,
        watchForBlocking(cfg, worker.name, watchDone.signal).then((blocking) => ({ kind: "blocked" as const, blocking })),
      ]);
      watchDone.abort();
      if (outcome.kind === "blocked") {
        // The pane stays open at its dialog; the next wake reuses it once a person answers.
        return failure(`worker ${worker.name} is waiting at a ${outcome.blocking ?? "dialog"}; answer it in the pane (herdr --session ${cfg.herdrSession})`, "worker_blocked", session);
      }
      if (outcome.kind === "cancelled") return failure("the run was cancelled before the worker's turn ended; the pane keeps running", "cancelled", session);
      if (outcome.kind === "timeout") return { ...failure(`no Stop hook within ${cfg.turnTimeoutSec}s`, "turn_timeout", session), timedOut: true };
      if (outcome.cb.sessionId !== worker.session_id) {
        return failure(`Stop callback named session ${String(outcome.cb.sessionId)}, but the worker's session is ${String(worker.session_id)}`, "stop_session_mismatch", session);
      }
      await log(`Stop hook received. Claude session: ${outcome.cb.sessionId}`);
    } else {
      const settled = await waitSettled(cfg, worker.name, cfg.turnTimeoutSec);
      if (settled.code !== 0) {
        const blocked = isBlocked(settled);
        return { ...failure(`turn did not settle: ${verdictReason(settled)}`, blocked ? "worker_blocked" : "turn_timeout", session), timedOut: !blocked };
      }
    }

    const report = await readReport(cfg, worker.name);
    if (!stop && report["found"] !== true) {
      // A settled codex pane with no REPORT line may never have started the turn; forgectl's wait cannot tell.
      return failure("codex turn settled without its REPORT line", "report_missing", session);
    }

    // Usage comes only from the transcript path forgectl recorded at launch, never from the callback body.
    let usage: ReturnType<typeof sumTranscriptUsage> | null = null;
    if (cfg.harness === "claude" && worker.transcript) {
      try {
        usage = sumTranscriptUsage(await readAppended(worker.transcript, offset));
      } catch (err) {
        await log(`Encountered ${(err as Error).message} while reading usage; the run succeeds without usage. Transcript: ${worker.transcript}`);
      }
    }

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
      sessionParams: { ...session },
      sessionDisplayId: worker.name,
      summary: typeof report["report"] === "string" ? report["report"] : null,
      resultJson: { worker: worker.name, pane: worker.pane_id, report: report["report"] ?? null, reportFound: report["found"] ?? false, reattached },
    };
  } finally {
    stop?.cancel();
    await removeTurnFiles(written);
  }
}

async function testEnvironment(_ctx: AdapterEnvironmentTestContext): Promise<AdapterEnvironmentTestResult> {
  const forgectlPath = process.env["FORGECTL_PAPERCLIP_FORGECTL"] || "forgectl";
  const r = await runForgectl({ forgectlPath, herdrSession: process.env["FORGECTL_PAPERCLIP_HERDR_SESSION"] || "paperclip" } as AdapterConfig, ["--version"], 10_000);
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
    agentConfigurationDoc:
      "repo (required; one of the server's FORGECTL_PAPERCLIP_REPOS), harness (claude or codex; default claude), profile (claude only; default main), turnTimeoutSec (30-86400; default 1800). forgectl path, herdr session and callback port are operator-only server env vars.",
  };
}
