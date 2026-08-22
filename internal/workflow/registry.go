package workflow

import "sort"

// RegistryExportNames returns the merged registry's complete export vocabulary
// in deterministic order. Several verbs may intentionally produce the same
// name (worktree and clone both export workspace); the vocabulary contains
// that name once. Callers use this shared view to reserve the execution
// Context namespace even when an exporting verb is absent from one workflow.
func RegistryExportNames(registry StepRegistry) []string {
	names := make(map[string]struct{})
	for _, def := range registry {
		for _, name := range def.Exports {
			names[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
