## Why

`attention put` counted every parsed item, notes and nested options included, toward its cap of 20. A reply with five questions of four options each fills the cap by Question 4, so Question 5 and a following action were dropped without warning. The spec already said the 20 are question and action items.

## What Changes

- Questions and actions have their own cap of 20; notes and options have a separate cap of 40.
- The same caps apply when a record is read back from the spool.

## Impact

- `internal/attention/parse.go`, `record.go` and their tests.
- `docs/commands/attention.md`.
