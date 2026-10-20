## Purpose
The theme capability resolves the colour palette every styled forgectl surface draws from, lets the operator override it per role in `[theme]`, and reports what actually resolved through `forgectl theme show` and `theme preview`.

## ADDED Requirements

### Requirement: Default palette and presets
forgectl SHALL draw every styled surface (the TUI, prompts, doctor marks, help and error frames) from one palette. The default preset MUST be `artificer`, the Artificer terminal palette; `legacy` is the only other accepted preset.

#### Scenario: No theme section
- **WHEN** `config.toml` has no `[theme]` section and `forgectl theme show --json` is run
- **THEN** `preset` is `artificer`

### Requirement: Seventeen named roles
The theme SHALL define exactly these roles: `accent`, `ok`, `danger`, `warn`, `active`, `muted`, `meta`, `dim`, `fg`, `steel`, `brand`, `surfaceraised`, `accentfill`, `onaccent`, `urgentfill`, `onurgent`, `bg`. Every role MUST be overridable under `[theme.colors]`.

#### Scenario: Unknown role
- **WHEN** `[theme.colors] sparkle = "#ffffff"` is set
- **THEN** the theme section is rejected and the error lists the valid role names

### Requirement: Overrides must be hex colours
A `[theme.colors]` value SHALL be either one `#rrggbb` string for both modes or a `{dark, light}` table setting one or both modes. Any value that is not exactly `#` and six hex digits MUST be rejected, so a control sequence in the config can never reach the terminal. Role names match case-insensitively, and a role set twice MUST be rejected.

#### Scenario: Escape sequence in an override
- **WHEN** `[theme.colors] accent` holds a string containing an ESC byte
- **THEN** the section is rejected with an error naming `[theme.colors].accent` and no override is applied

### Requirement: A bad theme never stops the binary
A rejected `[theme]` section (unknown preset, unknown mode, bad role or bad hex) MUST NOT stop any verb from running. forgectl SHALL print one `[theme] ignored` warning on stderr, fall back to the default palette, and report the problem through `doctor` and `launch doctor`.

#### Scenario: Unknown preset
- **WHEN** `[theme] preset = "neon"` is set and `forgectl status` is run
- **THEN** status runs on the default palette and stderr carries one `[theme] ignored` line

### Requirement: Mode resolution
`[theme] mode` SHALL accept `auto` (the default), `dark` or `light`. An explicit `dark` or `light` MUST be used as given and never probe the terminal. Under `auto`, forgectl MUST ask the terminal for its background only when stdin and stdout are terminals, `NO_COLOR` is unset, and `TERM` does not start with `screen` or `tmux`; otherwise it SHALL resolve dark.

#### Scenario: Inside tmux
- **WHEN** `mode = "auto"` and `TERM=tmux-256color`
- **THEN** no background query is sent by forgectl's own detection and the theme resolves dark

#### Scenario: Forced light
- **WHEN** `mode = "light"` on a terminal that would answer dark
- **THEN** the theme resolves light

### Requirement: theme show reports resolved roles
`forgectl theme show` SHALL print each role's resolved hex, its provenance (the preset name, or `override` when `[theme.colors]` supplied the value for the resolved mode) and its WCAG 2.x contrast ratio. `--json` SHALL emit `{"preset","mode","dark","roles":[{"role","hex","provenance","contrast","below_aa"}],"warnings"}`, with `warnings` an array, never null.

#### Scenario: Override for the resolved mode
- **WHEN** the theme resolves dark and `[theme.colors] accent = { dark = "#dbbb6f" }` is set
- **THEN** the `accent` row shows hex `#dbbb6f` with provenance `override`

### Requirement: Contrast measured against the real surface
Contrast SHALL be measured against the surface the role is drawn on: the matching fill for `onaccent` and `onurgent`, and the background for every other role. A role MUST be flagged below AA only when its ratio is under 4.5:1 and it carries text; `surfaceraised`, `accentfill`, `urgentfill` and `bg` are never flagged.

#### Scenario: Fill role with low contrast
- **WHEN** `urgentfill` measures under 4.5:1 against the background
- **THEN** its `below_aa` is `false`

### Requirement: Inert overrides are warned about
When an override sets only the mode the theme is not resolved to, `theme show` SHALL report a warning naming that role, because the override has no visible effect. The role's provenance for the resolved mode MUST stay the preset name.

#### Scenario: Light-only override on a dark theme
- **WHEN** the theme resolves dark and `[theme.colors] danger = { light = "#8a2418" }` is set
- **THEN** `theme show` lists a warning for `danger` and its provenance is the preset name

### Requirement: theme preview renders every role
`forgectl theme preview` SHALL render a swatch and the hex for every role, plus the ok, warn, fail and skip marks. Like every styled print, it MUST emit no colour when stdout is not a terminal or `NO_COLOR` is set.

#### Scenario: Preview into a pipe
- **WHEN** `forgectl theme preview | cat` is run
- **THEN** every role name is listed and the output carries no ANSI colour sequences
