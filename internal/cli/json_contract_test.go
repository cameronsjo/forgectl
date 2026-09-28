package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// stateVerbNames are the leaf names that enumerate or report state. ADR-0008
// makes --json a MUST on those, so a verb with one of these names that does
// not declare --json is a broken promise an agent will act on (#482: exit 1,
// "Unknown flag: --json", read as "nothing there").
var stateVerbNames = map[string]bool{
	"list": true, "ls": true, "status": true, "show": true, "which": true,
	"doctor": true, "dash": true, "tree": true, "windows": true, "keys": true,
	"search": true, "check": true, "verify": true, "stats": true, "prs": true,
}

// jsonContractExempt names state-shaped verbs that deliberately take no
// --json, each with the reason. An entry here is a decision, not a to-do.
var jsonContractExempt = map[string]string{
	"pr keys": "a static tmux cheatsheet; it reports no state",
}

// TestStateVerbsDeclareJSON enforces ADR-0008's --json rule over the whole
// command tree, so a new list or status verb cannot ship without it.
func TestStateVerbsDeclareJSON(t *testing.T) {
	root := newRoot(module.Deps{})
	var missing []string
	checked := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.HasSubCommands() || c == root || !stateVerbNames[c.Name()] {
			return
		}
		path := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
		if _, ok := jsonContractExempt[path]; ok {
			return
		}
		checked++
		if c.Flags().Lookup("json") == nil && c.InheritedFlags().Lookup("json") == nil {
			missing = append(missing, path)
		}
	}
	walk(root)
	if checked == 0 {
		t.Fatal("no state verbs found; the walk is vacuous")
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("state verb %q has no --json flag (ADR-0008); add one or list it in jsonContractExempt with a reason", m)
	}
}
