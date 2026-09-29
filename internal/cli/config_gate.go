// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"

	"github.com/cameronsjo/forgectl/internal/config"
)

// configRecoveryVerbs are the top-level verbs that still run on a config.toml
// that does not parse, because they are how an operator finds and repairs it:
// config and doctor report the parse error as a finding, init rewrites the
// file, and help/completion/man/version never read configuration at all.
var configRecoveryVerbs = map[string]bool{
	"config": true, "init": true, "doctor": true, "version": true,
	"help": true, "completion": true, "man": true,
	"__complete": true, "__completeNoDesc": true,
}

// launchRecoveryVerbs is the same carve-out for `forgectl launch <verb>`.
// which/stats/migrate are absent on purpose: they would report against
// defaults the operator did not choose.
var launchRecoveryVerbs = map[string]bool{
	"init": true, "edit": true, "doctor": true,
	"help": true, "--help": true, "-h": true,
}

// configParseGate turns a config.toml that exists but does not parse into a
// hard error for every command that would otherwise run against silent
// defaults (forgectl#653). The default log level is off, so the loader's WARN
// was invisible and `docs list --json` exited 0 against the wrong roots. An
// absent file is not an error, and neither is an unreadable one: only a parse
// failure reaches here. Recovery verbs and help/version flags are exempt.
func configParseGate(cfg config.Config, args []string) error {
	parseErr := cfg.DecodeError()
	if parseErr == nil || configGateExempt(args) {
		return nil
	}
	return fmt.Errorf("%w; fix the file, or run `forgectl config` or `forgectl doctor` to inspect it", parseErr)
}

func configGateExempt(args []string) bool {
	first, idx := firstNonFlag(args)
	if first == "launch" || first == "cl" {
		// Everything after the verb belongs to claude, so a help flag there is
		// not ours to honour.
		rest := args[idx+1:]
		return len(rest) > 0 && launchRecoveryVerbs[rest[0]]
	}
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "--version" {
			return true
		}
	}
	return configRecoveryVerbs[first]
}
