# hub-menu Specification

## Purpose
The hub is the menu bare `forgectl` opens on a terminal, and `forgectl menu` prints the same contents as text or JSON so agents and scripts can read it without a TTY.

## Requirements

### Requirement: Hub layout
On a terminal, bare `forgectl` SHALL open the hub: a status line, then the pinned commands (`docs`, `pr`, `projects`, `tmux`, `sessions`) in that order, then up to three recent commands ranked by shell history, then one row per area (`agents`, `repos`, `shell`, `setup`). When no `config.toml` exists, a `forgectl init` row MUST show first.

#### Scenario: First run
- **WHEN** bare `forgectl` opens on a terminal with no `config.toml`
- **THEN** a `forgectl init` row is shown above the pinned commands

### Requirement: Numbered keys never run a command
Keys `1` to `9` SHALL open the pinned rows and then the areas, in order. A keyed row MUST only open a list or the argument picker and MUST NOT run a command. Recent rows and the first-run `init` row MUST carry no key, so a key always means the same row.

#### Scenario: Press 1 on the hub
- **WHEN** the operator presses `1` on the top hub screen
- **THEN** the `docs` row opens its subcommands and nothing runs

#### Scenario: Digits inside an area
- **WHEN** the operator presses `3` inside an area
- **THEN** the cursor moves to the third row and only `enter` acts on it

### Requirement: Hub search and navigation
`/` SHALL search every command and runnable subcommand by name, showing subcommands under their full path; inside an area it filters that area. `enter` opens the selected row, and `q` or `esc` goes back one screen and quits from the top. `esc` MUST clear an active search before it backs out of a screen.

#### Scenario: Search for a subcommand
- **WHEN** the operator types `/findings` on the hub
- **THEN** `pr findings list` appears as a result under its full path

### Requirement: Hub shows what it will run
The line under the list SHALL show the exact `$ forgectl …` invocation the selected row runs, and a hub-chosen command MUST echo that line to stderr before it runs. A command whose required argument the picker cannot take MUST leave the hub and print the invocation with its placeholders instead of running.

#### Scenario: Row needing an argument
- **WHEN** the operator presses `enter` on a row whose required argument fills another CLI's subcommand slot
- **THEN** the hub exits and prints the invocation with its placeholder, and nothing runs

### Requirement: Picker opt-out
A command carrying the `forgectl:hub-no-picker` annotation MUST never be offered the hub's inline argument picker, and the hub's argv builder MUST refuse it as a backstop.

#### Scenario: Annotated command
- **WHEN** the operator selects a command annotated `forgectl:hub-no-picker`
- **THEN** no argument picker opens

### Requirement: Small and dumb terminals
Below 20 columns by 8 rows the hub SHALL say the terminal is too small instead of drawing, and take no key but `q` or `esc`. Under `TERM=dumb` bare `forgectl` MUST NOT open the hub; it exits 2 and points at `forgectl menu`.

#### Scenario: Tiny terminal
- **WHEN** the hub opens in a 15 by 6 terminal
- **THEN** it shows a too-small message and only `q` or `esc` does anything

### Requirement: menu prints the hub without a TTY
`forgectl menu` SHALL print the hub's status line and sections without opening a screen or needing a TTY. It MUST NOT run any row and SHALL exit 0. Its text output MUST be plain, with no colour or escape sequences.

#### Scenario: Menu from a pipe
- **WHEN** `forgectl menu | cat` is run
- **THEN** the hub's contents are printed as plain lines and the exit status is 0

### Requirement: menu reads only local sources
The status line SHALL come from the checkout's `.git` files, `tmux list-sessions`, and the PR review store, all read locally under the hub's short time budget. `menu` MUST NOT use the network, and the review store MUST be read only when it already exists.

#### Scenario: tmux unavailable
- **WHEN** tmux is not installed, so `tmux list-sessions` fails
- **THEN** `menu --json` reports `header.tmux_sessions` as `null`, never `0`

### Requirement: menu --json shape
`menu --json` SHALL emit `{"header","first_run","pinned","recent","commands"}` with every key always present and the three sections as arrays, never null. Each row MUST carry `command`, `group`, `argv`, `description`, `usage`, `needs_args` and `leaves`, nested to any depth. Fields MAY be added and existing fields MUST NOT change.

#### Scenario: Required argument
- **WHEN** a row's usage is `forgectl pr <ref>`
- **THEN** its `needs_args` is `true` and its `argv` is `["pr"]`

#### Scenario: Header outside a checkout
- **WHEN** `menu --json` runs outside a git checkout
- **THEN** `header.project` and `header.branch` are `null`

### Requirement: Recent rows hold no history text
The recent rows SHALL hold command paths only, each resolved against the registered commands. No text from the shell history file MUST ever appear in hub or `menu` output.

#### Scenario: Secret in history
- **WHEN** the shell history holds `forgectl env set TOKEN=abc`
- **THEN** a recent row shows at most the command path `env set`, never `TOKEN=abc`

### Requirement: menu scoped to one group
`forgectl menu <group>` SHALL accept a command name (keeping rows whose command path starts with it) or an area name (keeping `commands` rows in that area), keeping `header` and `first_run`. A name that is neither MUST be a usage error, exit 1 with code `usage_error` under `--json`, that lists every valid command and area name.

#### Scenario: One area
- **WHEN** `forgectl menu --json repos` is run
- **THEN** `pinned` and `recent` are empty and `commands` holds only rows whose `group` is `repos`

#### Scenario: Unknown group
- **WHEN** `forgectl menu --json nosuch` is run
- **THEN** stdout is empty, stderr holds one object with code `usage_error`, and the exit status is 1
