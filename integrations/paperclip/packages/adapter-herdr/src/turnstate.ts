// Every filesystem write the adapter makes. The worker can write inside its
// own worktree and reads untrusted issue text, so nothing the adapter trusts
// lives there and the adapter writes nothing there.
import { constants } from "node:fs";
import { lstat, mkdir, open, realpath, rm, type FileHandle } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

const ID_RE = /^[A-Za-z0-9-]{1,64}$/;
const WRITE_NEW = constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW;

/**
 * Where turn files live: outside every worktree, under $HOME/.local/state.
 * XDG_STATE_HOME is ignored on purpose: forgectl does not pass it to workers,
 * so the Stop hook could not follow it there.
 */
export function stateBase(env: NodeJS.ProcessEnv = process.env): string {
  const home = env["HOME"] && path.isAbsolute(env["HOME"]) ? env["HOME"] : os.homedir();
  return path.join(home, ".local", "state");
}

export function turnStateDir(env: NodeJS.ProcessEnv = process.env): string {
  return path.join(stateBase(env), "forgectl-paperclip", "turns");
}

async function writeNew(file: string, content: string, mode = 0o600): Promise<void> {
  let fh: FileHandle | null = null;
  try {
    fh = await open(file, WRITE_NEW, mode);
    await fh.writeFile(content, "utf8");
  } finally {
    await fh?.close();
  }
}

/**
 * Create (or check) a private directory under the state base: every component
 * below the base must be a real directory (no symlink), and the leaf must be
 * owned by this user with mode 0700.
 */
async function privateDir(dir: string): Promise<string> {
  await mkdir(dir, { recursive: true, mode: 0o700 });
  const base = stateBase();
  const expected = path.join(await realpath(base), path.relative(base, dir));
  if ((await realpath(dir)) !== expected) throw new Error(`${dir} resolves through a symlink below ${base}; refusing to use it`);
  const st = await lstat(dir);
  const uid = process.getuid?.() ?? st.uid;
  if (st.isSymbolicLink() || !st.isDirectory() || st.uid !== uid || (st.mode & 0o077) !== 0) {
    throw new Error(`${dir} must be a 0700 directory owned by uid ${uid} (mode ${(st.mode & 0o777).toString(8)}, uid ${st.uid})`);
  }
  return dir;
}

/**
 * Write the turn file the worker's Stop hook reads: run id, one-time token and
 * callback URL, keyed by the Claude session id forgectl assigned the worker.
 * Returns the path to remove when the turn ends.
 */
export async function writeTurnFile(sessionId: string, turn: { runId: string; token: string; callbackUrl: string }): Promise<string> {
  if (!ID_RE.test(sessionId)) throw new Error(`refusing turn file for session id ${JSON.stringify(sessionId)}: not [A-Za-z0-9-]{1,64}`);
  const dir = await privateDir(turnStateDir());
  const file = path.join(dir, `${sessionId}.json`);
  await rm(file, { force: true });
  await writeNew(file, JSON.stringify(turn));
  return file;
}

/**
 * Write the brief into the private state dir, never into the worktree: the
 * worker can write its worktree and swap a directory for a symlink between any
 * check and the open, and Node has no openat to close that race. The worker is
 * briefed with the absolute path. Returns the file to remove.
 */
export async function writeBrief(runId: string, content: string): Promise<string> {
  if (!ID_RE.test(runId)) throw new Error(`refusing brief for run id ${JSON.stringify(runId)}: not [A-Za-z0-9-]{1,64}`);
  const dir = await privateDir(path.join(stateBase(), "forgectl-paperclip", "briefs"));
  const file = path.join(dir, `${runId}.md`);
  await rm(file, { force: true });
  await writeNew(file, content);
  return file;
}

/** Remove turn files. Unlink never follows a link. */
export async function removeTurnFiles(files: string[]): Promise<void> {
  for (const f of files) await rm(f, { force: true });
}

/** Read the bytes a turn appended to forgectl's transcript, capped so a huge file cannot exhaust memory. */
export async function readAppended(file: string, offset: number, maxBytes = 32 << 20): Promise<string[]> {
  const st = await lstat(file);
  if (!st.isFile()) throw new Error(`transcript ${file} is not a regular file`);
  // O_NONBLOCK: a FIFO swapped in after the lstat cannot hang the open.
  const fh = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const opened = await fh.stat();
    if (!opened.isFile()) throw new Error(`transcript ${file} is not a regular file`);
    const { size } = opened;
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

/** Write this turn's report script (0700, exclusive, no link followed) next to its turn file. */
export async function writeReportScript(sessionId: string, content: string): Promise<string> {
  if (!ID_RE.test(sessionId)) throw new Error(`refusing report script for session id ${JSON.stringify(sessionId)}: not [A-Za-z0-9-]{1,64}`);
  const dir = await privateDir(turnStateDir());
  const file = path.join(dir, `${sessionId}.report.sh`);
  await rm(file, { force: true });
  await writeNew(file, content, 0o700);
  return file;
}

/**
 * Read the operator config file named by FORGECTL_PAPERCLIP_CONFIG; unset means
 * none, and a named file that is missing is an error. There is no default path: the adapter registry is shared
 * by every Paperclip instance under one PAPERCLIP_HOME, so each instance names
 * its own file in its environment (install-service.sh puts it in the
 * instance's .env). The opened file must be a regular file owned by this user
 * with no group or other access. The checks run on the opened descriptor, and
 * O_NONBLOCK keeps a FIFO from hanging the open.
 */
export async function readOperatorConfig(env: NodeJS.ProcessEnv = process.env): Promise<unknown> {
  const file = env["FORGECTL_PAPERCLIP_CONFIG"];
  if (!file) return {};
  if (!path.isAbsolute(file)) throw new Error(`FORGECTL_PAPERCLIP_CONFIG must be an absolute path; got ${JSON.stringify(file)}`);
  let fh;
  try {
    fh = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === "ENOENT") throw new Error(`FORGECTL_PAPERCLIP_CONFIG names ${file}, which does not exist`);
    if (code === "ELOOP") throw new Error(`operator config ${file} must be a regular file, not a symlink`);
    throw err;
  }
  try {
    const st = await fh.stat();
    const uid = process.getuid?.() ?? st.uid;
    if (!st.isFile() || st.uid !== uid || (st.mode & 0o077) !== 0) {
      throw new Error(`operator config ${file} must be a regular file owned by uid ${uid} with mode 0600`);
    }
    return JSON.parse(await fh.readFile("utf8"));
  } finally {
    await fh.close();
  }
}
