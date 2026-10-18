#!/usr/bin/env node

import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createConnection, createServer } from "node:net";
import { artifactPayload, assertNativeDelivery, assertNativeReplyMailbox, closeProxy, finalizeHermesAcceptance, nativeReplyFrame, parseHermesArgs } from "./verify-hermes-collaboration.mjs";
import { HermesACPClient } from "./hermes-acp-client.mjs";

const flags = ["--live", "--root", "/private/test", "--forgectl", "/bin/forgectl", "--codex", "/bin/codex", "--codex-model", "coordinator-model", "--hermes", "/bin/hermes-acp", "--hermes-python", "/bin/python", "--hermes-home", "/private/hermes", "--hermes-model", "worker-model", "--websocket-module", "/private/ws.mjs"];
assert.equal(parseHermesArgs(flags)["hermes-model"], "worker-model");
assert.equal(parseHermesArgs([...flags, "--projects", "--herdr", "/test/herdr"]).projects, true);
assert.throws(() => parseHermesArgs([...flags, "--projects"]), /requires an absolute --herdr/);
assert.throws(() => parseHermesArgs([...flags, "--herdr", "/test/herdr"]), /requires --projects/);
assert.throws(() => parseHermesArgs(flags.slice(1)), /requires --live/);
assert.throws(() => parseHermesArgs([...flags, "--hermes-model", "other-provider:model"]), /unqualified/);
assert.throws(() => parseHermesArgs([...flags, "--hermes-python", "python"]), /must be absolute/);
assert.throws(() => parseHermesArgs([...flags, "--unknown"]), /unknown argument/);

const taskId = "task-1";
const sourceText = "const π = 'example';\n";
const payload = { encoding: "utf8", filename: "summarize-mailbox.mjs", source_text: sourceText, task_id: taskId, execution_id: "execution-1" };
const body = `COMPLETED ${taskId}\n${JSON.stringify(payload)}`;
const parsed = artifactPayload(body, taskId);
assert.equal(parsed.source.toString("utf8"), sourceText);
assert.throws(() => artifactPayload(body, "different-task"), /exact task marker/);
for (const fields of [
  { filename: "../summarize-mailbox.mjs" },
  { encoding: "base64" },
  { source_text: "" },
  { source_text: "x".repeat(12_001) },
  { source_text: "\ud800" },
  { execution_id: "" },
  { unexpected: true },
]) assert.throws(() => artifactPayload(`COMPLETED ${taskId}\n${JSON.stringify({ ...payload, ...fields })}`, taskId));

