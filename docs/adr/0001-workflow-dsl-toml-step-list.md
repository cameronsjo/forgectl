# 0001 — Workflow DSL is a TOML step list

- **Status:** Accepted (2026-07-01)
- **Context:** #9 workflow DSL design spike. Related: 0002, 0003, 0004.

## Context

The `forgectl workflow` feature (#9) needs a file format for declarative workflows — an ordered
list of steps (`worktree`, `strip`, `run`, `launch`, `collect`, `teardown`) with typed
parameters. The format is also a **security surface**: workflow files execute commands, so they
are signed and verified (#10), and they may be shared (#17). The format must be human-authorable
(a new workflow requires no Go changes — a #9 acceptance criterion), diff-reviewable, and sign
cleanly as a flat file.

## Decision

Workflow files are **TOML**, with steps as an ordered `[[step]]` array of tables and params under
`[params]`.

## Alternatives considered

- **TOML step list (chosen).** Pros: forgectl already depends on `BurntSushi/toml` for its
  config (no new dependency), matches the house config idiom, human-editable, comments-friendly,
  and a flat text file is trivial to hash + sign. Cons: array-of-tables (`[[step]]`) is slightly
  verbose for long workflows.
- **YAML step list.** Pros: familiar CI-workflow shape, richer nesting. Cons: adds a YAML
  dependency forgectl doesn't carry; YAML's implicit type coercion and indentation-significance
  are footguns in a security-sensitive executable file.
- **Embedded Go builder.** Pros: type-safe, no parser to write. Cons: workflows can't be authored
  without editing Go — directly violates #9's "define a new workflow with no Go changes."
- **cmux-JSON-compatible (reuse `commands[]` templates).** Pros: reuses an existing template shape
  Cameron already uses. Cons: JSON is clunkier to hand-author and sign-review (no comments), and
  couples the DSL to cmux's schema.

## Consequences

- The parser is a thin `toml.Decode` into a `Workflow` struct — minimal new machinery. The decode
  is **strict**: an unknown key is a parse error, so a typo'd field (e.g. `glob` for `globs`)
  can't silently no-op, and a newer grammar's fields can't be silently ignored under an older
  `dsl_version` (0004).
- Signing (#10) operates on the raw file bytes; a one-byte edit invalidates the signature.
- A future need for richer control flow (conditionals, loops) will strain a flat step list; if
  that arrives, it bumps `dsl_version` (0004) rather than breaking existing files.
