---
status: complete
branch: feat/env-set-sops
pr: forgectl#516
issue: forgectl#498
approved_in: session
approved_session_id: 2d4b9aa6-61b9-4645-8591-016fae184f38
---

# `env set --sops` — write one key into a SOPS file without exposing the value

## Goal

`forgectl env set agentgateway.llm_key_hermes --sops` writes one key into
`secrets.sops.yaml` from stdin, a no-echo prompt, or the clipboard; untouched
values keep byte-identical ciphertext and key order; and the write is proven to
have landed **encrypted** before success is reported.

Closes forgectl#498.

## Context

`forgectl env set KEY` already solves "get a secret into a file without it
touching argv, a terminal, or a transcript" for `.env`. SOPS-encrypted YAML has
the identical problem and had no equivalent, so every estate secret write was
hand-managed.

Both obvious workarounds are wrong. `sops set file '["a"]["b"]' '"value"'` puts
the plaintext in argv — visible in `ps`, left in shell history. `sops file`
drops you into vim to paste one line into a 200-line encrypted document.

## Decisions taken

| Decision | Choice | Why |
|---|---|---|
| Command shape | `--sops` flag on the existing `env set` | One verb; no new module manifest |
| Path depth | Arbitrary, dotted (`a.b.c.d`) | The general case costs little once the walk is recursive |
| Dots inside a key name | Refused | A dotted string cannot disambiguate `{a, b.c}` from `{a, b, c}` |
| SOPS driver | Shell out to the `sops` binary | No vendored cloud-KMS SDK tree, no coupling to SOPS' internal API |
| Target gate | Name allowlist **and** content check, no escape hatch | Operator's call; see § Deviations |
| `--sops` with `--any-file` | Refuse | Operator's call: the flag would imply a bypass that does not exist |

## Alternatives declined

- **A separate `forgectl secret` command group.** Cleaner separation, but
  Cameron chose one verb. The code paths stay separate underneath.
- **`getsops/sops` as a Go library.** No subprocess and richer errors, at the
  cost of a large transitive cloud-KMS tree — and the library path inherits
  SOPS' YAML marshalling, which is the whole-file-reflow problem this change
  exists to avoid.
- **`sops set`.** Puts the plaintext in argv, the exposure #498 was filed about.
- **Backslash-escaped dotted keys.** An escaping surface on every path for a
  case no estate secrets file has.
- **Reusing `env.writeAtomic` for the restore.** Its `.env-*.tmp` name and
  forced 0600 are `.env` semantics.

## Panel

Panel: plan-reviewer, security-posture-reviewer, red-team-reviewer ran — 31
findings, 29 folded in, 2 declined.

### Findings declined

- **`govulncheck ./...` in CI** — worth doing, unrelated to this path. Filed as
  forgectl#514.
- **A `cadence-hooks` issue** for its sops-decrypt guard's documented blind
  spot — correct, but a change to another repo's docs.

## Measured ground truth

Every number here was measured on sops 3.13.3 / darwin 25.5, not read from
documentation.

| Question | Measured |
|---|---|
| `EDITOR` handling | Shell-word split (quotes honoured), temp path appended as the only argument; self-exec works |
| `sops -d --extract --output` trailing newline | **None.** A 9-byte value writes a 9-byte file — so the read-back compares raw bytes with no strip |
| Editor leaves the temp unchanged | Exits **200**, `File has not changed, exiting.`, file untouched, returns immediately |
| Editor exits non-zero | Exits 201, clean failure, encrypted file byte-identical |
| Editor writes invalid YAML | sops re-invokes the editor **without bound**: 36,851 invocations and 8.4 MB of stderr in ~3 minutes, still going when killed |
| Diff size, replace | 3 changed lines: the content line plus sops' `lastmodified` and `mac` |
| Diff size, add | 2 insertions / 1 deletion |
| `.sops.yaml` discovery | Walks up from the **working directory**, not the input file's path |
| `unencrypted_suffix` | A matching key is written in **plaintext** beside `ENC[AES256_GCM,...]` siblings |

## Shipped shape

- **`internal/sops`** — the pure half (`path.go`, `value.go`, `edit.go`,
  `file.go`) makes every decision and touches nothing; `driver.go` runs the
  binary and the filesystem around those decisions.