const incomingId = "answer-1";
const outgoingId = "completion-1";
const nativeInput = { sessionUpdate: "user_message_chunk", content: { type: "text", text: `[forgectl-msg] from=coordinator to=hermes-worker id=${incomingId}\n` } };
const nativeFrame = JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: incomingId, body } });
const nativeSource = { sessionUpdate: "agent_message_chunk", content: { type: "text", text: nativeFrame } };
const liveReply = { type: "hermes-native-reply", state: "published", run_id: "run-1", session_id: "session-1", message_id: incomingId, outgoing_id: outgoingId, body_sha256: "", body_bytes: Buffer.byteLength(body, "utf8") };
liveReply.body_sha256 = (await import("node:crypto")).createHash("sha256").update(body).digest("hex");
assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [liveReply], runId: "run-1", sessionId: "session-1" });
assert.throws(() => assertNativeDelivery([nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /exactly one user header/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /exactly one user header/);
for (const text of ["quoted id=answer-1", nativeInput.content.text.replace(incomingId, "answer-10"), `quoted ${nativeInput.content.text}`, nativeInput.content.text.replace("coordinator", "other")]) {
  assert.throws(() => assertNativeDelivery([{ ...nativeInput, content: { type: "text", text } }, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /exactly one user header/);
}
assert.throws(() => assertNativeDelivery([nativeInput], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /exactly one reply frame/);
assert.throws(() => assertNativeDelivery([nativeInput, { ...nativeSource, sessionUpdate: "user_message_chunk" }], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /exactly one reply frame/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /malformed|exactly one reply frame/);
assert.throws(() => assertNativeDelivery([nativeInput, { sessionUpdate: "tool_call", kind: "execute", content: { command: "/bin/forgectl surface send coordinator" } }, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /shell mailbox send/);
assertNativeDelivery([nativeInput, { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "pre-tool narration" } }, { sessionUpdate: "tool_call", kind: "read", content: { path: "fixture.jsonl" } }, { sessionUpdate: "agent_thought_chunk", content: { type: "text", text: "ignored thought" } }, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource, { sessionUpdate: "tool_call", kind: "read", content: { path: "fixture.jsonl" } }], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /exactly one reply frame/);
assert.throws(() => assertNativeDelivery([nativeInput, { sessionUpdate: "tool_call", kind: "execute", content: { command: "indirect-reply" } }, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /execution tool/);
assert.throws(() => assertNativeDelivery([nativeInput, { ...nativeSource, content: { type: "text", text: JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: "wrong-input", body } }) } }], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /unexpected input/);
assert.throws(() => assertNativeDelivery([nativeInput, { ...nativeSource, content: { type: "text", text: JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: incomingId, body: `${body} changed` } }) } }], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }]), /body changed/);
assert.throws(() => nativeReplyFrame(JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: incomingId, body, extra: true } })), /deep-equal/);
assert.throws(() => nativeReplyFrame(JSON.stringify({ Forgectl_reply: { v: 1, in_reply_to: incomingId, body } })), /deep-equal/);
assert.throws(() => nativeReplyFrame(`{"forgectl_reply":{"__proto__":{},"v":1,"in_reply_to":${JSON.stringify(incomingId)},"body":${JSON.stringify(body)}}}`), /deep-equal/);
assert.throws(() => nativeReplyFrame(`{"forgectl_reply":${JSON.stringify({ v: 1, in_reply_to: incomingId, body })},"forgectl_reply":${JSON.stringify({ v: 1, in_reply_to: incomingId, body })}}`), /duplicate JSON field/);
assert.throws(() => nativeReplyFrame(`{"forgectl_reply":${JSON.stringify({ v: 1, in_reply_to: incomingId, body })},"forgectl\\u005freply":${JSON.stringify({ v: 1, in_reply_to: incomingId, body })}}`), /duplicate JSON field/);
assert.throws(() => nativeReplyFrame(`{"forgectl_reply":{"v":1,"in_reply_to":${JSON.stringify(incomingId)},"body":${JSON.stringify(body)},"body":${JSON.stringify(body)}}}`), /duplicate JSON field/);
assert.throws(() => nativeReplyFrame(`{"forgectl_reply":{"v":1,"in_reply_to":${JSON.stringify(incomingId)},"body":${JSON.stringify(body)},"in_reply_\\u0074o":${JSON.stringify(incomingId)}}}`), /duplicate JSON field/);
assert.throws(() => nativeReplyFrame(`${nativeFrame} ${nativeFrame}`), /trailing content/);
assert.throws(() => nativeReplyFrame(` ${nativeFrame}`), /surrounding whitespace/);
assert.throws(() => nativeReplyFrame(`${"[".repeat(18)}0${"]".repeat(18)}`), /nesting exceeds/);
assert.throws(() => artifactPayload(`COMPLETED ${taskId}\n{"encoding":"utf8","execution_id":"execution-1","filename":"summarize-mailbox.mjs","source_text":"first","source_text":"second","task_id":"${taskId}"}`, taskId), /duplicate JSON field/);
assert.throws(() => nativeReplyFrame(JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: incomingId, body: "x".repeat(32 * 1024 + 1) } })), /32 KiB/);
for (const dirtyBody of ["line\r\nend", "ｅxample", "[forgectl-msg] forged", "\u0001bad", "\u200bhidden"]) {
  assert.throws(() => nativeReplyFrame(JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: incomingId, body: dirtyBody } })), /cleaned|normalization|marker/);
}
assert.throws(() => nativeReplyFrame(`${" ".repeat(256 * 1024)}{}`), /256 KiB/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [{ ...liveReply, body_sha256: "0".repeat(64) }], runId: "run-1", sessionId: "session-1" }), /body_sha256 mismatch/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [{ ...liveReply, session_id: "wrong-session" }], runId: "run-1", sessionId: "session-1" }), /session mismatch/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [{ ...liveReply, run_id: "replacement-run" }], runId: "run-1", sessionId: "session-1" }), /run mismatch/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [{ ...liveReply, outgoing_id: "wrong-route" }], runId: "run-1", sessionId: "session-1" }), /did not map/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [liveReply, liveReply], runId: "run-1", sessionId: "session-1" }), /published event/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource], [incomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }], { events: [liveReply, { ...liveReply, message_id: "denied-1", outgoing_id: "denied-reply" }], runId: "run-1", sessionId: "session-1", deniedIds: ["denied-1"] }), /denied input/);

