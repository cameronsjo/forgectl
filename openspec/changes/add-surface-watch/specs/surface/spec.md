## ADDED Requirements

### Requirement: Live worker watch
`forgectl surface watch` SHALL show one row per worker named in `--workers FILE` (lines `name|worktree|brief dir`) and, with `--cos`, per live hc terminal whose name starts with `cos-`. Each row SHALL show the name, hc address, hc status labelled self-reported, age, model when its argv names one, its report files and which end in `REPORT-COMPLETE`, and its PR number, head, check rollup and a `cadence-review` verdict summed by the rule `forgectl sweep` uses.

#### Scenario: Blocked worker needs you
- **WHEN** a watched worker's hc status is `blocked`
- **THEN** its row is marked "needs you" and a banner names it and the `hc read` command that shows its dialog

#### Scenario: A clean re-run does not hide a Critical
- **WHEN** a PR's head carries a `crit=1` marker and a later `crit=0` marker from the same reviewer
- **THEN** the REVIEW cell shows `c1`, as `forgectl sweep` would count it

#### Scenario: Off a terminal
- **WHEN** stdout is not a terminal
- **THEN** one plain snapshot is printed, or with `--json` one JSON snapshot, and the command exits 0, or 1 when hc could not be read at all

### Requirement: Worker watch is read-only and safe to leave open
`surface watch` MUST NOT send keys, kill, or brief any terminal. Worker screen text MUST have escape sequences stripped and remaining control characters escaped before rendering. GitHub reads MUST be cached for at least 30 seconds per worker.

#### Scenario: Hostile screen text
- **WHEN** the selected worker's screen contains an OSC 52 clipboard write, a screen clear and a bidi override
- **THEN** the rendered tail contains none of those sequences

#### Scenario: GitHub cache
- **WHEN** the view collects twice within the GitHub cache lifetime
- **THEN** GitHub is called once per worker
