# forgectl

Personal dev-experience CLI for a headless macOS workbench driven over SSH — from laptops, phones, and Termius. First module is tmux. Supersedes the ad-hoc bash `s` script. Delegates smart session-naming to `sesh`.

Built for two hands and one thumb:

- **Power mode** — typed verbs (`forgectl tmux ls`, `forgectl tmux pick`). Full keyboard, full control.
- **Thumb mode** — bare `forgectl` opens a TUI menu. Number-key select. Narrow-screen. Forgiving input. Works fine in Termius over mosh.

## Install

```sh
brew install cameronsjo/tap/forgectl
```

## Usage

```sh
forgectl                   # open TUI menu (thumb mode)
forgectl tmux ls           # list sessions
forgectl tmux pick [name]  # connect/smart-create via sesh (no name → list)
forgectl tmux kill <name>  # kill a session (--others keeps only it)
forgectl tmux rename <old> <new>
forgectl tmux windows      # list windows across all sessions
forgectl tmux tree         # session → window → pane tree
forgectl tmux last         # jump to the last-used session
forgectl tmux cheat        # tmux terms + the keys that matter
```

The `fx` alias is available after install:

```sh
fx                    # same as bare forgectl — opens the TUI
fx tmux ls
```

## How it fits together

```
forgectl tmux pick
    └── delegates session selection to sesh
            └── hands off to tmux
```

`sesh` handles the smarts — path discovery, named sessions, zoxide integration. `forgectl` provides the stable verbs and the thumb-friendly TUI on top.
