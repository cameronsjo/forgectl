## 1. Brief

- [x] 1.1 `hcadapter.Adapter.SendBrief` takes the checked worker, re-reads it, and refuses a changed turn or a status other than idle or done; `--if idle` follows the re-read status.
- [x] 1.2 `surface brief` passes the checked state; tests for each refusal and for the checked state reaching the send.
- [x] 1.3 Escape spacing: wait out one second after the last typed brief; hold one second after an Escape of unknown delivery.

## 2. Forget

- [x] 2.1 `hcadapter.Adapter.Abandoned`: a reason for a wiped core, an absent terminal, or a reused address; refusals otherwise.
- [x] 2.2 `surface forget <name> --surface hc [--json]`: row refusals, hc refusal, row-still-the-same removal, exit 0/1/2.

## 3. Ledger posture

- [x] 3.1 `worker.Row.Posture` written with the harness; `surface list --json` prints it.

## 4. herdr-app

- [x] 4.1 `--herdr-space <id>` on hc worker launches labels the terminal `herdr.space=<id>`; refused for herdr workers and non-worker launches.

## 5. Docs

- [x] 5.1 `docs/herdr.md` hc section.
