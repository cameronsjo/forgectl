// Every filesystem write the adapter makes. The worker can write inside its
// own worktree and reads untrusted issue text, so nothing the adapter trusts
// lives there, and nothing it writes there follows a link the worker planted.
import { constants } from "node:fs";
import { lstat, mkdir, open, realpath, rm, rmdir, type FileHandle } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

const ID_RE = /^[A-Za-z0-9-]{1,64}$/;
const WRITE_NEW = constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW;

/** Where turn files live: outside every worktree, mode 0700. */
export function turnStateDir(env: NodeJS.ProcessEnv = process.env): string {
  const base = env["XDG_STATE_HOME"] && path.isAbsolute(env["XDG_STATE_HOME"]) ? env["XDG_STATE_HOME"] : path.join(os.homedir(), ".local", "state");
  return path.join(base, "forgectl-paperclip", "turns");
}

async function writeNew(file: string, content: string): Promise<void> {
  let fh: FileHandle | null = null;
  try {
    fh = await open(file, WRITE_NEW, 0o600);
    await fh.writeFile(content, "utf8");
  } finally {
    await fh?.close();
  }
}

/**
 * Write the turn file the worker's Stop hook reads: run id, one-time token and
 * callback URL, keyed by the Claude session id forgectl assigned the worker.
 * Returns the path to remove when the turn ends.
 */
export async function writeTurnFile(sessionId: string, turn: { runId: string; token: string; callbackUrl: string }): Promise<string> {
  if (!ID_RE.test(sessionId)) throw new Error(`refusing turn file for session id ${JSON.stringify(sessionId)}: not [A-Za-z0-9-]{1,64}`);
  const dir = turnStateDir();
  await mkdir(dir, { recursive: true, mode: 0o700 });
  const st = await lstat(dir);
  if (!st.isDirectory() || (st.mode & 0o077) !== 0) throw new Error(`turn state dir ${dir} must be a 0700 directory (mode ${(st.mode & 0o777).toString(8)})`);
  const file = path.join(dir, `${sessionId}.json`);
  await rm(file, { force: true });
  await writeNew(file, JSON.stringify(turn));
  return file;
}

/**
 * Write the brief into `<worktree>/.paperclip/` without following a symlink at
 * either level: `.paperclip` must be a real directory inside the worktree, and
 * the brief is created exclusively with O_NOFOLLOW. Returns the paths to remove.
 */
export async function writeBrief(worktree: string, runId: string, content: string): Promise<{ rel: string; file: string; dir: string }> {
  if (!ID_RE.test(runId)) throw new Error(`refusing brief for run id ${JSON.stringify(runId)}: not [A-Za-z0-9-]{1,64}`);
  const dir = path.join(await realpath(worktree), ".paperclip");
  try {
    await mkdir(dir, { mode: 0o700 });
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err;
  }
  const st = await lstat(dir);
  if (st.isSymbolicLink() || !st.isDirectory()) throw new Error(`${dir} is not a plain directory; refusing to write the brief through it`);
  if ((await realpath(dir)) !== dir) throw new Error(`${dir} resolves outside the worktree; refusing to write the brief through it`);
  const rel = `.paperclip/brief-${runId}.md`;
  const file = path.join(dir, `brief-${runId}.md`);
  await writeNew(file, content);
  return { rel, file, dir };
}

/** Remove turn files. Unlink never follows a link; rmdir only removes an empty real directory. */
export async function removeTurnFiles(files: string[], briefDir: string | null): Promise<void> {
  for (const f of files) await rm(f, { force: true });
  if (briefDir) await rmdir(briefDir).catch(() => undefined);
}

/** Read the bytes a turn appended to forgectl's transcript, capped so a huge file cannot exhaust memory. */
export async function readAppended(file: string, offset: number, maxBytes = 32 << 20): Promise<string[]> {
  const st = await lstat(file);
  if (!st.isFile()) throw new Error(`transcript ${file} is not a regular file`);
  const fh = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const { size } = await fh.stat();
    const start = Math.min(Math.max(0, offset), size);
    const len = Math.min(size - start, maxBytes);
    const buf = Buffer.alloc(len);
    await fh.read(buf, 0, len, start);
    return buf.toString("utf8").split("\n");
  } finally {
    await fh.close();
  }
}

export async function sizeOf(file: string | null): Promise<number> {
  if (!file) return 0;
  try {
    const st = await lstat(file);
    return st.isFile() ? st.size : 0;
  } catch {
    return 0;
  }
}
