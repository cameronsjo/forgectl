# forgectl `env` — safe .env management (forgectl#82)

**Panel: 4 seats ran (plan-reviewer ×2, security-posture Opus, cameron-review) — 17 substantive findings, 15 folded in, 2 resolved by trimming the flagged surface. See § Panel review.**

## Context

Agent-driven workflows constantly touch `.env` files, and today that means secrets landing in terminal output and session transcripts, or a human doing it by hand. forgectl#82 (open, unstarted) specs an `env` command group that makes `.env` management **safe to delegate to an agent**: key names visible, values never. Cameron needs it now ("immediate ask but I want it right" — speed and need set priority, not quality), scoped this session to: **full file surface, zero secret-manager coupling** — plain `.env` family kept simple, 1Password playing nicely *by composition* (`op read op://… | forgectl env set KEY`), never by dependency.

forgectl has no `.env` code today (verified). Greenfield Extension-tier module alongside the existing **16** command groups on `origin/main` — pin goes **16→17**.

> **Corrected during execution (2026-07-16).** This plan originally said 17 groups / pin 17→18, and three panel seats independently "CONFIRMED" it. All three read the forgectl **main checkout**, which is parked on the unmerged `feat/docs-serve-pr1` branch (it carries a `docsModule`); `origin/main` has 16 and `wantCount = 16`. Agreement across seats is not independence when they share a working tree. Verify live-state claims with `git show origin/main:<path>`, never the checkout.

**Security-sensitive:** secrets-handling surface. Security seat ran at plan review (floor seat); a **control-level security review gates the merge** (§ Security gate). Security review work routes to Opus.

## Command surface

```
forgectl env keys [--file .env]                        list KEY names only — never values
forgectl env set KEY [--file .env] [--clipboard]       value from piped stdin, no-echo prompt, or clipboard — never argv
forgectl env get KEY --clipboard [--file .env]         value to clipboard only; no print path exists
forgectl env check [--file .env] [--example .env.example]   missing/extra keys, names only; exit 1 iff missing
forgectl env redact [--file .env]                      print file with values masked ****
```

Behavior spec (complete — implementer resolves nothing else against intuition):

