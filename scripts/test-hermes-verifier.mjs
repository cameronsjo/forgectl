#!/usr/bin/env node

import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createConnection, createServer } from "node:net";
import { artifactPayload, assertNativeDelivery, closeProxy, parseHermesArgs } from "./verify-hermes-collaboration.mjs";
import { HermesACPClient } from "./hermes-acp-client.mjs";

const flags = ["--live", "--root", "/private/test", "--forgectl", "/bin/forgectl", "--codex", "/bin/codex", "--codex-model", "coordinator-model", "--hermes", "/bin/hermes-acp", "--hermes-python", "/bin/python", "--hermes-home", "/private/hermes", "--hermes-model", "worker-model", "--websocket-module", "/private/ws.mjs"];
assert.equal(parseHermesArgs(flags)["hermes-model"], "worker-model");
assert.throws(() => parseHermesArgs(flags.slice(1)), /requires --live/);
assert.throws(() => parseHermesArgs([...flags, "--hermes-model", "other-provider:model"]), /unqualified/);
assert.throws(() => parseHermesArgs([...flags, "--hermes-python", "python"]), /must be absolute/);
assert.throws(() => parseHermesArgs([...flags, "--unknown"]), /unknown argument/);

const taskId = "task-1";
const payload = { encoding: "base64", filename: "summarize-mailbox.mjs", source_base64: Buffer.from("process.stdout.write('example');\n").toString("base64"), task_id: taskId, execution_id: "execution-1" };
const body = `COMPLETED ${taskId}\n${JSON.stringify(payload)}`;
const parsed = artifactPayload(body, taskId);
assert.equal(parsed.source.toString("utf8"), "process.stdout.write('example');\n");
assert.throws(() => artifactPayload(body, "different-task"), /exact task marker/);
for (const fields of [
  { filename: "../summarize-mailbox.mjs" },
  { encoding: "utf8" },
  { source_base64: payload.source_base64.slice(0, -1) },
  { source_base64: Buffer.alloc(12_001).toString("base64") },
  { source_base64: Buffer.from([255, 254]).toString("base64") },
  { execution_id: "" },
  { unexpected: true },
]) assert.throws(() => artifactPayload(`COMPLETED ${taskId}\n${JSON.stringify({ ...payload, ...fields })}`, taskId));

const nativeInput = { sessionUpdate: "user_message_chunk", content: { type: "text", text: "[forgectl-msg] from=coordinator to=hermes-worker id=message-1\n" } };
const nativeSource = { sessionUpdate: "agent_message_chunk", content: { type: "text", text: body } };
assertNativeDelivery([nativeInput, nativeSource], ["message-1"], parsed, "/bin/forgectl");
assert.throws(() => assertNativeDelivery([nativeSource], ["message-1"], parsed, "/bin/forgectl"), /exactly one user header/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeInput, nativeSource], ["message-1"], parsed, "/bin/forgectl"), /exactly one user header/);
for (const text of ["quoted id=message-1", nativeInput.content.text.replace("message-1", "message-10"), `quoted ${nativeInput.content.text}`, nativeInput.content.text.replace("coordinator", "other")]) {
  assert.throws(() => assertNativeDelivery([{ ...nativeInput, content: { type: "text", text } }, nativeSource], ["message-1"], parsed, "/bin/forgectl"), /exactly one user header/);
}
assert.throws(() => assertNativeDelivery([nativeInput], ["message-1"], parsed, "/bin/forgectl"), /exact worker-authored payload/);
assert.throws(() => assertNativeDelivery([nativeInput, { ...nativeSource, sessionUpdate: "user_message_chunk" }], ["message-1"], parsed, "/bin/forgectl"), /exact worker-authored payload/);
assert.throws(() => assertNativeDelivery([nativeInput, nativeSource, nativeSource], ["message-1"], parsed), /exactly one exact worker-authored payload/);
assert.throws(() => assertNativeDelivery([nativeInput, { sessionUpdate: "tool_call", kind: "execute", content: { ...payload, command: "/bin/forgectl surface send coordinator" } }], ["message-1"], parsed), /exact worker-authored payload/);

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
