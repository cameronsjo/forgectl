# workflow Specification

## Purpose
The `workflow` command group (alias `flow`) runs declarative TOML workflows that compose forgectl's verbs, gated by a human-presence blessing on user workflow bytes and resumable from per-step checkpoints.

## Requirements

### Requirement: Workflow verbs
forgectl SHALL provide `workflow run <name>` (alias `r`) with `--param k=v` (repeatable), `--dry-run`, and `--resume`; `workflow status <name>`, `workflow list`, `workflow bless <name>`, `workflow verify <name>`, and `workflow trust init|rebuild|list`. `--dry-run` and `--resume` together MUST be refused.

#### Scenario: Group alias
- **WHEN** the user runs `forgectl flow r clean-room-review --dry-run`
- **THEN** it behaves as `forgectl workflow run clean-room-review --dry-run`

#### Scenario: Conflicting flags
- **WHEN** the user passes both `--dry-run` and `--resume`
- **THEN** the command fails without running any step

### Requirement: Workflow resolution
A workflow name MUST be a single plain name; an empty name, `.`, `..`, or a name containing `/` or `\` is refused. A user file `<name>.workflow.toml` in the `workflows/` directory under the config dir SHALL take precedence over a built-in of the same name. `workflow list` SHALL print the built-in workflow names, sorted, and `--json` emits `[{"name":...}]`.

#### Scenario: Path in name
- **WHEN** the user runs `forgectl workflow run ../evil`
- **THEN** the command refuses the name and reads no file

#### Scenario: User file shadows a built-in
- **WHEN** a user file named `clean-room-review.workflow.toml` exists
- **THEN** `workflow run clean-room-review` loads the user file and requires its blessing

### Requirement: Strict versioned grammar
A workflow file SHALL be TOML with a required integer `dsl_version`, `[params]`, and an ordered `[[step]]` list. The parser MUST check `dsl_version` against the supported set (currently `1`) before reading anything else and refuse an unknown version, and MUST refuse any unknown key rather than ignore it.

#### Scenario: Newer grammar
- **WHEN** a file declares `dsl_version = 2`
- **THEN** the run is refused before planning

#### Scenario: Typo'd field
- **WHEN** a step carries an unrecognized key
- **THEN** parsing fails naming the unknown key

### Requirement: Parameter boundary
`workflow run` MUST reject any `--param` name the workflow does not declare, MUST fail when a required param has no value, and MUST reject a `--param` not in `key=value` form.

#### Scenario: Undeclared param
- **WHEN** the user passes `--param evil=1` to a workflow without an `evil` param
- **THEN** the run fails with `unknown param` before any step runs

### Requirement: Blessing gate on user workflows
`workflow run` MUST refuse any user workflow whose exact bytes lack a valid blessing sidecar (`<file>.blessing`) chained to the trust anchor at `/etc/forgectl/trust-anchor.pub`, failing closed on a missing or invalid anchor, store, or sidecar. Verification MUST run on the raw bytes before parsing, and the same buffer is parsed. Built-in workflows and `--dry-run` skip the gate. There is no warn mode.

#### Scenario: Edited after blessing
- **WHEN** one byte of a blessed user workflow changes
- **THEN** `workflow run` refuses it and runs no step

#### Scenario: Dry run unsigned
- **WHEN** the user runs `workflow run <unblessed> --dry-run`
- **THEN** the resolved plan prints and no step runs

### Requirement: Bless ceremony
`workflow bless <name>` SHALL sign a user workflow's exact bytes behind a per-signature Touch ID or account-password prompt and write the `.blessing` sidecar. It MUST refuse a built-in, a file that does not parse, a step verb this binary cannot execute, any `${param}` reference in a guarded field (such as `run`'s `cmd`/`args` or `strip`'s globs), a missing or invalid trust chain, and a machine key not enrolled in the trust store.

#### Scenario: Param injected into run
- **WHEN** a `run` step's `cmd` references `${target}` where `target` is a declared param
- **THEN** `workflow bless` refuses and writes no sidecar

#### Scenario: Step export allowed
- **WHEN** a `run` step's args reference `${workspace}` exported by an earlier `worktree` step
- **THEN** the guard accepts the reference

### Requirement: Verify without running
`workflow verify <name>` SHALL report whether a workflow's blessing is valid without running it, exiting 0 for a built-in or a valid blessing and non-zero otherwise. With `--json` it MUST emit `{"name","builtin","valid"}` only on success and leave stdout empty on failure.

#### Scenario: Unblessed under JSON
- **WHEN** `workflow verify <name> --json` runs on an unblessed user workflow
- **THEN** stdout is empty and the exit is non-zero

### Requirement: Trust store management
`workflow trust init` SHALL mint this machine's presence-gated key, sign a trust store enrolling it, and install the anchor through an interactive sudo step, refusing when an anchor already exists. `trust rebuild` SHALL require an installed anchor and rewrite the store enrolling only this machine. `trust list` prints the anchor key id and enrolled keys; `--json` emits `{"anchor_key_id","keys":[{"key_id","machine","added_at"}]}` with no key material.

#### Scenario: Anchor already present
- **WHEN** `workflow trust init` runs and the anchor file exists
- **THEN** it refuses, naming rotation as a manual re-establishment

### Requirement: Dry-run plan
`workflow run --dry-run` SHALL print the fully resolved plan, including every `run` step's arguments in full, and MUST NOT execute any step or touch run state.

#### Scenario: Plan review
- **WHEN** the user runs a workflow with `--dry-run`
- **THEN** each resolved step prints and no command runs

### Requirement: Step vocabulary
The step verbs SHALL be `run`, `worktree`, `clone`, `teardown`, and `collect` from the engine, plus `strip` and `launch` contributed by modules; a verb registered twice MUST be an error. `worktree` sandboxes a local repo as a git worktree in a fresh temp dir (cloning a remote), `clone` always clones, and both export `${workspace}`. `teardown` is idempotent. `collect` and `launch` currently fail as not yet wired.

#### Scenario: Explicit clone of a local repo
- **WHEN** a `clone` step names a local repository
- **THEN** a full clone, not a linked worktree, is created in a temp dir

### Requirement: Strip stays inside the sandbox
The `strip` step MUST resolve globs only inside `${workspace}`, refusing any absolute glob, any glob with a `..` segment, and any match that resolves outside the workspace through a symlink.

#### Scenario: Traversal glob
- **WHEN** a `strip` glob is `../*.md`
- **THEN** the step fails and deletes nothing

### Requirement: Run step output and argument redaction
A `run` step's stdout MUST be drained and discarded, never captured, so a step may print any amount. Its arguments MUST NOT appear in logs except as flag names.

#### Scenario: Very chatty command
- **WHEN** a `run` step prints far more than the normal output ceiling
- **THEN** the step still succeeds

### Requirement: Checkpoints and resume
Each successful step SHALL be checkpointed atomically to `workflows/.state/<name>.state.toml` with the file's definition hash and the step's input hash; a fresh run clears prior state first and a fully successful run clears it. `--resume` MUST re-verify the blessing, refuse when no state exists or the definition hash changed, skip only the leading unchanged completed steps, and refuse when a step to run needs an export only a skipped step produced.

#### Scenario: Definition edited before resume
- **WHEN** the workflow file changed since the checkpointed run
- **THEN** `--resume` refuses and tells the user to run it fresh

#### Scenario: Export from skipped step
- **WHEN** a remaining step references `${workspace}` and the `worktree` step was checkpointed
- **THEN** `--resume` refuses rather than reconstructing the value from the sidecar

### Requirement: Concurrent run lock
A non-dry run SHALL hold an exclusive advisory lock per workflow name for its whole duration, and a second concurrent run of the same workflow MUST fail fast with "workflow is already running" instead of interleaving. On non-Unix platforms the lock is a no-op.

#### Scenario: Double invocation
- **WHEN** a second `workflow run x` starts while the first still runs
- **THEN** the second fails immediately without running any step

### Requirement: Status view
`workflow status <name>` SHALL print the last run's checkpointed steps without touching the trust chain, noting when the definition has changed or cannot be loaded. `--json` MUST emit `{"name","has_state","run_id","started_at","updated_at","steps","note"}` with `steps` always an array of `{"index","uses","completed_at"}`.

#### Scenario: No saved state
- **WHEN** a workflow has no state file
- **THEN** `--json` emits `has_state: false` and `steps: []`

### Requirement: Param values are substituted once
A param's value SHALL be substituted into a step exactly once and MUST NOT be read again as a `${...}` reference, so the step receives what `--dry-run` printed. Only references written in the workflow file resolve to params or exports.

#### Scenario: Param value that looks like a reference
- **WHEN** a workflow runs with `--param note='${workspace}'` and a `run` step's args reference `${note}`
- **THEN** the step receives the literal text `${workspace}`, not the sandbox path