- **`keys`**: valid KEY names, one per line, first-seen order, deduped. Malformed lines skipped with `skipped N malformed line(s)` to stderr. Zero-pair file → empty stdout, exit 0. Missing file → error, nonzero.
- **`set KEY`**: `Args: ExactArgs(1)`. `ValidKey` first — refuse before touching the file or reading input. Source selection: `--clipboard` given → clipboard (wins even if stdin is also piped — deterministic, no exclusion error); else stdin not a TTY → read all of stdin; else interactive **no-echo prompt** (`Value for KEY: ` via `term.ReadPassword`). Strip exactly one trailing `\n` and a preceding `\r` from stdin/clipboard input (Windows-clipboard paste leaves no stray `\r`); prompt path returns no newline; interior whitespace never trimmed. **Empty value (zero bytes after strip) → refuse** (`empty value; refusing to set KEY to empty — edit the file directly if intended`). Duplicate key in file → **refuse**, naming lines (`duplicate key "KEY" at lines N,M; resolve manually`). Success prints `set KEY in <file>` (+ tighten note) — never the value.
- **`get KEY --clipboard`**: `--clipboard` **required**; absent → error, copy nothing, print nothing. Present → value to clipboard; stdout gets only `copied KEY to clipboard`. Missing key → error naming the key only. **No value-print code path exists** — the "refuses TTY/pipe" property is structural.
- **`check`**: compare key sets of `--file` vs `--example` (default `.env.example`), names only, sorted output under `missing:` / `extra:` headers. **Exit 1 iff any missing key; extra keys are reported but exit 0** (local-only secrets are benign). Missing `--example` file → its own clear error (`example file <path> not found`), nonzero, distinct from drift. Missing `--file` → same treatment. No `--strict`/`--missing-only` flags in v1 (declined — YAGNI; see Alternatives).
- **`redact`**: masked file to stdout: every pair renders `KEY=****` (or `export KEY=****`) — **fixed 4-char mask, quotes dropped, no length hint**. Inline trailing comment preserved only when the value was quoted (unambiguous boundary); after an unquoted value, `#…` is part of the value → masked with it. Malformed assignment-shaped lines mask everything after the first `=` (lenient masking — a malformed key can't leak a value). Comments/blanks/order verbatim; EOL style + missing trailing newline reproduced. Missing file → error.
- **Refusal-message discipline (all commands):** an error must never echo the rejected *argument* — `env set KEY=VALUE` (typo'd value inside the key arg) and `env get VALUE`-shaped mistakes would otherwise leak via stderr. Errors name the *rule* (`key must match [A-Za-z_][A-Za-z0-9_]*; values are piped or --clipboard, never argv`), the *key name* (only when it validated), and *paths* — never the offending token, never a value.

The `set` long help documents the blessed patterns, non-inline producers first:

```
op read op://vault/item/field | forgectl env set API_KEY   # 1Password by composition
forgectl env set API_KEY < value.txt                       # from a file
forgectl env set API_KEY --clipboard                       # from the clipboard
forgectl env set API_KEY                                   # interactive, no echo
```
…with an explicit warning against inlining the secret in the producing command (`printf 'secret' | …` puts it in that command's argv/transcript — the tool can't close the producer's channel).

## Architecture (ADR-0005 module pattern)

Extension-tier module `env`, mirroring `y` (client-injection seam) and `clean` (filesystem safety):

- `internal/env/` — domain package, no cobra. Takes `*clip.Client` (NOT `exec.Runner` — env shells nothing itself; the clipboard is its only non-filesystem effect). **Value-bearing operations live inside the domain** (`CopyValue`, `SetFromClipboard`) so plaintext never crosses the domain→CLI boundary — structural, not review-enforced.
- `internal/cli/env.go` — `envModule` manifest (`Tier: Extension`, `ConfigKey: ""` — stateless), `newEnvCmd(deps)` + `newEnvCmdForClient(client)` test seam, five subcommands, package-level seam vars `isTerminal`/`readPassword` (overridable in tests).
- Register in `internal/cli/modules.go` `allModules()`; pins in `modules_test.go`: `wantCount` **16→17** (against `origin/main` — see the Context correction), add `"env"` to `wantNames`, NOT in `wantCore`. If this branch is later rebased onto a main that has absorbed `feat/docs-serve-pr1`, the pin needs re-bumping.

### Document model (`internal/env/document.go`) — hand-rolled, line-based, parse-to-fields

```go
type Kind int // KindBlank | KindComment | KindPair | KindMalformed
type Line struct {
    Kind   Kind
    Raw    []string // verbatim source line(s) — >1 entry when a quoted value spans lines
    Export bool; Key string; Value string // decoded logical value (may contain newlines)
    Quote  byte     // 0 bare, '"', '\''
    Inline string   // trailing inline comment incl. leading spaces (quoted values only)
    dirty  bool     // set by Set → re-render from fields; otherwise emit Raw verbatim
}
type Document struct { Lines []Line; crlf bool; finalNL bool }

func Parse(r io.Reader) (*Document, error)
func (d *Document) Bytes() []byte                 // untouched lines byte-verbatim; dirty re-rendered
func (d *Document) Keys() []string                // valid keys, first-seen order, deduped
func (d *Document) Get(key string) (string, bool) // last-wins
func (d *Document) Set(key, value string) error   // in-place if unique; append if absent; ERROR if duplicate
func (d *Document) Redacted() []byte              // per § redact spec; reuses crlf/finalNL reconstruction
func ValidKey(key string) bool                    // ^[A-Za-z_][A-Za-z0-9_]*$
func Diff(file, example *Document) (missing, extra []string)
```

- Untouched lines are emitted from `Raw` byte-for-byte (offsets model declined — parse-to-fields + dirty-flag re-render is the simpler mechanism with the same round-trip guarantee; the only normalization ever applied is to the single line `Set` touched).
- **Multiline quoted values are first-class** (security HIGH fix): `Parse` tracks open-quote state — a quoted value containing literal newlines (PEM keys, certs, JSON) becomes ONE `KindPair` whose `Raw` holds all its source lines and whose `Value` is the decoded multi-line string. `Redacted` emits a single `KEY=****` for the whole region (continuation lines never print). `Get` returns the full decoded value. `Set` on such a key re-encodes to escaped single-line form (`"…\n…"`) — documented normalization.
- Assignment detection is **lenient** (`KindMalformed` for non-blank/comment/valid-pair lines) so `redact` can mask malformed assignments; `ValidKey` strictly governs display/mutate surfaces.
- CRLF detected from the first terminator, preserved; missing trailing newline preserved unless `Set` appends (append adds exactly one pair line, no spurious blank).
- **Encode** (dirty-line render): empty never reaches Encode (refused upstream); `^[A-Za-z0-9_./:+@%-]+$` → bare; else single-quote if no `'` and no newline (literal, no `$` expansion); else double-quote — **escape backslash FIRST, then** `"` `$`, newline→`\n` (order pinned; inverted order double-escapes).
- Duplicates: `Keys` dedupes; `Get` last-wins; `Set` refuses (see surface spec).

### Safety rails (`internal/env/locate.go`)

```go
func Locate(fileFlag, cwd string) (realPath string, exists bool, err error)
```

1. Absolutize `--file` against cwd.
2. Repo root: pure-Go **walk-up** for a `.git` entry (dir or file — worktrees); none → refuse (`not inside a git repository`). (Verified: no existing up-walk helper in forgectl; this part is genuinely new.)
3. `EvalSymlinks` the root; resolve the file (`EvalSymlinks` the file if it exists — follows a symlinked `.env` to its real target; else its parent dir, which must exist, + base name).
4. **Containment: call `sandbox.WithinWorkspace(root, real)`** (`internal/sandbox/sandbox.go:111-125`) — the existing, tested primitive already used by clean/quarantine/pr; do **not** reimplement `Rel`+`..` checking. (Decision: reuse over a standalone security-critical copy — one audited containment mechanism in the binary beats five; if sandbox ever changes for worktree reasons, its four existing security-relevant call sites break too, so env is not uniquely coupled.)
5. New files allowed when the parent resolves inside the repo; read commands error clearly on a missing file.

### Write discipline (`internal/env/write.go`)

`writeAtomic(realPath, data) (tightened bool, err error)`: `os.CreateTemp` same dir (0600 **at creation** — no chmod window), write, `Sync()`, close, `os.Rename`; tmp removed on any error. Pre-existing looser perms → land 0600, return `tightened` → CLI prints `tightened <file> to 0600` on stderr. Errors carry paths only. (Hardlink write-escape is neutralized by rename — fresh inode; stated in security notes.)

### Logging discipline

No value ever passed to `slog`, error strings, argv, or Runner args (`pbcopy` value rides `RunWithInput` stdin; `pbpaste` takes no args; `exec.CommandError` joins Name+Args only — verified). Per-package test helper `assertNoSecretInOutput` (small, duplicated in `internal/env` and `internal/cli` tests — no shared testutil package exists and one isn't worth inventing) captures cobra out/err **and installs `slog.SetDefault` to the captured buffer** so a stray `slog` call fails the assertion, and asserts a known sentinel value appears nowhere.

## Files

**Create:** `internal/env/{document,env,locate,write}.go` + co-located `*_test.go`; `internal/cli/env.go` + `internal/cli/env_test.go`.
**Modify:** `internal/cli/modules.go` (register), `internal/cli/modules_test.go` (pins 16→17 + `"env"`), `README.md` (`### env` block).
**Reuse verbatim (do not modify):** `internal/clip/clip.go`, `internal/exec/exec.go`, `internal/module/module.go`, **`internal/sandbox/sandbox.go` (`WithinWorkspace`)**.

## Commit sequence (one PR)

1. `env: line-based .env document model` — parse/render/keys/get/set/redact/validate/diff incl. multiline quoted values + tests.
2. `env: safety rails + atomic 0600 write` — `Locate` (walk-up + `sandbox.WithinWorkspace`), `writeAtomic` + tests.
3. `env: clipboard-backed get/set` — `CopyValue`, `SetFromClipboard` + tests.
4. `env: CLI surface + module registration` — five subcommands, TTY seam, `envModule`, pins 16→17 + CLI tests.
5. `env: docs` — README section + help text (blessed producers, anti-inline warning, residual-risk notes).

Each commit green on: `go build ./... && go vet ./... && gofmt -l . && go test ./...` (Go 1.25, no golangci).

## Test list

House style: individual named funcs, cobra harness, `FakeRunner`, `t.TempDir` with `.git` dir, absolute `--file` paths, `isTerminal`/`readPassword` seam overrides.

- **Document:** round-trip verbatim (comments/blanks/order/export/quoting/inline-comment/CRLF/no-trailing-newline); **multiline PEM fixture** (parse → one pair; `Redacted` output contains no body line; `Get` returns full value; `Set` re-encodes escaped); `Keys` dedup + malformed-excluded; `Get` decode single/double/bare, last-wins, missing; `Set` in-place preserving rest, append no-spurious-blank, **duplicate → error naming lines**, bare-vs-single-vs-double encode, **backslash+quote combined escape order**; `Redacted` constant mask no length hint, quoted-inline-comment kept, unquoted-trailer masked, malformed masked, no-trailing-newline reproduced; `ValidKey` adversarial (`FOO;rm -rf /`, spaces, `$`, backtick, unicode, leading digit, `FOO=BAR`, empty); `Diff` missing+extra.
- **Locate/write:** in-repo OK (incl. `.env.prod`), not-a-repo refused, outside refused, **symlink-escape refused**, new-file-in-repo allowed, worktree `.git`-file OK; atomic 0600 create, **tightens 0644→0600 + reports**, read-only dir errors path-only.
- **Domain clipboard:** `CopyValue` → fake pbcopy `Input == value`, missing key errors, clipboard failure surfaced (no value in error); `SetFromClipboard` pastes+writes, failure surfaced.
- **CLI:** keys names-only / skips-malformed-note / empty-file-empty-stdout / missing-file error; set from piped stdin / strips one trailing `\n` and `\r\n` / clipboard / **clipboard-wins-over-piped-stdin** / **TTY prompt via seam (no echo path)** / **empty stdin refused** / hostile argv key refused **with no argument echo** (`set KEY=VALUE` shape: assert error contains no `VALUE` substring) / duplicate refused / new file 0600; get requires `--clipboard` + prints nothing / confirmation-only / missing key / **`get VALUE`-shape refusal echoes nothing**; check clean exit 0 / missing → exit 1 / extra → exit 0 + reported / **missing example file distinct error** / `--file .env.prod --example .env.example` composes; redact masks + PEM fixture end-to-end / missing file; outside-repo refused (representative). Both packages: **`assertNoSecretInOutput` incl. slog capture** wired into every value-bearing test.

## Security gate (must precede merge — floor tasks from the security seat)

1. **Control-level security review** of the full file set (`internal/env/*.go`, `internal/cli/env.go`) by `cadence-forge:security-reviewer` on **Opus** (fallback: `/security-review`), with an explicit per-channel verdict: argv, stdout, stderr, slog/error strings, clipboard, tmpfile, redact continuation lines. Not a diff-hygiene pass.
2. README/help **residual-risk disclosure** ships in commit 5: clipboard contents are readable by every local process and persisted by clipboard managers (Raycast/Maccy/Alfred) — and how to clear; non-inline producers are the documented default; one-line agent-write threat statement (the operator is granting agents write authority over repo-contained env files; containment + 0600 + atomicity bound the blast radius).

**Accepted residual risk (named, not fixed in v1):** hardlink *read* exfil (`ln /outside/secret ./x.env` — creating the hardlink already implies access; write path is neutralized by rename); TOCTOU between Locate and write (local single-operator CLI; openat hardening is overkill). Both recorded in the README security notes.

## Execution mechanics

- Work in the **forgectl repo** (`~/Projects/claude-configurations/forgectl`) on `feat/env-module`; session is rooted in the meta-repo → **all dispatches use absolute paths** (subagents inherit cwd).
- Sonnet implementer subagent(s) execute commit-by-commit; orchestrator reviews each diff.
- **Polish gate (nested-repo gotcha):** `/polish` built-in arms read session cwd and no-op on forgectl — run value arms as diff-based reviewers: `cadence-forge:security-reviewer` (**Opus**) + `cadence:code-reviewer` (Sonnet) against a built diff. The `cadence-forge:polish` Skill invocation satisfies the pre-PR hook.
- PR: `gh pr create -R cameronsjo/forgectl`, body `Closes #82` (plain text). Verify `closingIssuesReferences`. Confirm a real CodeRabbit review per `cadence-forge:using-coderabbit` before merge.
- At approval: copy plan to `forgectl/docs/plans/2026-07-16-env-module.md` (Lane A, rides the PR); write active-plan pointer to auto memory.

## Verification

1. Full CI set green locally — evidence: output + exit 0.
2. E2E in a scratch repo: `printf 'v1' | forgectl env set X`; `keys`; clipboard `set`/`get` round-trip; `check` vs drifted example (exit 1) + missing-example error; `redact` shows `****` and — with a **multiline PEM fixture** — no body lines; `ls -l` shows 0600; symlink-escape refused; `set KEY=VALUE` typo-shape error contains no value. **Transcript-safety sweep:** no command output contains any test value.
3. `op read` pipe smoke test at Cameron's discretion — not a merge gate.
4. Security gate task 1 verdict recorded; PR CI green; CodeRabbit real review confirmed.

## Alternatives declined

- **`--ref op://…` / any in-binary secret-manager awareness** — owner declined: coupling. Composition via stdin; future `--ref` is its own issue if pipe friction shows.
- **`godotenv`** — no comment/ordering round-trip; dependency vs single-binary bias. **`4nd3r5on/go-envfile`** (structure-preserving, surveyed at owner-lens prompting) — 0 stars, single maintainer, unvetted for a secrets parser; declined.
- **`--missing-only` / `--strict` on `check`** — beyond #82's sketch (owner-lens YAGNI); v1 semantics (missing fails, extra reports) need no flags.
- **Warn-and-edit-last on duplicate `set`** — refusal with line numbers chosen (deny-by-default; editing one of two occurrences leaves a lying file).
- **Refusing interactive TTY on `set`** — no-echo `term.ReadPassword` prompt kept (agent path is non-TTY and unchanged; the prompt serves the human half with no echo channel).
- **Any non-clipboard `get` output (`--print-unsafe`)** — no escape hatch, not even loudly named.
- **Setting empty values** — refused in v1 (empty stdin is far more often a broken producer than intent).
- **Offset-splice (`ValStart/ValEnd`) document model** — parse-to-fields + dirty-flag re-render chosen (same round-trip guarantee, no offset arithmetic to get wrong).
- **Hand-rolled containment in `locate.go`** — `sandbox.WithinWorkspace` reused (panel finding; one audited mechanism, five call sites).
- **`$VAR` expansion; partial reveal in redact; `[env]` config section; shared testutil package** — all declined (value-blind; length is signal; stateless; two 10-line helpers beat a new package).

## Panel review — findings declined

- **Owner lens** judged the (earlier draft's) duplicate-key *warn* behavior fine to keep — superseded by the refuse adjudication above; recorded as the disagreement.
- **Security seat** offered `Nlink>1` warn-on-read for hardlink exfil as the stronger arm — lighter arm taken (documented residual), rationale above. No security finding was declined outright.

## Post-approval chores (non-code)

- `cadence:capture` — Cameron's tenet candidate: "speed and need set priority, not quality" → cadence tracker, doctrine candidate.

## Orchestrator

**Sonnet-drivable** — fully-spec'd implementation from this committed plan file; security review dispatches route to Opus per canon (no ambiguous-reasoning or doctrine triggers remain in the execution path).