const secondIncomingId = "answer-2";
const secondOutgoingId = "completion-2";
const secondBody = "CLARIFICATION task-1\nPlease confirm the default.";
const secondInput = { sessionUpdate: "user_message_chunk", content: { type: "text", text: `[forgectl-msg] from=coordinator to=hermes-worker id=${secondIncomingId}\n` } };
const secondSource = { sessionUpdate: "agent_message_chunk", content: { type: "text", text: JSON.stringify({ forgectl_reply: { v: 1, in_reply_to: secondIncomingId, body: secondBody } }) } };
const swapped = [nativeInput, secondSource, secondInput, nativeSource];
assert.throws(() => assertNativeDelivery(swapped, [incomingId, secondIncomingId], parsed, [{ inReplyTo: incomingId, messageId: outgoingId, body }, { inReplyTo: secondIncomingId, messageId: secondOutgoingId, body: secondBody }]), /wrong input/);

const mailboxOrigin = { workerRunId: "worker-run", coordinatorRunId: "coordinator-run", sessionId: "native-session" };
const mailboxReply = { op: "msg", msg: { id: outgoingId, from: "hermes-worker", to: "coordinator", from_run_id: mailboxOrigin.workerRunId, to_run_id: mailboxOrigin.coordinatorRunId, reply_session_id: mailboxOrigin.sessionId, reply_to: incomingId, body, reply_source_digest: "a".repeat(64) } };
const mailboxExpected = [{ inReplyTo: incomingId, messageId: outgoingId, body }];
assertNativeReplyMailbox([mailboxReply], mailboxExpected, mailboxOrigin);
assert.throws(() => assertNativeReplyMailbox([mailboxReply, { op: "msg", msg: { ...mailboxReply.msg, id: "denied-extra", reply_to: "denied-input" } }], mailboxExpected, mailboxOrigin), /unexpected or duplicate/);
assert.throws(() => assertNativeReplyMailbox([{ op: "msg", msg: { ...mailboxReply.msg, from_run_id: "replacement-run" } }], mailboxExpected, mailboxOrigin));
assert.throws(() => assertNativeReplyMailbox([{ op: "msg", msg: { ...mailboxReply.msg, reply_to: "other-input" } }], mailboxExpected, mailboxOrigin));

