# Releasing forgectl

The full release recipe — goreleaser config, GitHub App auth, quarantine postflight, cask wiring — lives in the personal skill:

```
releasing-to-homebrew-tap
```

Load it in any Claude Code session with `/releasing-to-homebrew-tap` or via the Skill tool.

## Verify before tagging

```bash
mise exec -- goreleaser check
mise exec -- goreleaser release --snapshot --clean --skip=publish
cat dist/homebrew/Casks/forgectl.rb
```

A clean `check` + a snapshot that writes `dist/homebrew/Casks/forgectl.rb` means the tag will work.

## Ship it

```bash
git tag v0.X.0 && git push origin v0.X.0
```

Watch the run: `gh run watch`. The workflow builds all targets, cuts the GitHub release, and pushes the cask to `cameronsjo/homebrew-tap/Casks/forgectl.rb`.
