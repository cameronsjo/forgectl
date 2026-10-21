## 1. Codex trust preflight

- [x] 1.1 Read codex folder trust from `$CODEX_HOME/config.toml`, exact path or physical path, no parent inheritance
- [x] 1.2 Refuse codex worker and plain launches (and `--dry-run`) with no trust entry, naming the fix
- [x] 1.3 Tests: the trust reading, the config lookup, the message, the worker check and a `--dry-run` refusal