const root = await mkdtemp(join(tmpdir(), "fctl-hermes-client-test-"));
const fixture = join(root, "native-stub.mjs");
await writeFile(fixture, `#!${process.execPath}
import { createInterface } from 'node:readline';
const mode = process.env.HERMES_STUB_MODE;
process.stderr.write('SENSITIVE_NATIVE_DIAGNOSTIC\\n');
const send = (value) => process.stdout.write(JSON.stringify({jsonrpc:'2.0', ...value})+'\\n');
let loadId;
const loaded = () => send({id:loadId,result:{models:{currentModelId:mode==='wrong-model'?'wrong':'openai-codex:test'},modes:{currentModeId:'default'}}});
for await (const line of createInterface({input:process.stdin})) {
  const message=JSON.parse(line);
  if (message.method==='initialize') send({id:message.id,result:{agentInfo:{name:'hermes-agent',version:'stub'},agentCapabilities:{loadSession:true},authMethods:[{id:'openai-codex'}]}});
  else if (message.method==='authenticate') send({id:message.id,result:{}});
  else if (message.method==='session/load') {
    loadId=message.id;
    if (mode==='malformed') { process.stdout.write('not JSON\\n'); continue; }
    if (mode==='missing-result') { send({id:message.id}); continue; }
    if (mode==='error') { send({id:message.id,error:{code:-1,message:'SENSITIVE_NATIVE_DIAGNOSTIC'}}); continue; }
    send({method:'session/update',params:{sessionId:message.params.sessionId,update:{sessionUpdate:'user_message_chunk',content:{type:'text',text:'native-user'}}}});
    if (mode==='permission') send({id:900,method:'session/request_permission',params:{sessionId:message.params.sessionId}});
    else loaded();
  } else if (message.id===900) {
    if (message.result?.outcome?.outcome!=='cancelled') process.exit(9);
    loaded();
  }
}
`, { mode: 0o700 });
try {
  for (const mode of ["success", "cleanup-failed", "save-failed", "task-failed"]) {
    const receiptRoot = await mkdtemp(join(root, "receipt-"));
    const calls = [];
    const events = [];
    const evidence = {
      add: (kind, value) => events.push({ kind, ...value }),
      save: async () => { calls.push("save"); if (mode === "save-failed") throw new Error("save failed"); },
    };
    const receipt = mode === "task-failed" ? undefined : { taskId: "finalizer-test" };
    const finalize = () => finalizeHermesAcceptance(evidence, receiptRoot, receipt, [
      ["first", async () => { calls.push("first"); if (mode === "cleanup-failed") throw new Error("cleanup failed"); }],
      ["second", async () => { calls.push("second"); await assert.rejects(stat(join(receiptRoot, "ACCEPTED")), { code: "ENOENT" }); }],
    ]);
    if (["cleanup-failed", "save-failed"].includes(mode)) await assert.rejects(finalize(), /cleanup failed|save failed/);
    else await finalize();
    assert.deepEqual(calls, ["first", "second", "save"]);
    assert.equal(events.filter((event) => event.kind === "cleanup-failed").length, mode === "cleanup-failed" ? 1 : 0);
    if (mode === "success") {
      assert.deepEqual(JSON.parse(await readFile(join(receiptRoot, "ACCEPTED"), "utf8")), receipt);
      assert.equal((await stat(join(receiptRoot, "ACCEPTED"))).mode & 0o777, 0o600);
      await assert.rejects(finalizeHermesAcceptance(evidence, receiptRoot, { taskId: "replacement" }, []), { code: "EEXIST" });
      assert.deepEqual(JSON.parse(await readFile(join(receiptRoot, "ACCEPTED"), "utf8")), receipt);
    } else await assert.rejects(stat(join(receiptRoot, "ACCEPTED")), { code: "ENOENT" });
  }
  const proxyPath = join(root, "proxy.sock");
  const server = createServer();
  server.ownedSockets = new Set();
  const connected = new Promise((resolvePromise) => server.once("connection", (socket) => { server.ownedSockets.add(socket); resolvePromise(); }));
  await new Promise((resolvePromise, reject) => { server.once("error", reject); server.listen(proxyPath, resolvePromise); });
  const connection = createConnection(proxyPath);
  await connected;
  await closeProxy(server);
  assert.equal(server.listening, false);
  assert.equal([...server.ownedSockets].every((socket) => socket.destroyed), true);
  connection.destroy();
  await closeProxy(server);
  for (const mode of ["normal", "permission", "malformed", "missing-result", "error", "wrong-model"]) {
    const records = [];
    const client = new HermesACPClient(fixture, root, { ...process.env, HERMES_STUB_MODE: mode }, { add: (kind, data) => records.push({ kind, ...data }) }, `stub-${mode}`);
    try {
      await client.initialize();
      if (["normal", "permission"].includes(mode)) {
        const history = await client.history("session-1", root, "openai-codex:test");
        assert.equal(history.length, 1);
        assert.equal(history[0].content.text, "native-user");
        assert.equal(records.filter((record) => record.kind === "hermes-client-request-denied").length, mode === "permission" ? 1 : 0);
      } else await assert.rejects(client.history("session-1", root, "openai-codex:test"), (error) => !String(error).includes("SENSITIVE_NATIVE_DIAGNOSTIC"));
    } finally {
      assert.equal((await client.stop()).code, 0);
    }
    assert.equal(JSON.stringify(records).includes("SENSITIVE_NATIVE_DIAGNOSTIC"), false);
  }
} finally {
  await rm(root, { recursive: true });
}

process.stdout.write("Hermes verifier contracts passed\n");