- **`internal/cli/sops_edit.go`** — the hidden `__sops-edit` subcommand sops
  invokes as its `EDITOR`, so the value travels by file and the key path by
  environment. Neither is ever an argument to any process.
- **`internal/cli/env.go`** — the `--sops` flag, the branching key gate, the
  repo-root default, the target gate, and the clipboard route.
- **`internal/exec/sensitive.go`** — two sops `CommandKind`s and four
  `EnvMutation` constructors.
- **`internal/doctor`** — a `sops` check reporting the **version**.

## Deviations

**The execution seam is `exec.SensitiveRunner`, not `exec.Runner` as planned.**
The plan's own step 11 required capturing sops' output to a file, which
`exec.Runner` cannot do — its methods capture into strings. Worse, `runAndWrap`
logs child stderr at `Error` level, which survives any configured log level and
can be pointed at a file on disk, and retains it on a `*CommandError` fang
renders. A sops YAML parse error quotes the offending line, and that line is
`key: '<the secret>'`. `SensitiveRunner` already existed for exactly this and
can render neither, and its 64 KiB stream cap is a second brake on the measured
8.4 MB stderr case.

**The target gate refuses rather than confirms, and that reordered the whole
plan.** The plan routed a non-standard target through `resolveAllowAnyFile`.
The step-0 security review found that function carried a demonstrated RCE — the
confirmed path and the written path could be two different files — and that its
TTY probe reads stdin, so `--any-file` refuses whenever a value is piped,
making the plan's own `printf … | forgectl env set --sops` example impossible.
Cameron chose to refuse a non-standard name outright. The fix landed first, as
forgectl#515, and this branch is stacked on it.

**Three env vars, not five.** The work directory is named once and the value,
nonce, result, counter, and error files sit at fixed names inside it. Every
variable is a name an attacker could try to set.

**The nonce is not described as a privilege boundary, because it is not one.**
The plan claimed it stopped `__sops-edit` being "a bare arbitrary-YAML-write
primitive". It does not: a caller who can set the environment can also create
the directory and nonce file it names — and a caller who can exec forgectl can
already write YAML with a shell, so the subcommand grants no capability its
invoker lacked. What the nonce and the work-directory name constraint do bound
is a **stray or replayed** invocation. The output validation and the once-only
counter are the load-bearing guards.

**An error relay was added.** The editor's refusals are the actionable ones — a
typo'd block name is the commonest mistake — and they live only in the child,
whose stderr is sops' stderr and therefore unsurfaceable. The child writes its
message to a file in the work directory and the driver surfaces that instead.
Safe because every relayed message originates in forgectl and names a rule, a
property the package's tests assert.

**The name check precedes the existence check.** A refused name should refuse
on the name whatever the filesystem says, and answering existence first turns a
refused path into an existence oracle.

**`env.RepoRoot` came back.** forgectl#515 deleted it as dead; the `--sops`
default lives at the repository root, so it now has a caller.

**`openatCreate` retries a spurious `ENOENT`.** Not in the plan, and not
optional: `unix.Openat` with `O_CREAT` on darwin returned 582 spurious `ENOENT`
in 800 concurrent attempts where `os.OpenFile` with identical flags returned
800/800. Landed with forgectl#515.

## Out of scope

- Reading or listing SOPS values (`env get --sops`, `env keys --sops`).
- Creating a missing block or a missing file.
- Non-scalar values (sequences, maps, multi-line strings).
- Key rotation, recipient management, `updatekeys`.
- A SOPS file under a name outside the allowlist.

## Verification

```bash
gofmt -l .                                    # clean
go vet ./...                                  # clean
go test ./... -count=1                        # all packages pass
golangci-lint run                             # 0 issues
FORGECTL_REQUIRE_SOPS_INTEGRATION=1 go test -run Integration ./internal/sops
```

End to end against the built binary, in a scratch repo with an age identity:
add from piped stdin, byte-exact round-trip, zero plaintext occurrences, an
idempotent re-set reporting success, a rotation reporting `replaced`, three
refusals each leaving the file byte-identical, and no work directory left
behind.

The integration gate was verified in both directions: with `sops` off PATH the
tests skip by default and **fail** under
`FORGECTL_REQUIRE_SOPS_INTEGRATION=1`.
