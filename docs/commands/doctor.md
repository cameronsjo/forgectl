# doctor

Ecosystem health check: claude, tmux/ghostty/cmux, gh auth, config, the local
bench (hearth and chronicle), the trust store, and forgectl's own currency.

```bash
forgectl doctor          # one line per check, with a remediation hint on warn/fail
forgectl doctor --json   # {"checks":[{name,state,detail?,hint?}], "healthy":bool}
```

Every check runs independently, so one failure never hides another. A check's
`state` is `ok`, `warn`, `fail`, or `skip` (an optional integration that is not
configured on this machine). A machine that never set up blessed workflows (no
trust anchor and no trust store) reports the `trust store` check as `skip`; a
present-but-insecure anchor, or a store whose anchor is gone, is still `fail`.

## Exit codes

`0` when no check is `fail` (`warn` and `skip` exit `0`); `1` when at least one is. `--json` prints the full
report to stdout **and** still exits `1` on a failure, so the report is never
lost to the exit code.

## Gating a preflight on only some checks

The exit code is all-or-nothing: a down hearth or chronicle bench (`fail`)
makes `doctor` exit `1` even when the tooling an agent needs is fine. Gate on
the checks you care about by filtering the JSON, not the exit code. There is no
`--require` flag; the exit code stays "any failure", so existing scripts that
rely on it keep working.

```bash
# Exit 0 only if claude, gh and config are not failing (other checks ignored).
forgectl doctor --json \
  | jq -e 'all(.checks[] | select(.name | IN("claude","gh","config")); .state != "fail")' \
  > /dev/null

# List what is failing, for the error message.
forgectl doctor --json \
  | jq -r '.checks[] | select(.state == "fail") | "\(.name): \(.detail)"'
```

`jq -e` sets its exit status from the last output (`false` gives `1`). Note the
pipe: the shell reports `jq`'s status, not `doctor`'s, which is what you want
here. Check names are the ones in the `name` field of the output (`claude`,
`config`, `log path`, `tmux`, `ghostty`, `cmux`, `mdroll`, `sops`, `gitleaks`, `gh`,
`hearth`, `chronicle`, `trust store`, `forgectl version`, and others); run
`forgectl doctor --json | jq -r '.checks[].name'` to see the current list. A
name that no check carries matches nothing, so the `all(...)` passes vacuously:
misspelling a name silently drops that gate.
