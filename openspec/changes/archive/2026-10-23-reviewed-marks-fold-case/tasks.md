## 1. Store

- [x] 1.1 Fold keys in `ReviewedStore` (`keys`, `MarkKey`, `UnmarkKey`, `IsReviewedKey`, `SyncKeys`, `SyncKeysScoped` and its active hosts)
- [x] 1.2 Fold stored keys on load, keeping the newest mark per folded key
- [x] 1.3 Tests: a mark typed in another case dims, survives sync, and unmarks; stored keys fold on load and on disk; a capitalised active host still prunes
