import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { join, relative } from "node:path";
import { ProjectPresentationAcceptance } from "./project-presentation-acceptance.mjs";

export function assertBoundRun(report, projectId, threadId, member, previousRuns = []) {
  assert.equal(report.project_id, projectId);
  assert.equal(report.thread.id, threadId);
  assert.equal(report.thread.active_run_id, member.run_id);
  assert.equal(report.thread.runs.length, previousRuns.length + 1);
  assert.deepEqual(report.thread.runs.slice(0, -1), previousRuns);
  const binding = report.thread.runs.at(-1);
  for (const [field, expected] of Object.entries({
    member: member.name, run_id: member.run_id, harness: member.harness,
    native_session_id: member.thread_id ?? "", worktree: member.worktree,
    socket: member.socket ?? "", pane_id: member.pane_id ?? "",
  })) assert.equal(binding[field] ?? "", expected, `project binding ${field}`);
  return binding;
}

export function assertNativeEvidence(report, projectId, threadId, binding, expected = {}) {
  assert.equal(report.project_id, projectId);
  assert.equal(report.thread_id, threadId);
  assert.equal(report.run_id, binding.run_id);
  assert.equal(report.process_liveness, "unknown");
  assert.equal(report.native.native_session_id, binding.native_session_id);
  assert.equal(typeof report.native.provider, "string");
  assert.equal(typeof report.native.model, "string");
  assert(report.native.provider.length > 0 && report.native.model.length > 0);
  if (expected.provider) assert.equal(report.native.provider, expected.provider);
  if (expected.model) assert.equal(report.native.model, expected.model);
  if (expected.completed) {
    assert.equal(report.native.turn.status, "completed");
    assert.equal(report.evidence?.status, "completed");
    assert.equal(report.evidence.thread_id, threadId);
    assert.equal(report.evidence.run_id, binding.run_id);
    assert.equal(report.evidence.native_session_id, binding.native_session_id);
    assert.equal(report.evidence.harness, binding.harness);
    assert.equal(report.evidence.turn_id, report.native.turn.id);
    const sources = {
      codex: ["codex-thread-read", "codex-turn-history"],
      pi: ["pi-inbox", "pi-agent-events"],
      hermes: ["hermes-supervisor", "hermes-prompt"],
    };
    assert(sources[binding.harness], "unknown evidence harness");
    assert.equal(report.evidence.source, sources[binding.harness][0]);
    const activity = report.native.activity;
    if (activity) {
      assert.equal(activity.version, 1);
      assert.equal(activity.source, sources[binding.harness][1]);
      assert(Array.isArray(activity.spans));
    }
    const intervals = (activity?.spans ?? []).filter((span) => span.turn_id === report.native.turn.id && span.ended_at);
    assert(intervals.length <= 1, "ambiguous native interval");
    const interval = intervals[0];
    if (interval) {
      assert.equal(interval.state, "finished");
      const started = Date.parse(interval.started_at), ended = Date.parse(interval.ended_at);
      assert(Number.isFinite(started) && Number.isFinite(ended) && started <= ended);
      assert(ended <= Date.parse(activity.observed_at));
      assert(ended <= Date.parse(report.evidence.observed_at));
    }
    for (const field of ["started_at", "ended_at", "provider", "model"]) {
      assert.equal(report.evidence[field], interval?.[field], `native interval ${field}`);
    }
  }
  return report;
}

export class ProjectAcceptance {
  constructor({ evidence, runCommand, forgectl, root, worktree, env, herdr }) {
    this.evidence = evidence;
    this.runCommand = runCommand;
    this.forgectl = forgectl;
    this.root = join(root, "projects");
    this.worktree = worktree;
    this.env = env;
    this.threads = new Map();
    this.bindings = new Map();
    this.histories = new Map();
    this.recordedEvidence = [];
    this.presentationOptions = herdr ? { herdr, root } : null;
  }

  async command(args, { allowFailure = false, asJSON = true } = {}) {
    const result = await this.runCommand(this.evidence, this.forgectl, ["surface", "project", "--root", this.root, ...args, ...(asJSON ? ["--json"] : [])], { env: this.env, cwd: this.worktree, timeout: 120_000 });
    if (allowFailure) return result;
    assert.equal(result.code, 0, `project command ${args[0]} failed: ${result.stderr}`);
    return asJSON ? JSON.parse(result.stdout) : result.stdout;
  }

