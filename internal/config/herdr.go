package config

import (
	"fmt"
	"strings"
	"unicode"
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
	if err := checkLabel(hc.Default); err != nil {
		return fmt.Errorf("[herdr.organize] default %q: %w", hc.Default, err)
	}
	for i, r := range hc.Rules {
		switch {
		case strings.TrimSpace(r.Glob) == "":
			return fmt.Errorf("[[herdr.organize.rule]] #%d (glob %q): glob is empty", i+1, r.Glob)
		case strings.TrimSpace(r.Workspace) == "":
			return fmt.Errorf("[[herdr.organize.rule]] #%d (glob %q): workspace is empty", i+1, r.Glob)
		}
		if err := checkLabel(r.Workspace); err != nil {
			return fmt.Errorf("[[herdr.organize.rule]] #%d (glob %q): workspace %q: %w", i+1, r.Glob, r.Workspace, err)
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
		if err := checkLabel(label); err != nil {
			return fmt.Errorf("[herdr.organize] workspace_order entry #%d %q: %w", i+1, label, err)
		}
		seen[label] = true
	}
	return nil
}

// checkLabel refuses a workspace label herdr's client would refuse at apply
// time: one that starts with '-' (read as a flag) or holds a control
// character. Catching it here means the dry run cannot promise a move that
// --apply then fails halfway through.
func checkLabel(label string) error {
	if strings.HasPrefix(label, "-") {
		return fmt.Errorf("a label must not start with '-'")
	}
	if strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return fmt.Errorf("a label must not contain control characters")
	}
	return nil
}
