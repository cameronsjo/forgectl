#!/usr/bin/env node

import assert from "node:assert/strict";
import { assertNoDuplicateMarkers, assertProjectOperation, assertThreadOperation, commandEnvironment, ownershipMarker, parseArgs } from "./verify-project-presentation.mjs";

assert.throws(() => parseArgs(["--forgectl", "/f", "--herdr", "/h", "--root", "/r"]), /without explicit --live/);
assert.throws(() => parseArgs(["--live", "--forgectl", "forgectl", "--herdr", "/h", "--root", "/r"]), /--forgectl must be an absolute path/);
const args = parseArgs(["--live", "--forgectl", "/f", "--herdr", "/h", "--root", "/r"]);
assert.deepEqual(args, { live: true, forgectl: "/f", herdr: "/h", root: "/r" });

const server = "a".repeat(64);
const endpoint = "b".repeat(64);
const projectTag = "0123456789abcdef0123456789abcdef";
const threadTag = "fedcba9876543210fedcba9876543210";
const refFields = { version: 2, kind: "herdr", source: "herdr-named-session", server, endpoint };
const projectOperation = {
  id: "op-project",
  kind: "workspace",
  session: "projects-test",
  phase: "verified",
  tag: projectTag,
  expected_server: server,
  expected_endpoint: endpoint,
  ref: { ...refFields, tag: projectTag, workspace: "w1", tab: "w1:t1", pane: "w1:p1", terminal: "term-1" },
};
assert.equal(assertProjectOperation(projectOperation, "project", "projects-test", "Target Project"), projectOperation);

const threadOperation = {
  id: "op-thread",
  kind: "thread",
  thread_id: "thread-1",
  run_id: "run-1",
  session: "projects-test",
  presentation: "timeline",
  phase: "verified",
  tag: threadTag,
  expected_server: server,
  expected_endpoint: endpoint,
  parent: projectOperation.ref,
  ref: { ...refFields, tag: threadTag, workspace: "w1", tab: "w1:t2", pane: "w1:p2", terminal: "term-2" },
};
assert.equal(assertThreadOperation(threadOperation, "project", "thread-1", "run-1", "projects-test"), threadOperation);

const projectMarker = ownershipMarker(projectOperation);
const threadMarker = ownershipMarker(threadOperation);
assert.equal(projectMarker, `fc-surface-${projectTag}`);
assert.throws(() => ownershipMarker({ ...projectOperation, tag: projectMarker }));
assert.throws(() => ownershipMarker({ ...projectOperation, expected_server: "c".repeat(64) }));
assert.throws(() => assertProjectOperation({ ...projectOperation, ref: { ...projectOperation.ref, source: "herdr-default-session" } }, "project", "projects-test", "Target Project"));
for (const field of ["tab", "pane", "terminal", "server", "endpoint"]) {
  const replacement = ["server", "endpoint"].includes(field) ? "c".repeat(64) : projectOperation.ref[field];
  assert.throws(() => assertThreadOperation({ ...threadOperation, ref: { ...threadOperation.ref, [field]: replacement } }, "project", "thread-1", "run-1", "projects-test"));
}
const workspaces = [{ workspace_id: "w1", label: projectMarker }, { workspace_id: "w2", label: "unrelated" }];
const tabs = [{ tab_id: "w1:t2", label: threadMarker }];
assert.doesNotThrow(() => assertNoDuplicateMarkers(workspaces, tabs, projectMarker, threadMarker));
assert.throws(() => assertNoDuplicateMarkers([...workspaces, { workspace_id: "w3", label: projectMarker }], tabs, projectMarker, threadMarker), /must be unique/);
assert.throws(() => assertNoDuplicateMarkers(workspaces, [...tabs, tabs[0]], projectMarker, threadMarker), /must be unique/);
assert.doesNotThrow(() => assertNoDuplicateMarkers([{ workspace_id: "w1", label: `Project · ${projectMarker}` }], [{ tab_id: "w1:t2", label: `Thread · ${threadMarker}` }], projectMarker, threadMarker));
assert.throws(() => assertNoDuplicateMarkers([...workspaces, { workspace_id: "w3", label: `Different name · ${projectMarker}` }], tabs, projectMarker, threadMarker), /must be unique/);
assert.throws(() => assertNoDuplicateMarkers(workspaces, [...tabs, { tab_id: "w1:t3", label: `Different name · ${threadMarker}` }], projectMarker, threadMarker), /must be unique/);
const env = commandEnvironment({ OPENAI_API_KEY: "not-real", HTTP_PROXY: "not-real", SSH_AUTH_SOCK: "/ambient", FORGECTL_LEDGER: "/ambient", HERDR_SESSION: "fleet", HOME: "/ambient", SHELL: "/ambient", PATH: "/ambient" }, "/private/task");
for (const key of ["OPENAI_API_KEY", "HTTP_PROXY", "SSH_AUTH_SOCK", "FORGECTL_LEDGER", "HERDR_SESSION"]) assert.equal(env[key], undefined);
assert.equal(env.HOME, "/private/task/home");
assert.equal(env.SHELL, "/bin/sh");
assert.equal(env.ENV, "/dev/null");
assert(!env.PATH.includes("ambient"));

process.stdout.write("project presentation verifier contract tests passed\n");
