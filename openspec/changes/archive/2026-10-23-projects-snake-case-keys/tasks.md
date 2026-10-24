## 1. Implementation

- [x] 1.1 Add `ssh_url` and `local_path` to `projects list --json` rows through a wire type, leaving `projects.Repo` unchanged
- [x] 1.2 Accept both spellings in `--fields`
- [x] 1.3 Point `surface status` and `surface usage` at `surface list` on an unknown name
- [x] 1.4 Tests: both spellings present, omission on an uncloned row, `projects.Repo` still decodes, `--fields` with each spelling, `--limit` envelope

## 2. Docs

- [x] 2.1 `docs/commands/projects-and-review.md`
