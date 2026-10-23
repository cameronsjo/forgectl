## Why

A reviewed mark was keyed with the owner and repo exactly as typed. `forgectl pr reviewed mark CameronSjo/Forgectl#76` (or `review mark`) stored a key that never matched the lowercase `cameronsjo/forgectl#76` the forge lists, so the PR never dimmed, and `pr reviewed sync` then deleted the mark although the PR was still open. GitHub and Gitea owner and repo names are case-insensitive, and the session key already folds them.

## What Changes

- The reviewed store ASCII-lowercases every key it writes, reads, or prunes against, and the active hosts it scopes pruning to. A local key (`local/<hex oid>#N`) is already lowercase.
- Marks already on disk are folded on load, keeping the newest when two spellings collide.

## Impact

- `internal/pr/reviewed.go` and a new test file. Every `pr reviewed` and `review` mark path goes through the store.
- One-way: once a write persists folded keys, an older binary misses marks for items whose forge listing uses capitals.
