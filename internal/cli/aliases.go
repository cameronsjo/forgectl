package cli

import "github.com/spf13/cobra"

// applyAliases sets each subcommand's Cobra aliases from a canonical-verb →
// aliases map — the single source of truth per module. It replaces the seven
// per-module clones (tmux/projects/launch/workflow/bench/docker/y) with one
// helper. Two tokens are skipped: "-" (tmux last's shorthand — an
// argv-normalization spelling handled in dispatch.go, not a valid standalone
// Cobra command name) and the subcommand's own name (a self-alias is noise).
func applyAliases(parent *cobra.Command, subAliases map[string][]string) {
	for _, sub := range parent.Commands() {
		var valid []string
		for _, alias := range subAliases[sub.Name()] {
			if alias == "-" || alias == sub.Name() {
				continue
			}
			valid = append(valid, alias)
		}
		if len(valid) > 0 {
			sub.Aliases = valid
		}
	}
}
