// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
)

// configRecoveryVerbs are the top-level command names that still run on a
// config.toml that does not parse. Hook-invoked subverbs are exempt through
// hookVerbs. config and doctor report the parse error as
// a finding; version, help, completion and man never read configuration.
// init is absent on purpose: its writer strictly decodes the file under lock
// and refuses an unparseable one, so it would fail anyway, less clearly.
// Compared against the resolved command name, never the typed token, so an
// alias such as `cfg` gets its command's treatment.
var configRecoveryVerbs = map[string]bool{
	"config": true, "doctor": true, "version": true,
	"help": true, "completion": true, "man": true,
	"__complete": true, "__completeNoDesc": true,
}

// launchRecoveryVerbs is the carve-out for `forgectl launch <verb>`. edit is
// the recovery path (it opens the file in $EDITOR without parsing it) and
// doctor reports the parse error. init is absent for the same reason as the
// top-level init; which/stats/migrate would report against defaults the
// operator did not choose.
var launchRecoveryVerbs = map[string]bool{
	"edit": true, "doctor": true,
	"help": true, "--help": true, "-h": true,
}

// configParseGate turns a config.toml that exists but does not parse, or
// cannot be read (permission denied, a directory, a FIFO), into a hard error
// for every command that would otherwise run against silent defaults
// (forgectl#653, forgectl#684). The default log level is off, so the loader's
// WARN was invisible and `docs list --json` exited 0 against the wrong roots.
// Only an absent file is not an error. Recovery verbs and help/version flags
// are exempt.
func configParseGate(cfg config.Config, root *cobra.Command, args []string) error {
	parseErr := cfg.DecodeError()
	if parseErr == nil || configGateExempt(root, args) {
		return nil
	}
	return fmt.Errorf("%w; fix the file, or run `forgectl config` or `forgectl doctor` to inspect it", parseErr)
}

// resolveVerb maps the first non-flag token to a command name: builtins as
// themselves, a registered command or alias to its canonical name, anything
// else to "" (gated).
func resolveVerb(root *cobra.Command, first string) string {
	if builtinVerbs[first] {
		return first
	}
	if root != nil {
		if child := findChild(root, first); child != nil {
			return child.Name()
		}
	}
	return ""
}

// hookVerbs are the `<verb> <subverb>` pairs Claude Code runs as hooks.
// A hook that exits non-zero fails the turn it runs on, so a hook verb must
// never be refused over config: `resume snapshot` is documented to always
// exit 0 and is wired to the Stop hook of every session. It does not read
// the config file's settings, and the loader has already warned on stderr.
var hookVerbs = map[string]string{"resume": "snapshot"}

func configGateExempt(root *cobra.Command, args []string) bool {
	first, idx := firstNonFlag(args)
	verb := resolveVerb(root, first)
	if sub, ok := hookVerbs[verb]; ok && idx >= 0 && root != nil {
		if parent := findChild(root, first); parent != nil {
			next, _ := firstNonFlag(args[idx+1:])
			if child := findChild(parent, next); child != nil && child.Name() == sub {
				return true
			}
		}
	}
	if verb == "launch" {
		// Everything after the verb belongs to claude, so a help flag there is
		// not ours to honour.
		rest := args[idx+1:]
		return len(rest) > 0 && launchRecoveryVerbs[rest[0]]
	}
	for _, a := range args {
		if a == "--" {
			break // what follows is a positional, not a flag
		}
		if a == "--help" || a == "-h" || a == "--version" {
			return true
		}
	}
	return configRecoveryVerbs[verb]
}
