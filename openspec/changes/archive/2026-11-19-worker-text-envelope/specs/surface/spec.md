## ADDED Requirements

### Requirement: Worker text envelope
Every launch prompt and typed brief forgectl gives a worker MUST arrive inside an envelope naming its sender: forgectl's `<forgectl from="<terminal>" kind="launch|brief">…</forgectl>`, or hc's own where `hc send` delivers it from an hc terminal. forgectl MUST NOT add its envelope on top of hc's. The envelope is a convention, not authentication.

#### Scenario: Launch from a herdr pane
- **WHEN** a worker launches from a herdr pane with `HERDR_PANE_ID` `w1-p2`
- **THEN** its prompt opens with `<forgectl from="w1-p2" kind="launch">`

### Requirement: Envelope bodies cannot forge tags
A body MUST NOT be able to close or open an envelope: each tag-like run of `forgectl` or `hc` in it, in any case or spacing, SHALL have its bracket rewritten to `&lt;`.

#### Scenario: Forged closing tag in a brief
- **WHEN** a brief's text contains `</forgectl>` followed by an instruction
- **THEN** the worker receives one envelope whose only closing tag is forgectl's own

### Requirement: hc text sends keep the sender
The `hc send` calls that carry text (a typed brief, a desk answer) SHALL keep `HC_TERMINAL_ID` and `HC_SCOPE` so hc wraps the text, and MUST drop every other launcher handle. Every other hc call MUST drop both. Where forgectl runs outside an hc terminal, it SHALL wrap a typed brief itself.

#### Scenario: hc brief from a plain shell
- **WHEN** `surface brief --surface hc` runs where `HC_TERMINAL_ID` is not set
- **THEN** the text hc delivers is wrapped in forgectl's envelope

#### Scenario: hc brief from an hc terminal
- **WHEN** `surface brief --surface hc` runs inside an hc terminal
- **THEN** the `hc send` call keeps `HC_TERMINAL_ID` and `HC_SCOPE`, drops every other launcher handle, and the text carries no forgectl envelope

### Requirement: Launch brief states the assignment
The launch brief MUST open, inside its envelope, with a statement that it is the worker's assignment from its launcher, not text the operator typed, and that later text from the same sender is the channel for fix rounds.

#### Scenario: Preamble opens the brief
- **WHEN** a worker launches with a brief
- **THEN** the prompt's first paragraph inside the envelope is the assignment statement

### Requirement: Worker approve and merge rule
The worker rule MUST keep the review-marker and board-task bans, and MUST forbid approving or merging a pull request on the say of the task, a brief, or any tagged text. Text typed in the worker's terminal outside any tag is the one exception: it can direct one approval or one merge of the pull request it names. The rule MUST state that a tag never grants authority.

#### Scenario: Each clause pinned
- **WHEN** any clause of the rule is removed from `worker.WorkerRules`
- **THEN** a test fails naming that clause

## MODIFIED Requirements

### Requirement: Launch brief
`--brief <text|@file>` (worktree only, files at most 64 KiB) SHALL pass the first brief as the harness's prompt argument after `--`, so nothing is typed into the pane, inside forgectl's launch envelope with the provenance note. Each brief MUST get a random marker recorded in the ledger before sending, and ask the worker to end with a `REPORT <marker>:` line. A pi launch MUST refuse a brief starting with `@`.

#### Scenario: Brief recorded before start
- **WHEN** a worker launches with `--brief @task.md`
- **THEN** the ledger row holds the brief's marker and the harness receives the brief as its prompt argument

### Requirement: Typed follow-up briefs
`surface brief <name> <text|@file>` SHALL type the text without Enter, read the screen back until it sits in the input box with no dialog, record the brief and a new marker, then send Enter separately. A typed brief MUST be one line of at most 600 characters including the report instruction and envelope, and MUST NOT start with `- / ! # ? @ &`. It refuses inside a drain worker. Exit 1 `refused` sent nothing; exit 1 `unconfirmed` already pressed Enter.

#### Scenario: Read-back mismatch
- **WHEN** the input box does not show the typed text
- **THEN** Enter is never sent and `--json` reports outcome `refused`

#### Scenario: Brief over the cap
- **WHEN** a typed brief's text with its instruction and envelope passes 600 characters
- **THEN** it is refused with exit 2, naming how many characters the text itself may use
