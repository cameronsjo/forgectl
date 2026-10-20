## 1. Scaffold

- [x] 1.1 Run `openspec init --tools claude,codex` and commit the generated skills and commands
- [x] 1.2 Write the project context and artifact rules in `openspec/config.yaml`
- [x] 1.3 Pin the CLI in `scripts/openspec/` and add `scripts/openspec.sh`

## 2. Baseline specs

- [x] 2.1 Write one spec per capability listed in the proposal, from the code first and `docs/commands/` second
- [x] 2.2 Pass `scripts/openspec.sh validate --all --strict`
- [x] 2.3 Archive this change so the specs land in `openspec/specs/`

## 3. Process

- [x] 3.1 Add the `openspec` job to CI
- [x] 3.2 Freeze `docs/plans/` in `TestPlansIndex` and regenerate its index
- [x] 3.3 Point AGENTS.md, CONTRIBUTING.md and docs/README.md at `openspec/`
