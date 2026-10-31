## 1. Environment

- [x] 1.1 Check the hc scrub against the launch strip in both directions (`TestHcScrubAndLaunchStripAgree`)
- [x] 1.2 Move `HC_SCOPE` into hc's pane identity; strip the launcher's at launch and put back the new terminal's
- [x] 1.3 Strip herdr's custom-command focus (`HERDR_ACTIVE_*`) from launches and hc calls
- [x] 1.4 Record why `FORGECTL_SURFACE_PANE` is not a handle
- [x] 1.5 Measure how hc treats a worker once `HC_TERMINAL_*` and `HC_SCOPE` are hidden (design.md)

## 2. One hc runner

- [x] 2.1 Read-only hc queries on the hc adapter (`GetJSON`, `ListJSON`, `ReadText`, `StreamEvents`)
- [x] 2.2 `surface watch` reads through them; one constructor builds hc for both
- [x] 2.3 `OSRunner.RunStreamingScrubbed` for `hc events`

## 3. Specs

- [x] 3.1 Archive `add-hc-surface` and `add-surface-watch`; update the surface Purpose
