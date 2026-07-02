# 0004 — Workflow DSL versioning is dual-axis: `dsl_version` (grammar) + `version` (workflow)

- **Status:** Accepted (2026-07-01)
- **Context:** #9 workflow DSL design spike. Related: 0001, 0002; enables #10, #16, #17.

## Context

A signed, shareable, evolving DSL that declares no version is a format you cannot safely change:
an older executor could silently misread a file written for a newer grammar, and a shared workflow
has no identity to pin or attest. The spike exists to de-risk the grammar, so versioning must be in
the grammar from v1.

Two different questions hide under "versioning":

1. *Which grammar does this file speak?* — the parser's contract.
2. *Which revision of this particular workflow is this?* — the workflow's identity/provenance.

## Decision

Two independent version fields, both top-level, both inside the signed content.

- **`dsl_version` (integer, required)** — the grammar contract. The parser reads it **first** and
  gates on a `SupportedDSLVersions` set; an unknown version is a **typed refusal before planning**.
  New step verbs or fields bump `dsl_version`; files keep parsing under their declared version.
- **`version` (semver string, recommended)** — the workflow's own revision, author-bumped. It is
  the provenance handle: #10's attestation signs over `name@version` + file hash, #17's registry
  pins it, #16's dogfood reconciles against it on re-run. Editing a signed workflow bumps `version`
  and re-signs.

## Alternatives considered

- **Dual-axis (chosen).** Pros: cleanly separates "can I parse this?" (grammar) from "which
  revision is this?" (provenance); each evolves on its own cadence. Cons: two fields to explain.
- **Single `version` for both.** Rejected: conflates grammar compatibility with workflow revision
  — bumping a workflow's content would falsely imply a grammar change, and a grammar bump would
  falsely churn every workflow's revision.
- **No explicit version (infer from fields present).** Rejected: makes the parser guess, defeats
  the "refuse a newer grammar" safety property, and gives sharing/attestation nothing to pin.

## Consequences

- The parser gains a `SupportedDSLVersions` gate as its first check — a security property (a
  tampered file claiming a newer grammar can't smuggle unparsed steps past an older executor) as
  much as a compatibility one.
- Both versions are integrity-protected once #10 signs the file.
- Built-in shipped workflows carry a `version`; bumping one is a normal, signable edit.