  async create(label, workerName) {
    const created = await this.command(["create", label, "--worktree", this.worktree]);
    this.project = created.project;
    this.ledger = created.ledger;
    assert.match(this.project.id, /^[a-f0-9-]{36}$/);
    assert.equal(this.ledger, join(this.root, this.project.id, "team"));
    assert.equal(this.project.threads.length, 1);
    assert.equal(this.project.threads[0].role, "coordinator");
    assert.equal(this.project.threads[0].id, this.project.coordinator_thread_id);
    this.threads.set("coordinator", this.project.coordinator_thread_id);
    const worker = await this.command(["thread", "--project", this.project.id, "create", workerName]);
    assert.equal(worker.thread.role, "worker");
    this.threads.set("worker", worker.thread.id);
    const unrelated = await this.command(["create", "Unrelated same-directory project", "--worktree", this.worktree]);
    assert.notEqual(unrelated.project.id, this.project.id);
    assert.notEqual(unrelated.ledger, this.ledger);
    this.unrelated = unrelated.project;
    this.evidence.add("project-created", { root: this.root, projectId: this.project.id, coordinatorThread: this.threads.get("coordinator"), workerThread: this.threads.get("worker"), unrelatedProject: this.unrelated.id, ledger: this.ledger });
    return this;
  }

  thread(name) {
    const threadId = this.threads.get(name);
    assert(threadId, `unknown acceptance thread ${name}`);
    return threadId;
  }

  async bind(name, registration) {
    assert.equal(registration.ledger, this.ledger);
    const member = registration.member;
    assert(member.run_id && member.thread_id, "bind only an exact native enrollment");
    const threadId = this.thread(name);
    const previous = this.bindings.get(name);
    const history = this.histories.get(name) ?? [];
    assert(!previous || previous.run_id !== member.run_id, "acceptance replacement needs a fresh run");
    if (previous && this.presentation) await this.presentation.closeView(name);
    const bound = await this.command(["thread", "--project", this.project.id, "bind", threadId, "--member", member.name, "--run-id", member.run_id, "--expect-run", previous?.run_id ?? "none"]);
    const binding = assertBoundRun(bound, this.project.id, threadId, member, history);
    this.bindings.set(name, binding);
    this.histories.set(name, [...history, binding]);
    const shown = await this.command(["thread", "--project", this.project.id, "show", threadId]);
    assert.equal(shown.observation.enrollment, "current");
    assert.equal(shown.observation.native_session_known, true);
    assert.equal(shown.observation.process_liveness, "unknown");
    if (previous) {
      const before = await readFile(join(this.root, "registry.json"));
      const rejected = await this.command(["thread", "--project", this.project.id, "observe", threadId, "--run-id", previous.run_id], { allowFailure: true });
      assert.equal(rejected.code, 1, "retired run observation must fail");
      assert.deepEqual(await readFile(join(this.root, "registry.json")), before);
    }
    this.evidence.add("project-run-bound", { projectId: this.project.id, threadId, role: name, runId: binding.run_id, previousRunId: previous?.run_id ?? null, nativeSessionId: binding.native_session_id, harness: binding.harness, historicalRuns: history.length });
    if (previous && this.presentation) await this.presentation.openTimeline(name);
    return binding;
  }

  async presentNative(options) {
    assert(this.presentationOptions, "live Projects acceptance requires explicit isolated Herdr presentation");
    if (!this.presentation) {
      this.presentation = new ProjectPresentationAcceptance(this, this.presentationOptions);
      await this.presentation.start();
    }
    await this.presentation.native(options);
  }

  async cleanup() { await this.presentation?.close(); }

