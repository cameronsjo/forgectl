## Context

`internal/notify` posted every macOS notification through osascript. macOS credits a banner to the posting app, and osascript's belong to Script Editor, so a click opens Script Editor's file panel. Three other asks were routed elsewhere the same day: a desk UX deep dive and a notch surface for notifications, each in its own session. The notch work may later replace this sender with a native app.

## Decisions

### terminal-notifier posts, osascript is the fallback

terminal-notifier 3.1.0 (Homebrew, bundle `fr.julienxx.oss.terminal-notifier`) posts through UNUserNotificationCenter. Its banners carry an `-execute` command for a click, `-activate` for an app to bring forward, and `-group` so a later call replaces or removes a banner. It needs the operator's notification permission. Without it every send exits 3, so a failure falls back to osascript instead of losing the signal. `desk add` and `drain notify-test` say when that happened.

Declined: a native Swift notifier (needs an app bundle; the notch work decides whether one exists), herdr's own notification alone (no banner while herdr is not frontmost), `alerter` (not in Homebrew core, same mechanism), and a warning on every send when terminal-notifier is simply absent (doctor carries that).

### The click runs one shell string, built from fixed parts

`-execute` runs through `/bin/sh`. It is built from forgectl's own executable path, the fixed verb `herdr _focus-terminal`, a terminal id matching `^[A-Za-z0-9_-]{1,64}$`, and herdr-app's absolute path. Each part is single-quoted. Title and body never enter it. A terminal id that does not match drops `-execute` and keeps `-activate`.

The executable path is not resolved through symlinks. Homebrew's `/opt/homebrew/bin/forgectl` outlives an upgrade, while a versioned Caskroom path does not, and a banner can sit in Notification Center across one.

A click runs with launchd's bare PATH, so herdr-app is passed as an absolute path. The verb refuses any path whose base name is not `herdr-app`.

### terminal-notifier reads values as property lists

terminal-notifier reads option values through NSUserDefaults. A value starting with `[`, `(`, `{`, `<`, `-` or a quote is misread: the send exits 2, or (measured, for `(a, b)`) hangs until the timeout. It strips one leading backslash from every string value (`AppDelegate.m`, `objectForKeyedSubscript:`), so forgectl puts a backslash in front of every free-text value.

### The tab is found at click time

The banner names the terminal (`HC_TERMINAL_ID`), not its tab. The click looks the terminal up in `herdr-app ls`, then selects its space and tab; herdr-app's `tab.select` is not documented to switch spaces. A tab moved since the banner was posted still resolves, and posting costs no herdr call. herdr has no focus by pane, so the tab is the finest grain.

### Banner groups

A desk item's group is keyed by the hash it was queued at. `desk add` knows the hash, and every meta that leaves pending carries it, so the leave hook removes the banner without needing the item's name. A drain worker's group is keyed by its name. Clearing ignores `notify_macos` and drops failures, as the pane-state release does.

## Deviations from the approved plan

The plan was approved in session `d97a8686-dc8c-47ce-af84-c537175b00b4` and first copied to `docs/plans/`, which this repo has frozen since 2026-10-20. This change replaces that copy.

- Step 0 (prove a click works on Darwin 27) passed on 2026-11-22. The operator granted terminal-notifier permission, then clicked two probes. One went to another space's tab and one came back. Both are logged in `/tmp/forgectl-probe-click.log`.
- The plan assumed terminal-notifier used the deprecated NSUserNotification. It uses UNUserNotificationCenter and needs permission, which is why it falls back to osascript and why doctor reports the sender.
- The verb is `forgectl herdr _focus-terminal`, not `forgectl notify open`. A top-level `notify` group is a new module, which needs registry pins and a README section, for a verb nobody types.
- The click target is a herdr-app terminal resolved at click time, not a tab id resolved at post time.
- Only `desk add` from a herdr-app terminal gets a tab target. The drain's worker refs are herdr 0.9 panes, and the update watcher and `pr drain` run in no terminal, so their banners bring herdr-app forward only. A target for herdr 0.9 panes is a follow-up.
- Desk banner groups are keyed by the queued hash, not the item name.

## Loop

| Thing left behind | Opened by | Closed by |
|---|---|---|
| A banner per desk item | `desk add` | removed when the item runs or is skipped |
| A banner per drain worker | needs-you | removed when the worker leaves needs-you |
| Test desk items from live verification | this change's verification | skipped by this session |
| Notifier PR cameronsjo/forgectl#1476 | this change | merge after review and green CI |

## Workarounds

| Limit | Real? | Mitigation |
|---|---|---|
| osascript banners belong to Script Editor | yes | another sender |
| terminal-notifier needs notification permission | yes (exit 3) | fall back to osascript; doctor and notify-test say which sender posted |
| terminal-notifier parses values as property lists | yes (exit 2 or a hang) | a leading backslash on every free-text value |
| herdr has no focus by pane | yes | focus the tab |
