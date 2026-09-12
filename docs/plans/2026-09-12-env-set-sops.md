---
status: complete
branch: feat/env-set-sops
base_branch: fix/any-file-confirm
pr: forgectl#517
base_pr: forgectl#515
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

**The macOS runner installs sops from the release asset, not Homebrew.** The
plan said "beside the `tmux` installs", and `brew install sops` fails outright
on the self-hosted runner: its Homebrew prefix is owned by another user, so the
step dies with `/opt/homebrew` not writable. The neighbouring tmux step only
survives because tmux is already in the image and its `brew list`
short-circuits — nothing absent is installable through brew there. Both
binaries now install from checksum-verified release assets into a runner-owned
directory added to `PATH`, matching the ubuntu job.

**`openatCreate` retries a spurious `ENOENT`.** Not in the plan, and not
optional: `unix.Openat` with `O_CREAT` on darwin returned 582 spurious `ENOENT`
in 800 concurrent attempts where `os.OpenFile` with identical flags returned
800/800. Landed with forgectl#515.

## Review round — three Criticals, all reproduced

A two-arm Opus review (security, correctness) over the finished branch found
three Critical defects. None was a design mistake; all three were the same
shape of error — a check that looked right and could not go red on the case it
existed for.

**The encryption-rule check tested only the leaf.** sops applies
`unencrypted_suffix` and friends to a key AND ITS WHOLE SUBTREE, so a path
whose *parent* carried `_unencrypted` passed the check and the secret landed in
plaintext with the command reporting success — reproduced end to end, the exact
failure the check was built to prevent. `WouldStoreCleartext` now takes
`[]string` and walks every segment with sops' real precedence; the signature
change is what stops the leaf-only call being written again. The same bug ran
backwards too: an ancestor-scoped `encrypted_regex` falsely refused every key
beneath the block it matched, which made the feature unusable on such a file.

**The encrypted-at-path assertion was document-order dependent.** It scanned
for the first line whose trimmed text began with `leaf + ":"`, anywhere in the
document, so any same-named encrypted key elsewhere satisfied it — including
sops' own `mac`. Proven by reordering one write: identical input passed with
the secret in plaintext, or correctly went red, depending only on which line
came first. So the check the design calls "the one that matters most" was the
one that could not be made to go red on demand. It resolves the path through
`yaml.v3` now.

**A bare prefix match destroyed a colon-bearing sibling.** `a:b: 'v'` is valid
YAML and decodes to the key `a:b`; setting `a` matched that line, and since the
replace cuts at the first colon the result was `a: 'new'` — another key and its
encrypted value gone, reported as `replaced a`, passing every downstream check
because extracting the path then returns exactly what was supplied. `findLeaf`
now requires a space or end-of-line after the colon.

**Also folded in:** a leaf naming a block now refuses by name rather than
emitting YAML the child rejects; the staged plaintext and the decrypted
read-back are deleted the moment they are consumed, shrinking the window in
which a Ctrl-C could leave a committable secret in the work tree; `__sops-edit`
refuses a symlinked or non-mapping target, closing the one write in forgectl
that had no containment at all; the sops output capture happens only on the
path that reports it, rather than orphaning a file in `$TMPDIR` on every
successful run; both sops calls pass `--disable-version-check`; the protocol's
environment-variable names are now shared constants rather than literals
spelled in two packages; CI verifies the `sops` and `age` download checksums
before installing them; and `docs/commands/env.md` gained the `--sops`
reference plus a note that the flag widens the authority `env set` grants.

**Three comments were corrected rather than deleted**, each having claimed a
control the code did not have: `readOutcome`'s stated reason for its default
was factually wrong about which path reaches it, `ReplaceSopsNonce` still
described the nonce as a privilege boundary and contradicted the two artifacts
that correctly do not, and the editor's write claimed a mode restatement
prevented a umask from widening a file it cannot affect.

**Came back clean and worth recording:** the unbounded-loop defence held
against every value the reviewer could find, including the Unicode line breaks
`U+0085`/`U+2028`/`U+2029` that `NormalizeValue` permits — `U+0085` corrupted
the round-trip and the byte-exact comparison caught it, which is the check that
the declined trailing-newline strip would have masked. And `SetScalar`'s
refusal list turns out to be largely unreachable through the driver, because
the document it sees is sops' own yaml.v3 re-emission: tabs, CRLF, flow
mappings, anchors, and multi-document streams are all normalised away before
the line model sees them. The refusals stay as a contract on the function.

## Two review findings from forgectl#515, fixed here

CodeRabbit raised three findings on the base PR. One was already fixed there
(`dc8b7ef`); the other two land on this branch, because this branch contains
the base and the write path this feature adds depends on exactly that lock
correctness.

**`openLock` did not validate the descriptor it returned.** `withFileLock`
Lstats the lock name and then opens it, and `O_NOFOLLOW` closes that window for
a symlink ONLY — a swap to a FIFO inside the same window is not a symlink, so
nothing caught it. Since flock locks an open file description, two writers on
two FIFO inodes would both believe they held the lock, and the parse→write
section that exists to prevent a lost update would stop preventing one.
`openLock` now does the same post-open regular-file check `openRegular`
already did. The doc comment claiming the Lstat refused a FIFO was corrected
rather than deleted — the fourth instance of this repo's signature defect, a
comment asserting a control the code did not have.

Verified with a negative control: with the check reverted, the new `fifo`
subtest fails and the `symlink` subtest still passes, which is what proves the
new check is what adds the FIFO refusal rather than duplicating `O_NOFOLLOW`.

**Four refusal branches abandoned an open directory descriptor.** A `Target`
owns a dirfd, and `resolveEnvTarget` returned `Target{}` on four refusal paths
without closing it, so a long-lived process refusing repeatedly retained one
descriptor per attempt. Every refusal now routes through one closure, which is
what keeps the next branch added there from leaking — a caller-side `defer`
gives no signal when a return is missed. The test fixture and the one direct
`ResolveTarget` test close theirs too.

## Bot review round — four findings, all fixed

**The block refusal echoed the leaf segment.** `SetScalar`'s
names-a-block refusal carried the segment in `%q`, and that message is relayed
out of the child process to the operator's terminal. `ParsePath`'s grammar
admits plenty of provider token formats as one valid segment, so a secret
pasted into the key slot arrived as the leaf and was printed — a direct
violation of this plan's own refusal rule. The message now names only the rule.
The test asserts no refusal echoes the leaf, with a negative control proving
the assertion goes red when it does.

The ancestor segments still name the block they could not find, and the
asymmetry is deliberate: a mistyped block name is the commonest mistake on this
path and the only actionable thing the message can carry, and an ancestor is not
the paste site — a bare pasted secret is a single-segment path, whose whole walk
is the leaf.

That first assertion was itself over-broad and had to be fixed before it meant
anything: the sequence refusal reads "only scalar keys in a mapping", and the
fixture's leaf was named `key`, so a substring match caught an English word
rather than an echo. The refusal fixtures now use an unmistakable leaf.

**`openLock` gained `O_NONBLOCK`.** The comment justified its absence from a
darwin measurement, but POSIX leaves `O_RDWR` on a FIFO undefined and permits a
blocking open for a character device that supports non-blocking mode — either
of which would stall before the regular-file check ran. The flag has no effect
on regular-file I/O, which is the only case that reaches the return.

**Two doc corrections.** The command reference listed a value's rejected bytes
as "a C0 control byte" when `NormalizeValue` accepts tab, and the README's
target allowlist omitted every `.yml` form the code accepts.

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
