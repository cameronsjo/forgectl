## 1. Queue fields

- [x] 1.1 `key` and `tier` on every row; same-key duplicates from one source collapse; short key in the text view (T1).
- [x] 1.2 `sources.herdrDetail` and `sources.sweepGeneratedAt` (T1).
- [x] 1.3 Desk key also hashes the item's sha256 prefix and `added_at`, so a reused name is a new key (S1 N2).
- [ ] 1.4 Drain row and `sources.drain` (T2).
- [ ] 1.5 Desk items record `terminal` and `terminal_core`; `sources.core` from `hc ls --json` (T3a, S1 I1 and N1).

## 2. Jump

- [ ] 2.1 `attention jump --key=K [--json]` through cameronsjo/forgectl#1476's `focusTerminal`, showing the target terminal's title (T3b, S1 N3).
- [ ] 2.2 Security review of the jump path (S1 part 2).

## 3. The app

- [ ] 3.1 NotchCore: decode, raise rules, badge, health, jump messages, forgectl locator (T4).
- [ ] 3.2 App shell: DynamicNotchKit island per screen, FSEvents and a 30 s poll, menu (T5).
- [ ] 3.3 Security review of the release path (S2).
- [ ] 3.4 Bundle, sign, notarize, `forgectl-notch` cask, `doctor` row (T6).

## 4. Docs and decision

- [x] 4.1 `docs/commands/attention.md` and `docs/commands/desk.md` for each field as it lands.
- [ ] 4.2 `docs/notch.md` and the README Usage roster (T7).
- [ ] 4.3 File the supersede-banners decision issue at ship (T8).
