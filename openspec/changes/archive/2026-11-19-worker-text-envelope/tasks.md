## 1. Envelope

- [x] 1.1 Add `worker.Envelope`, `worker.Sender`, `worker.HcWraps`, `worker.TypedBrief` and `worker.HcTypedBrief`, with tag neutralizing for `forgectl` and `hc`
- [x] 1.2 Wrap the launch prompt, provenance note included, in `buildWorkerInvocation`
- [x] 1.3 Wrap the herdr typed brief; on hc, let hc wrap when forgectl runs in an hc terminal and wrap it otherwise
- [x] 1.4 Keep `HC_TERMINAL_ID` and `HC_SCOPE` for the two text-carrying `hc send` calls only
- [x] 1.5 Count the envelope against the typed cap and name the room left in the refusal

## 2. Rule text

- [x] 2.1 Open the launch brief with the assignment statement
- [x] 2.2 Rewrite the approve-and-merge rule with the operator carve-out; keep the marker and board bans verbatim

## 3. Tests and docs

- [x] 3.1 Tests per clause, per path and per branch, each with a staged break
- [x] 3.2 `docs/herdr.md`: the envelope, the paths that wrap, the rule
