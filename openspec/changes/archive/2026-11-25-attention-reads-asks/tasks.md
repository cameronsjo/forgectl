## 1. Retire the spool

- [x] 1.1 Remove `attention put`, `attention rm`, the spool store, its pruning and the reply parser
- [x] 1.2 Remove `[attention] ttl_hours`, the `q`/`a` rows and `sources.spool`

## 2. Asks as rows

- [x] 2.1 Read open asks read-only through the asks store; `OpenReadOnly` changes no mode
- [x] 2.2 Ask rows at cost 4, tier `count`, cleaned text; snoozed hidden, answered absent
- [x] 2.3 `sources.asks`, with `remote` when the asks live on another host
- [x] 2.4 Staged breaks for the read-only open, the text cleaning and the snooze filter

## 3. Docs

- [x] 3.1 `docs/commands/attention.md`, `docs/configuration.md`, README