  async observe(name, expected = {}) {
    const binding = this.bindings.get(name);
    assert(binding, `thread ${name} must be bound before native observation`);
    const threadId = this.thread(name);
    const report = await this.command(["thread", "--project", this.project.id, "observe", threadId, "--run-id", binding.run_id]);
    assertNativeEvidence(report, this.project.id, threadId, binding, expected);
    if (report.evidence) this.recordedEvidence.push(report.evidence);
    this.evidence.add("project-native-observed", { projectId: this.project.id, threadId, runId: binding.run_id, nativeSessionId: binding.native_session_id, provider: report.native.provider, model: report.native.model, turn: report.evidence ?? null, processLiveness: report.process_liveness });
    return report;
  }

  async verifyArtifact(name, path, digest) {
    const binding = this.bindings.get(name);
    assert(binding, `thread ${name} must be bound before artifact verification`);
    const relativePath = relative(binding.worktree, path);
    assert(relativePath && !relativePath.startsWith("..") && !relativePath.startsWith("/"));
    const threadId = this.thread(name);
    const args = ["thread", "--project", this.project.id, "verify-artifact", threadId, "--run-id", binding.run_id, "--path", relativePath, "--sha256"];
    const before = await readFile(join(this.root, "registry.json"));
    const wrong = await this.command([...args, digest === "0".repeat(64) ? "1".repeat(64) : "0".repeat(64)], { allowFailure: true });
    assert.equal(wrong.code, 1, "wrong artifact digest must fail");
    assert.deepEqual(await readFile(join(this.root, "registry.json")), before);
    const report = await this.command([...args, digest]);
    assert.equal(report.project_id, this.project.id);
    assert.equal(report.thread_id, threadId);
    assert.equal(report.run_id, binding.run_id);
    assert.equal(report.evidence.thread_id, threadId);
    assert.equal(report.evidence.run_id, binding.run_id);
    assert.equal(report.evidence.native_session_id, binding.native_session_id);
    assert.equal(report.evidence.harness, binding.harness);
    assert.equal(report.evidence.source, "artifact-digest");
    assert.equal(report.evidence.status, "digest_match");
    assert.equal(report.evidence.artifact.sha256, digest);
    assert.equal(report.evidence.artifact.relative_path, relativePath);
    assert.equal(report.evidence.turn_id, undefined);
    assert.equal(report.process_liveness, "unknown");
    this.recordedEvidence.push(report.evidence);
    this.evidence.add("project-artifact-bytes-verified", { projectId: this.project.id, threadId, runId: binding.run_id, sha256: digest, relativePath, scope: "byte identity only; executable correctness checks are separate" });
    return report;
  }

  async audit() {
    const shown = await this.command(["show", this.project.id]);
    for (const [name, threadId] of this.threads) {
      const thread = shown.project.threads.find((entry) => entry.id === threadId);
      assert(thread, "logical thread disappeared");
      assert.equal(thread.active_run_id, this.bindings.get(name)?.run_id);
      assert.deepEqual(thread.runs, this.histories.get(name));
    }
    for (const evidence of this.recordedEvidence) assert(shown.project.evidence.some((entry) => JSON.stringify(entry) === JSON.stringify(evidence)), "historical evidence disappeared");
    const unrelated = await this.command(["show", this.unrelated.id]);
    assert.deepEqual(unrelated.project, this.unrelated);
    const timeline = await this.command(["timeline", this.project.id]);
    const text = JSON.stringify(timeline);
    assert(text.includes("NATIVE-TURN-COMPLETED"), "rendered timeline has no completed native turn");
    assert(text.includes("ARTIFACT-BYTES-MATCHED"), "rendered timeline has no artifact byte verification");
    const plain = await this.command(["timeline", this.project.id], { asJSON: false });
    assert(plain.includes("NATIVE-TURN-COMPLETED") && plain.includes("ARTIFACT-BYTES-MATCHED") && plain.includes("unknown"));
    this.evidence.add("project-durable-audit", { projectId: this.project.id, threadIds: [...this.threads.values()], evidenceRecords: shown.project.evidence.length, unrelatedProjectUnchanged: true, jsonAndTextEvidenceRendered: true });
    if (this.presentationOptions) {
      assert(this.presentation, "presentation acceptance never ran");
      await this.presentation.render("worker");
      await this.presentation.render("coordinator");
      await this.presentation.audit();
    }
    return shown;
  }
}
