// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
)

// sweepModule declares `sweep`: the chief-of-staff ledger. It joins GitHub,
// the workers' brief directories, herdr and the desk into one JSON document
// (schema 2) and writes it where `attention list` reads it. The rules and
// threat model are docs/commands/sweep.md.
var sweepModule = module.Manifest{
	Name:      "sweep",
	Tier:      module.TierExtension,
	ConfigKey: "sweep",
	New:       newSweepCmd,
}

// sweepOpts are the flags; an unset one falls back to the [sweep] table.
type sweepOpts struct {
	owner, since, briefs string
	asJSON               bool
}

// sweepParams are the resolved settings.
type sweepParams struct {
	owner     string
	since     time.Time
	briefs    string
	reviewers []string
}

func newSweepCmd(deps module.Deps) *cobra.Command {
	var o sweepOpts
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Chief-of-staff ledger: workers, PRs, what waits on you",
		Long: `sweep reads GitHub (open and merged PRs), the workers' brief directories,
herdr and the desk, and writes the ledger (schema 2) to
$XDG_STATE_HOME/cadence/sweep.json, atomically, mode 0600. With --json it
also prints the ledger; without, a one-line summary. ` + "`attention list`" + ` reads the
file, so run sweep to refresh the queue's worker and PR rows.

There are no built-in defaults. Set owner, since, briefs and reviewers in the
[sweep] table of the config file; --owner, --since and --briefs override the
first three. A missing key exits 2 and names it.

A PR is blocked, marker-clean, waiting or draft. marker-clean is a claim, not
a clearance: it means a cadence-review marker at the PR's current head from a
reviewer in [sweep] reviewers (a claim filter, not an authenticity check, since
any session under your gh login can post any name), crit=0, green checks and a
mergeable PR. Merge only after your own review at that head.

sweep reads the desk without writing: nothing is pinned, moved, run or
skipped. If GitHub cannot be read, sweep exits 1 and leaves the last ledger
in place.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSweep(cmd, deps, o)
		},
	}
	cmd.Flags().StringVar(&o.owner, "owner", "", "GitHub account whose repos the sweep covers (default: [sweep] owner)")
	cmd.Flags().StringVar(&o.since, "since", "", "first day (YYYY-MM-DD, UTC) that counts (default: [sweep] since)")
	cmd.Flags().StringVar(&o.briefs, "briefs", "", "directory with one subdirectory per worker (default: [sweep] briefs)")
	cmd.Flags().BoolVar(&o.asJSON, "json", false, "print the ledger as JSON")
	return cmd
}

// sweepUsage is a refused setting: exit 2, usage_error under --json.
func sweepUsage(format string, args ...any) error {
	return withJSONUsageCode(WithExitCode(fmt.Errorf(format, args...), exitUsage))
}

// resolveSweepParams applies the flags over the [sweep] table and validates
// the result. Nothing is defaulted: a key set by neither is refused. One
// refusal names every missing or unusable key, so a fix takes one rerun; no
// value is echoed.
func resolveSweepParams(cfg config.Config, o sweepOpts) (sweepParams, error) {
	var problems []string
	if err := cfg.Sweep.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	var missing, missingFlags, badFlags []string
	// pick returns the flag, else the config value. A flag value is checked
	// here; a config value was checked by Validate.
	pick := func(flag, flagName, cfgVal, key string, valid func(string) bool, bad string) string {
		switch {
		case flag != "":
			if !valid(flag) {
				badFlags = append(badFlags, bad)
			}
			return flag
		case cfgVal != "":
			return cfgVal
		}
		missing = append(missing, key)
		missingFlags = append(missingFlags, flagName)
		return ""
	}
	var p sweepParams
	p.owner = pick(o.owner, "--owner", cfg.Sweep.Owner, "sweep.owner", config.ValidSweepOwner, "owner is not a GitHub account name")
	since := pick(o.since, "--since", cfg.Sweep.Since, "sweep.since", config.ValidSweepSince, "since must be a YYYY-MM-DD date")
	p.briefs = pick(o.briefs, "--briefs", cfg.Sweep.Briefs, "sweep.briefs", config.ValidSweepBriefs, "briefs must be a clean absolute path")
	if len(cfg.Sweep.Reviewers) == 0 {
		missing = append(missing, "sweep.reviewers")
	}
	p.reviewers = cfg.Sweep.Reviewers
	if len(missing) > 0 {
		problems = append(problems, sweepMissing(cfg, missing, missingFlags))
	}
	problems = append(problems, badFlags...)
	if len(problems) > 0 {
		return sweepParams{}, sweepUsage("sweep: %s", strings.Join(problems, "; "))
	}
	p.since, _ = time.Parse(time.DateOnly, since) // validated above
	return p, nil
}

// sweepMissing is the clause naming the unset keys and the flags that can
// set them, e.g. "sweep.owner, sweep.since are not set: pass --owner/--since
// or add them to the [sweep] table of <path>".
func sweepMissing(cfg config.Config, keys, flags []string) string {
	where := "config.toml"
	if p, err := config.ConfigPath(); err == nil {
		where = p
	}
	hint := ""
	if cfg.DecodeDegraded() {
		hint = " (the config file failed to decode, which may be why)"
	}
	verb, pronoun := "is", "it"
	if len(keys) > 1 {
		verb, pronoun = "are", "them"
	}
	pass := ""
	if len(flags) > 0 {
		pass = "pass " + strings.Join(flags, "/") + " or "
	}
	return fmt.Sprintf("%s %s not set: %sadd %s to the [sweep] table of %s%s", strings.Join(keys, ", "), verb, pass, pronoun, where, hint)
}
