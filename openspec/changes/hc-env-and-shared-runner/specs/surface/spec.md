## ADDED Requirements

### Requirement: Launches strip the launcher's handles
A surface launch MUST strip the launcher's pane identity for every terminal manager (for hc, every `HC_TERMINAL_` variable and `HC_SCOPE`), the launcher's Claude Code session id, and the pane focus herdr gives a custom command (the four `HERDR_ACTIVE_` variables). The trampoline SHALL put back only the new pane's own identity for the backend that launched it.

#### Scenario: Launch bound to a herdr key
- **WHEN** a surface launch runs as a herdr custom command, with `HERDR_ACTIVE_PANE_ID` naming the focused pane
- **THEN** the new surface's harness does not receive `HERDR_ACTIVE_PANE_ID`

#### Scenario: hc worker scope
- **WHEN** an hc worker is launched from inside another hc terminal
- **THEN** the worker's harness carries its own terminal's `HC_TERMINAL_ID` and `HC_SCOPE`, not the launcher's

### Requirement: hc calls drop every launcher handle
Every hc call MUST drop each variable a launch strips as the launcher's, plus Claude Code's variables, every `HC_TERMINAL_` variable, and a worker's identity. A variable that hc calls drop and a launch keeps MUST be one the code lists with its reason.

#### Scenario: A key added to the hc scrub alone
- **WHEN** a key is added to the hc calls' scrub and neither stripped at launch nor listed with a reason
- **THEN** the drift test fails

## MODIFIED Requirements

### Requirement: Worker watch is read-only and safe to leave open
`surface watch` MUST NOT send keys, kill, or brief any terminal. Worker screen text MUST have escape sequences stripped and remaining control characters escaped before rendering. GitHub reads MUST be cached for at least 30 seconds per worker. It MUST reach hc through the same runner as the hc worker verbs, so every hc call it makes, the `hc events` subscription included, runs the hc found on PATH at start by absolute path and drops the launching session's handles from its environment.

#### Scenario: Hostile screen text
- **WHEN** the selected worker's screen contains an OSC 52 clipboard write, a screen clear and a bidi override
- **THEN** the rendered tail contains none of those sequences

#### Scenario: GitHub cache
- **WHEN** the view collects twice within the GitHub cache lifetime
- **THEN** GitHub is called once per worker

#### Scenario: Watch started from a Claude Code session in an hc terminal
- **WHEN** `surface watch` runs with Claude Code's messaging socket, `HC_TERMINAL_ID` and `HC_SCOPE` in its environment
- **THEN** none of them reaches any `hc` it runs, while `HC_SESSION` does
