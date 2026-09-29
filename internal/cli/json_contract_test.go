package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// jsonContractExempt names the leaf commands that deliberately take no
// --json, each with the reason. Every other visible leaf MUST declare it.
// An entry here is a decision, not a to-do: add a leaf to the map only when
// it reports no state (it acts, opens a surface, serves, prints a static
// cheatsheet) or when its raw output is the payload.
//
// Flag arms are out of scope. The walk sees leaves, not flags, so a
// state-reporting arm of a mutating verb (`pr repair --history`, the
// `--dry-run` reports) is invisible to it; that is part of why #508 promoted
// `history` to a leaf. A state report that matters to agents belongs in its
// own leaf, where this walk enforces --json.
var jsonContractExempt = map[string]string{
	"bench open":             "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"bench up":               "reports no state; it changes something and the exit code is the outcome",
	"branch":                 "a dry-run/report arm of a mutating verb: a flag arm, out of scope for this walk (see the note above)",
	"clean":                  "a dry-run/report arm of a mutating verb: a flag arm, out of scope for this walk (see the note above)",
	"docker build":           "reports no state; it changes something and the exit code is the outcome",
	"docker run":             "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"docker shell":           "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"docs open":              "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"docs read":              "the raw output is the payload (a file, log, doc or shell text to eval); wrapping it in JSON would break its consumers",
	"docs serve":             "a long-running server (MCP or HTTP); it serves rather than reports",
	"env get":                "copies the value to the clipboard and prints nothing; there is no state to report and no print path by design",
	"env redact":             "the raw output is the payload (a file, log, doc or shell text to eval); wrapping it in JSON would break its consumers",
	"env set":                "reports no state; it changes something and the exit code is the outcome",
	"ghostty cheat":          "a static cheatsheet; it reports no state",
	"init":                   "reports no state; it changes something and the exit code is the outcome",
	"k8s exec":               "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"k8s inspect":            "the raw output is the payload (a file, log, doc or shell text to eval); wrapping it in JSON would break its consumers",
	"k8s logs":               "the raw output is the payload (a file, log, doc or shell text to eval); wrapping it in JSON would break its consumers",
	"launch edit":            "reports no state; it changes something and the exit code is the outcome",
	"launch init":            "reports no state; it changes something and the exit code is the outcome",
	"launch migrate":         "reports no state; it changes something and the exit code is the outcome",
	"pip remove":             "reports no state; it changes something and the exit code is the outcome",
	"pip restore":            "reports no state; it changes something and the exit code is the outcome",
	"pr attach":              "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"pr cleanup":             "reports no state; it changes something and the exit code is the outcome",
	"pr findings cleanup":    "a dry-run/report arm of a mutating verb: a flag arm, out of scope for this walk (see the note above)",
	"pr keys":                "a static cheatsheet; it reports no state",
	"pr local":               "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"pr open":                "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"pr pick":                "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"pr reviewed mark":       "reports no state; it changes something and the exit code is the outcome",
	"pr reviewed sync":       "reports no state; it changes something and the exit code is the outcome",
	"pr reviewed unmark":     "reports no state; it changes something and the exit code is the outcome",
	"pr teardown":            "reports no state; it changes something and the exit code is the outcome",
	"projects clone":         "reports no state; it changes something and the exit code is the outcome",
	"projects pick":          "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"projects worktree":      "reports no state; it changes something and the exit code is the outcome",
	"proxy off":              "the raw output is the payload (a file, log, doc or shell text to eval); wrapping it in JSON would break its consumers",
	"proxy use":              "the raw output is the payload (a file, log, doc or shell text to eval); wrapping it in JSON would break its consumers",
	"quarantine hide":        "reports no state; it changes something and the exit code is the outcome",
	"quarantine restore":     "reports no state; it changes something and the exit code is the outcome",
	"recipe afk":             "reports no state; it changes something and the exit code is the outcome",
	"resume snapshot":        "reports no state; it changes something and the exit code is the outcome",
	"review mark":            "reports no state; it changes something and the exit code is the outcome",
	"review sync":            "reports no state; it changes something and the exit code is the outcome",
	"review unmark":          "reports no state; it changes something and the exit code is the outcome",
	"surface launch":         "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"tasks mcp":              "a long-running server (MCP or HTTP); it serves rather than reports",
	"theme preview":          "a static cheatsheet; it reports no state",
	"tmux cheat":             "a static cheatsheet; it reports no state",
	"tmux kill":              "reports no state; it changes something and the exit code is the outcome",
	"tmux last":              "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"tmux pick":              "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"tmux rename":            "reports no state; it changes something and the exit code is the outcome",
	"upgrade":                "reports no state; it changes something and the exit code is the outcome",
	"workflow bless":         "reports no state; it changes something and the exit code is the outcome",
	"workflow run":           "opens or attaches an interactive surface (browser, tmux, shell, container, editor session); it reports no state",
	"workflow trust init":    "reports no state; it changes something and the exit code is the outcome",
	"workflow trust rebuild": "reports no state; it changes something and the exit code is the outcome",
	"y copy":                 "reports no state; it changes something and the exit code is the outcome",
	"y file":                 "reports no state; it changes something and the exit code is the outcome",
	"y img":                  "reports no state; it changes something and the exit code is the outcome",
	"y last":                 "the raw output is the payload and may carry secrets pasted into a shell; JSON would add a second copy for no consumer",
	"y paste":                "clipboard content may be secret and the raw output is the payload",
}

// TestStateVerbsDeclareJSON enforces ADR-0008's --json rule over the whole
// command tree. It walks every visible leaf and requires --json (own or
// inherited) or a jsonContractExempt entry with a reason, so a new verb cannot
// ship without a conscious choice. The earlier name list (list, status, ...)
// missed `version`, `k8s ns`, `ghostty themes` and `pip path` (#538).
func TestStateVerbsDeclareJSON(t *testing.T) {
	root := newRoot(module.Deps{})
	var missing []string
	seen := map[string]bool{}
	checked := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.HasSubCommands() || c == root || c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		path := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
		seen[path] = true
		hasJSON := c.Flags().Lookup("json") != nil || c.InheritedFlags().Lookup("json") != nil
		if _, ok := jsonContractExempt[path]; ok {
			if hasJSON {
				t.Errorf("leaf %q declares --json but is still in jsonContractExempt; drop the stale entry", path)
			}
			return
		}
		checked++
		if !hasJSON {
			missing = append(missing, path)
		}
	}
	walk(root)
	if checked == 0 {
		t.Fatal("no leaves found; the walk is vacuous")
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("leaf %q has no --json flag (ADR-0008); add one or list it in jsonContractExempt with a reason", m)
	}
	for path, reason := range jsonContractExempt {
		if !seen[path] {
			t.Errorf("jsonContractExempt names %q, which is not a visible leaf; drop the stale entry", path)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("jsonContractExempt[%q] has no reason", path)
		}
	}
}
