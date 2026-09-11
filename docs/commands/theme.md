# theme

Inspect the colours every styled surface in forgectl draws from.

```bash
forgectl theme show          # resolved hex per role, with provenance
forgectl theme show --json   # the same, machine-readable
forgectl theme preview       # render each role so you can see it
```

## Where the colours come from

The default palette is **Artificer's terminal palette** — the same
`themes/_palette.json` that generates the ghostty, tmux, gitmux and cmux
themes. That is deliberate: forgectl usually runs inside a tmux pane under
Ghostty, and drawing from a different palette would make it the one thing on
screen that did not match its own status bar.

It is a different artifact from the web tokens the docs server uses, and the
two disagree — `brandPurple` is `#9070d0` in the terminal palette and `#5a3a9a`
on the web. The design system treats that as intentional: the same hue has two
jobs, and only one of them is foreground text.

`internal/theme/artificer_gen.go` is generated from the vendored palette, never
hand-edited. Re-vendor with `scripts/vendor-artificer.sh`, then
`go generate ./internal/theme`; a test fails if the two drift.

## Roles

Seventeen roles. Fifteen come straight from a palette key; `active` shares
`attention`'s hue, and `bg` exists so contrast has something to measure
against.

| Role | Palette key | Used for |
|---|---|---|
| `accent` | `accent` | headers, the selected row, section titles |
| `ok` | `success` | success marks and attached sessions |
| `danger` | `urgentText` | errors and destructive prompts |
| `warn` | `attention` | warnings |
| `active` | `attention` | the active window or pane |
| `muted` | `fgMuted` | de-emphasised chrome |
| `meta` | `fgSecondary` | row metadata |
| `dim` | `fgDisabled` | skipped or unavailable |
| `fg` | `fg` | ordinary text |
| `steel` | `steel` | session names, paths, flags |
| `brand` | `brandPurpleBright` | the forge glyph |
| `surfaceraised` | `bgRaised` | a raised panel |
| `accentfill` | `accentFill` | a filled accent block |
| `onaccent` | `ink` | text on `accentfill` |
| `urgentfill` | `urgent` | a filled danger block |
| `onurgent` | `ivory` | text on `urgentfill` |
| `bg` | `bg` | the background contrast is measured against |

`danger` is `urgentText`, not bare `urgent`: the palette records bare `urgent`
at about 2.27:1 on the dark background, which fails AA as text. It is a fill
hue, which is why it is `urgentfill` here.

## Reading the contrast column

Contrast is the WCAG 2.x ratio **against the surface the role is actually drawn
on** — the background for most roles, but the matching fill for `onaccent` and
`onurgent`. Measuring `onaccent` against the page would report 1.12:1 for a
pairing nothing ever renders; against its fill it reads 5.65:1, which is the
figure the palette itself records.

A role is flagged `below AA` only when it is under 4.5:1 **and carries text**.
A fill or a raised surface is not text and is owed no such floor. The ratio a
colour must clear follows how it is used, not its hue.

## Overrides

Every role is overridable in `config.toml`. See
[configuration](../configuration.md#theme) for the full section.

```toml
[theme]
mode = "dark"

[theme.colors]
accent = "#dbbb6f"                                # both modes
danger = { dark = "#e6a8a2", light = "#8a2418" }  # per mode
```

An override that names only the mode you are **not** in is inert. `theme show`
warns about exactly that case, because nothing else would: the file looks
right, and the screen shows a different colour.
