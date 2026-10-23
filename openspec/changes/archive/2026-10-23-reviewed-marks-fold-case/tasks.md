## 1. Store

- [x] 1.1 Fold forge keys in `ReviewedStore` (`keys`, `MarkKey`, `UnmarkKey`, `IsReviewedKey`, `SyncKeys`, `SyncKeysScoped`); leave local keys alone
- [x] 1.2 Fold stored keys on load, keeping the newest mark per folded key
- [x] 1.3 Tests: a mark typed in another case dims, survives sync, and unmarks; stored keys fold on load
