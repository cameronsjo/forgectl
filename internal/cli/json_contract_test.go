package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
)

// jsonContractExempt names the leaf commands that deliberately take no
// --json, each with the reason. Every other visible leaf (and every parent that also runs) MUST declare it.
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
	"bench open":             "opens a hearth or grafana UI in the browser; nothing to report (`bench status --json` reports the services)",
	"bench up":               "brings the bench services up; the exit code is the outcome and `bench status --json` reports the result",
	"docker build":           "runs a build and streams docker's own output; the exit code is the outcome",
	"docker run":             "runs the image with the terminal wired through; the container's output is the payload",
	"docker shell":           "opens an interactive shell in the image; nothing to report",
	"docs open":              "points the browser at a doc on the running reader; nothing to report (`docs list --json` enumerates docs)",
	"docs read":              "the doc body is the payload, rendered by mdroll or the HTML reader; wrapping it in JSON would break its consumers",
	"docs serve":             "a long-running loopback HTTP server; it serves rather than reports",
	"env get":                "copies the value to the clipboard and prints nothing by design; there is no print path to wrap",
	"env redact":             "prints the file with values and comments masked; the redacted text is the payload (`env keys --json` reports key names)",
	"env set":                "writes a value read from stdin, a prompt or the clipboard; the exit code is the outcome and no value is ever echoed",
	"ghostty cheat":          "a static keybind cheatsheet parsed from ghostty; it reports no forgectl state",
	"init":                   "scaffolds config.toml templates; the exit code is the outcome (`config --json` prints the effective configuration)",
	"k8s exec":               "kubectl exec with the terminal wired through; the remote command's output is the payload",
	"k8s inspect":            "streams kubectl describe, get -o wide and events; kubectl's own output is the payload",
	"k8s logs":               "streams terminal-safe kubectl logs; the log stream is the payload",
	"launch":                 "a bare launch starts a session in the terminal; nothing to report (`launch which --json` and `launch stats --json` report state)",
	"launch edit":            "opens config.toml in $EDITOR; nothing to report",
	"launch init":            "scaffolds the [launch] section into config.toml; the exit code is the outcome",
	"launch migrate":         "imports a claunch.conf into config.toml; the exit code is the outcome",
	"pip remove":             "comments out a pip.conf entry; the exit code is the outcome (`pip show --json` reports the file)",
	"pip restore":            "un-comments what remove last tagged; the exit code is the outcome (`pip show --json` reports the file)",
	"pr":                     "a bare `pr <ref>` starts a clean-room review; nothing to report (`pr list --json` enumerates sessions)",
	"pr attach":              "jumps the terminal to a review window; nothing to report (`pr list --json` enumerates sessions)",
	"pr cleanup":             "discards a day's review sessions; the exit code is the outcome (`pr history --json` reads back the audit log)",
	"pr keys":                "a static tmux cheatsheet; it reports no state",
	"pr local":               "starts an offline clean-room review of local commits; the exit code is the outcome",
	"pr open":                "opens a shell window in the clean-room workspace; nothing to report",
	"pr pick":                "an interactive multiselect that spins up reviews for the selection; `pr list --json` reports what exists",
	"pr reviewed mark":       "records a reviewed mark; the exit code is the outcome",
	"pr reviewed sync":       "prunes reviewed marks for closed PRs; the exit code is the outcome",
	"pr reviewed unmark":     "clears a reviewed mark; the exit code is the outcome",
	"pr teardown":            "discards one review session or queue entry; the exit code is the outcome (`pr history --json` reads back the audit log)",
	"projects":               "a bare `projects` runs the interactive picker; `projects list --json` enumerates projects",
	"projects clone":         "clones a project or an org tree; the printed path is the payload (cd $(…)), and the exit code is the outcome",
	"projects pick":          "an interactive picker that opens the project in tmux; `projects list --json` enumerates projects",
	"projects worktree":      "initializes a bare-repo worktree layout; the printed path is the payload (cd $(…)), and the exit code is the outcome",
	"proxy off":              "emits shell unsets for the caller to eval; the shell text is the payload",
	"proxy use":              "emits shell exports for the caller to eval; the shell text is the payload",
	"quarantine":             "the bare form is Hide, a mutation; the exit code is the outcome (`quarantine status --json` reports the state)",
	"quarantine hide":        "renames AI-instruction files aside and prints the per-file move list; the `--dry-run` arm is a flag arm, out of scope (`quarantine status --json` reports the state)",
	"quarantine restore":     "renames quarantined files back and prints the per-file move list; the `--dry-run` arm is a flag arm, out of scope (`quarantine status --json` reports the state)",
	"recipe afk":             "journals and compacts the current Herdr pane; the exit code is the outcome",
	"resume":                 "a bare `resume` picks a session and resumes it; `resume ls --json` lists sessions",
	"resume restart":         "stops and resumes sessions in their panes, one progress line per state change; the exit code is the outcome (`resume outdated --json` reports the sessions it acts on)",
	"resume snapshot":        "writes a snapshot of what a live session's exit would destroy; the exit code is the outcome (`resume ls --json` lists sessions)",
	"review mark":            "records a reviewed mark; the exit code is the outcome",
	"review sync":            "prunes reviewed marks for closed work items; the exit code is the outcome",
	"review unmark":          "clears a reviewed mark; the exit code is the outcome",
	"surface launch":         "starts a harness in a new managed surface; the exit code is the outcome",
	"tasks mcp":              "a long-running MCP server (stdio or streamable HTTP); it serves rather than reports",
	"theme preview":          "renders a live palette to the terminal for the eye; `theme show --json` reports the resolved roles",
	"tmux":                   "a bare `tmux` runs the interactive session picker; `tmux ls --json` reports sessions",
	"tmux cheat":             "a static tmux terminology and key cheatsheet; it reports no state",
	"tmux kill":              "kills a session; the exit code is the outcome (`tmux ls --json` reports sessions)",
	"tmux last":              "jumps to the last-used session; nothing to report",
	"tmux pick":              "connects to or smart-creates a session through sesh; nothing to report",
	"tmux rename":            "renames a session; the exit code is the outcome",
	"upgrade":                "updates through the Homebrew tap and exits with the outcome; the `--check` arm is a state flag arm, out of scope here, and `doctor --json` already reports the forgectl version check",
	"workflow bless":         "signs a workflow file's bytes with a user-presence signature; the exit code is the outcome (`workflow verify --json` reports blessing state)",
	"workflow run":           "an executor that runs steps and streams their output; `--dry-run` prints the plan (a flag arm, out of scope), while `workflow status --json` and `workflow list --json` report state",
	"workflow trust init":    "establishes the machine as trust anchor; the exit code is the outcome (`workflow trust list --json` reports the store)",
	"workflow trust rebuild": "rebuilds the trust store from the installed anchor; the exit code is the outcome (`workflow trust list --json` reports the store)",
	"y copy":                 "copies stdin to the clipboard; the exit code is the outcome",
	"y file":                 "puts a file reference on the clipboard; the exit code is the outcome",
	"y img":                  "puts decoded image data on the clipboard; the exit code is the outcome",
	"y last":                 "prints recent shell commands, which may carry secrets typed inline; the raw lines are the payload and a JSON copy adds no consumer",
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
		// A parent that also runs (launch, pr, projects, quarantine, resume,
		// tmux) is a leaf-like verb in its own right, so it is checked too.
		// A pure grouping parent, with no Run/RunE of its own, is not.
		if c == root || c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			return
		}
		if c.HasSubCommands() && c.Run == nil && c.RunE == nil {
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
