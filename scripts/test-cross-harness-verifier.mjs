#!/usr/bin/env node

import assert from "node:assert/strict";
import { assertRoundtripOrder, parseArgs } from "./verify-cross-harness-loop.mjs";

const flags = ["--live", "--forgectl", "/test/forgectl", "--root", "/test/root", "--websocket-module", "/test/ws.mjs"];
assert.throws(() => parseArgs(flags), /--codex-model is required/);
const selected = parseArgs([...flags, "--codex-model", "coordinator-model", "--pi-provider", "worker-provider", "--pi-model", "worker-model"]);
assert.equal(selected["codex-model"], "coordinator-model");
assert.equal(selected.websocketModule, "/test/ws.mjs");
assert.equal(selected["pi-provider"], "worker-provider");
assert.equal(selected["pi-model"], "worker-model");
assert.throws(() => parseArgs([...flags, "--codex-model", "coordinator-model", "--pi-provider", "worker-provider"]), /must be supplied together/);
assert.throws(() => parseArgs(flags.slice(1)), /without explicit --live/);

const messages = ["delegate", "clarify", "answer", "complete"];
assert.deepEqual(assertRoundtripOrder(messages, messages), [0, 1, 2, 3]);
assert.deepEqual(assertRoundtripOrder(["other", ...messages], messages), [1, 2, 3, 4]);
assert.throws(() => assertRoundtripOrder(["delegate", "answer", "clarify", "complete"], messages), /causal mailbox order/);
assert.throws(() => assertRoundtripOrder(["delegate", "clarify", "complete", "answer"], messages), /causal mailbox order/);
assert.throws(() => assertRoundtripOrder(messages.slice(0, 3), messages), /causal mailbox order/);
assert.throws(() => assertRoundtripOrder(messages, ["delegate", "clarify", "answer", "answer"]), /causal mailbox order/);

process.stdout.write("cross-harness verifier contract tests passed\n");
