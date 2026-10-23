## MODIFIED Requirements

### Requirement: Publish a reply's items
`forgectl attention put --session ID --turn ID` SHALL read a reply from stdin, parse the last `What's next:` block outside fenced code, keep at most 20 question and action items plus at most 40 note and option items, so notes and options never crowd out a question or action, and write them as that session's record. It MUST print `items=`, `seen=` and `read=`. stdin MUST NOT be a terminal (exit 2), a reply over 16 MiB MUST be refused, and a reply over 64 KiB MUST be parsed from its last 64 KiB.

#### Scenario: Reply with two items
- **WHEN** a reply ending in a `What's next:` list with one `**Question 1**:` and one `**Action 2 (Claude)**:` line is piped to `put`
- **THEN** the session's record holds one `q` item and one `a` item
- **AND** `put` exits 0

#### Scenario: Terminal on stdin
- **WHEN** `put` runs with stdin attached to a terminal
- **THEN** it exits 2 and writes nothing

#### Scenario: Options under many questions
- **WHEN** a block holds five questions with four options each, then `**Action 6 (Cameron)**:`
- **THEN** the record holds all five questions and action 6
