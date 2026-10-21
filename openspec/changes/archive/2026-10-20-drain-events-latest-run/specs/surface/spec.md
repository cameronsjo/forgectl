## ADDED Requirements

### Requirement: Drain events by run
`surface drain events` SHALL print the events file oldest first. `seq` counts from 1 in each drain process. With `--since <n>` above 0 it MUST print only the latest run's events with `seq` above `n`. With no `--since`, or `--since 0`, it SHALL print every run. `--latest-run` MUST limit the output to the latest run, from its last `seq` 1 event on.

#### Scenario: Latest run only
- **WHEN** the events file holds two runs and `surface drain events --latest-run --json` runs
- **THEN** only the second run's events print, starting at its `seq` 1

#### Scenario: Zero cursor reads every run
- **WHEN** the events file holds two runs and `surface drain events --since 0 --json` runs
- **THEN** both runs' events print
