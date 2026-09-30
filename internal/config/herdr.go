package config

import (
	"fmt"
	"strings"
)

// HerdrConfig is the [herdr] section: settings for the `forgectl herdr`
// command group. Only [herdr.organize] exists so far.
type HerdrConfig struct {
	Organize HerdrOrganizeConfig `toml:"organize"`
}

// HerdrOrganizeConfig is [herdr.organize]: the rules `forgectl herdr organize`
// uses to group tabs into workspaces. internal/config does not import the
// planner; the CLI converts this value to the planner's own Config.
type HerdrOrganizeConfig struct {
	// Default is the workspace label for a tab no rule matches.
	Default string `toml:"default"`
	// WorkspaceOrder lists workspace labels in their desired left-to-right
	// order. A label absent from the session is skipped; a workspace not listed
	// keeps its place after the listed ones.
	WorkspaceOrder []string `toml:"workspace_order"`
	// Rules are tried in order; the first whose glob matches a pane's
	// "<cwd> :: <title>" decides the tab's workspace.
	Rules []HerdrOrganizeRule `toml:"rule"`
}

// HerdrOrganizeRule is one [[herdr.organize.rule]] block.
type HerdrOrganizeRule struct {
	Glob      string `toml:"glob"`
	Workspace string `toml:"workspace"`
}

// IsZero reports whether the section was absent or empty.
func (hc HerdrOrganizeConfig) IsZero() bool {
	return hc.Default == "" && len(hc.WorkspaceOrder) == 0 && len(hc.Rules) == 0
}

// Validate reports the first semantically invalid [herdr.organize] value. An
// absent section is valid; whether rules exist at all is the command's check,
// because organize reports that case with guidance rather than a parse error.
func (hc HerdrOrganizeConfig) Validate() error {
	if len(hc.Rules) > 0 && strings.TrimSpace(hc.Default) == "" {
		return fmt.Errorf("[herdr.organize] default is empty: name the workspace that tabs matching no rule go to")
	}
	for i, r := range hc.Rules {
		switch {
		case strings.TrimSpace(r.Glob) == "":
			return fmt.Errorf("[[herdr.organize.rule]] #%d (glob %q): glob is empty", i+1, r.Glob)
		case strings.TrimSpace(r.Workspace) == "":
			return fmt.Errorf("[[herdr.organize.rule]] #%d (glob %q): workspace is empty", i+1, r.Glob)
		}
	}
	seen := make(map[string]bool, len(hc.WorkspaceOrder))
	for i, label := range hc.WorkspaceOrder {
		switch {
		case strings.TrimSpace(label) == "":
			return fmt.Errorf("[herdr.organize] workspace_order entry #%d is empty", i+1)
		case seen[label]:
			return fmt.Errorf("[herdr.organize] workspace_order lists %q more than once", label)
		}
		seen[label] = true
	}
	return nil
}
