// Thin client over `forgectl surface ... --json`. Every call passes argv as an
// array (no shell) and pins the herdr session through HERDR_SESSION.
import { execFile } from "node:child_process";
import type { AdapterConfig, ListedWorker } from "./core.js";

export interface ForgectlResult {
  code: number;
  stdout: string;
  stderr: string;
}

/** herdr handles of the process that started Paperclip; a worker must not inherit them. */
const INHERITED_HERDR_KEYS = ["HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID"] as const;

export function forgectlEnv(base: NodeJS.ProcessEnv, herdrSession: string): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...base, HERDR_SESSION: herdrSession };
  for (const k of INHERITED_HERDR_KEYS) delete env[k];
  return env;
}

export function runForgectl(cfg: AdapterConfig, args: string[], timeoutMs: number): Promise<ForgectlResult> {
  return new Promise((resolve) => {
    execFile(
      cfg.forgectlPath,
      args,
      { env: forgectlEnv(process.env, cfg.herdrSession), timeout: timeoutMs, maxBuffer: 8 * 1024 * 1024 },
      (err, stdout, stderr) => {
        const code = err ? (typeof (err as { code?: unknown }).code === "number" ? ((err as { code: number }).code) : 1) : 0;
        resolve({ code, stdout: String(stdout), stderr: String(stderr) });
      },
    );
  });
}

function parseJson(r: ForgectlResult, what: string): Record<string, unknown> {
  try {
    const v: unknown = JSON.parse(r.stdout);
    if (v && typeof v === "object" && !Array.isArray(v)) return v as Record<string, unknown>;
  } catch {
    // fall through to the error below
  }
  throw new Error(`forgectl ${what} printed no JSON object (exit ${r.code}). stderr: ${r.stderr.trim().slice(0, 500)}`);
}

export async function listWorkers(cfg: AdapterConfig): Promise<ListedWorker[]> {
  const r = await runForgectl(cfg, ["surface", "list", "--repo", cfg.repo, "--json"], 30_000);
  const obj = parseJson(r, "surface list");
  const workers = obj["workers"];
  if (!Array.isArray(workers)) throw new Error(`forgectl surface list --json has no workers array (exit ${r.code})`);
  return workers as ListedWorker[];
}

export async function launchWorker(cfg: AdapterConfig, name: string): Promise<ForgectlResult> {
  const args = [
    "surface", "launch", cfg.repo,
    "--surface", "herdr",
    "--worktree", `paperclip/${name}`,
    "--name", name,
    "--harness", cfg.harness,
  ];
  if (cfg.profile !== "main") args.push("--profile", cfg.profile);
  return runForgectl(cfg, args, 180_000);
}

export async function waitReady(cfg: AdapterConfig, name: string): Promise<Record<string, unknown> & { code: number }> {
  const r = await runForgectl(cfg, ["surface", "ready", name, "--repo", cfg.repo, "--timeout", "2m", "--json"], 150_000);
  return { ...parseJson(r, "surface ready"), code: r.code };
}

export async function brief(cfg: AdapterConfig, name: string, text: string): Promise<Record<string, unknown> & { code: number }> {
  const r = await runForgectl(cfg, ["surface", "brief", name, text, "--repo", cfg.repo, "--json"], 60_000);
  return { ...parseJson(r, "surface brief"), code: r.code };
}

/** Waits for a codex turn to settle; the caller still requires the REPORT line before calling it done. */
export async function waitSettled(cfg: AdapterConfig, name: string, timeoutSec: number): Promise<Record<string, unknown> & { code: number }> {
  const r = await runForgectl(cfg, ["surface", "wait", name, "--repo", cfg.repo, "--timeout", `${timeoutSec}s`, "--json"], (timeoutSec + 30) * 1000);
  return { ...parseJson(r, "surface wait"), code: r.code };
}

export async function readReport(cfg: AdapterConfig, name: string): Promise<Record<string, unknown> & { code: number }> {
  const r = await runForgectl(cfg, ["surface", "read", name, "--report", "--repo", cfg.repo, "--json"], 30_000);
  return { ...parseJson(r, "surface read"), code: r.code };
}
