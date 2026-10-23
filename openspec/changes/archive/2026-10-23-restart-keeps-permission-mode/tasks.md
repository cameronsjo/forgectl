## 1. Implementation

- [x] 1.1 Read the last top-level `permissionMode` from a session's transcript
- [x] 1.2 Validate it against the known modes and the posture's reach, and fall back with a note
- [x] 1.3 Add `--keep-permission-mode` to `forgectl resume`
- [x] 1.4 Relaunch restarted sessions with the flag; carry it in the by-hand command
- [x] 1.5 Notify before each watcher restart, and summarize the run
- [x] 1.6 Tests: transcript extraction, mode validation and argv, CLI wiring, notices

## 2. Docs

- [x] 2.1 Update `docs/commands/resume.md`
