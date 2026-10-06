# Cancelling a prompt

Backing out of a prompt is not an error. Esc or Ctrl+C at a forgectl picker or
confirm, and No at a Yes/No confirm, print `cancelled` and exit **130**
(`resume`, `projects pick`, `pr pick`, `tmux kill`, `clean`, `branch --apply`,
`pr findings cleanup`). Nothing is styled as an
error, and 130 is neither 0 (a script can tell "declined" from "done") nor the
generic failure 1.

- An abort (Esc, Ctrl+C) writes `cancelled` to **stderr**; a No at a confirm
  writes it to stdout, as it always has.
- The `pr` review and clean-room prompts have a **Cancel** button of their own.
  It records a typed `declined` outcome instead of exiting 130; Esc and Ctrl+C
  on those prompts exit 130 like any other.
- A verb with several confirms (`clean --apply` runs one per pass) exits 130 when
  *any* of them was declined, even if another was accepted and ran.
- Closing the bare menu (`forgectl` with no verb) with Esc exits 0.
- A prompt needs a terminal. Without one, pass `--yes` where the verb offers it.

A cancel never goes through the error renderer. That renderer
([fang](https://github.com/charmbracelet/fang)) asks the terminal for its
background colour first, and on a terminal that never answers
(some multiplexer, ssh, and mosh setups) that wait takes about 4.5 s; see
[Theme](configuration.md#theme).
