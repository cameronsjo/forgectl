## ADDED Requirements

### Requirement: Queuing terminal in the item meta
`desk add` SHALL record the item's queuing herdr-app terminal as `terminal`, and the hc core it belongs to as `terminal_core`, in the item meta, only when it runs in a herdr-app terminal (`$HC_TERMINAL_ID` set, no `HERDR_*` variables), the value has the shape of a local hc address, and no tmux, screen or zellij is in between. Otherwise it MUST record neither.

#### Scenario: Inside tmux
- **WHEN** `desk add` runs in a tmux pane whose environment carries `HC_TERMINAL_ID`
- **THEN** the item meta has no `terminal` and no `terminal_core`
