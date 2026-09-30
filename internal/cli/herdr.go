package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/herdr/organize"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// herdrModule declares the `herdr` command group: forgectl's native helpers
// for the herdr terminal multiplexer. It owns the [herdr] config section.
var herdrModule = module.Manifest{
	Name:      "herdr",
	Tier:      module.TierExtension,
	ConfigKey: "herdr",
	New:       newHerdrCmd,
}

// Injectable seams, in the style of recipe.go's lookupRecipeEnv, so command
// tests need no real herdr socket, home directory, or projects root.
var (
	lookupHerdrEnv    = os.LookupEnv
	herdrCheckSession = herdr.CheckSession
	herdrProjectsRoot = projects.ResolveRoot
	herdrUserHome     = os.UserHomeDir
	herdrFileExists   = func(path string) bool { _, err := os.Stat(path); return err == nil }
)

const (
	// legacyOrganizeRulesEnv and legacyOrganizeRulesFile are what the
	// forgectl-herdr script read. forgectl does not; they are named only so the
	// no-rules message can say so.
	legacyOrganizeRulesEnv  = "HERDR_ORGANIZE_RULES"
	legacyOrganizeRulesFile = ".config/herdr-organize/rules.toml"

	// titleCols is the width a tab title is clipped to in the report.
	titleCols = 30
	// unmatchedListed bounds how many unmatched tabs the summary line names.
	unmatchedListed = 5
)

func newHerdrCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "herdr",
		Short: "Helpers for the herdr terminal multiplexer",
		Long: `herdr holds forgectl's helpers for the herdr terminal multiplexer. Run them
from inside a herdr pane.

  forgectl herdr organize      group tabs into workspaces by rule and order them`,
	}
	cmd.AddCommand(newHerdrOrganizeCmd(deps))
	return cmd
}

func newHerdrOrganizeCmd(deps module.Deps) *cobra.Command {
	var explain, asJSON bool
	cmd := &cobra.Command{
		Use:   "organize",
		Short: "Group herdr tabs into workspaces by rule, and order them",
		Long: `organize reads your herdr session and reports how it would file each tab into
a workspace and in what order. It changes nothing: no tab is closed or
renamed.

  forgectl herdr organize             report the plan (changes nothing)
  forgectl herdr organize --explain   also show which rule caught each tab
  forgectl herdr organize --json      the plan as one JSON object on stdout

A tab goes to the workspace of the first rule whose glob matches
"<cwd> :: <title>" for any of its panes (first pane first); a tab no rule
matches goes to the default workspace. Within a workspace tabs are ordered by
wing, repo, then cwd, where wing and repo are the first two path parts under
the projects root ($PROJECTS_DIR, else ~/Projects). Worktree paths sort with
their repo.

Rules live in config.toml (forgectl config shows the path):

  [herdr.organize]
  default = "misc"                       # workspace for unmatched tabs
  workspace_order = ["forge", "misc"]    # left-to-right order
  [[herdr.organize.rule]]
  glob = "*/Projects/forge/* :: *"       # * matches any run, including /
  workspace = "forge"

A glob follows shell rules (* ? [abc] [!x]) and is case-sensitive. herdr will
not empty a workspace, so a move that would leave one with no tabs is reported
as blocked.

Run it inside a herdr pane. Moving tabs needs the cameronsjo/herdr fork; the
report does not.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHerdrOrganize(cmd, deps, explain, asJSON)
		},
	}
	cmd.Flags().BoolVar(&explain, "explain", false, "show which rule caught each tab, and why unmatched tabs matched nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the plan as one JSON object on stdout; the human report goes to stderr")
	return cmd
}

func runHerdrOrganize(cmd *cobra.Command, deps module.Deps, explain, asJSON bool) error {
	cfg := deps.Cfg.Herdr.Organize

	// Both preconditions are checked before any herdr call, and reported
	// together, so a first-time user outside herdr with no config fixes both
	// in one round.
	var problems []error
	if err := organizeConfigProblem(deps.Cfg); err != nil {
		problems = append(problems, err)
	}
	if err := herdrCheckSession(lookupHerdrEnv); err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return WithExitCode(termsafe.Error(errors.Join(problems...)), 2)
	}

	ctx := cmd.Context()
	snap, err := takeSnapshot(ctx, herdr.New(deps.Runner))
	if err != nil {
		return termsafe.Error(err)
	}
	root := herdrProjectsRoot()
	plan := organize.BuildPlan(toOrganizeConfig(cfg), snap, root)

	human := cmd.OutOrStdout()
	if asJSON {
		human = cmd.ErrOrStderr()
	}
	home, _ := herdrUserHome()
	r := organizeReport{w: human, home: home}
	r.dryRun(cfg, snap, plan, explain)

	if asJSON {
		return writeOrganizeJSON(cmd.OutOrStdout(), plan)
	}
	return nil
}

// organizeConfigProblem returns why organize cannot run from the config, or
// nil. Config load is tolerant and never validates, so the command does.
func organizeConfigProblem(c config.Config) error {
	o := c.Herdr.Organize
	if err := o.Validate(); err != nil {
		return err
	}
	if len(o.Rules) > 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("no rules are configured: ")
	if c.HasHerdrOrganizeSection() {
		b.WriteString("edit the [herdr.organize] section of config.toml and add [[herdr.organize.rule]] entries (forgectl config shows the path)")
	} else {
		b.WriteString("forgectl init adds a commented [herdr.organize] section; edit it (forgectl config shows the path)")
	}
	if home, err := herdrUserHome(); err == nil {
		legacy := filepath.Join(home, legacyOrganizeRulesFile)
		if herdrFileExists(legacy) {
			fmt.Fprintf(&b, "\nforgectl no longer reads ~/%s. Move its keys into config.toml: default and workspace_order go under [herdr.organize], and each [[rule]] becomes [[herdr.organize.rule]].", legacyOrganizeRulesFile)
		}
	}
	if _, ok := lookupHerdrEnv(legacyOrganizeRulesEnv); ok {
		fmt.Fprintf(&b, "\n%s is set, and forgectl ignores it.", legacyOrganizeRulesEnv)
	}
	return errors.New(b.String())
}

func toOrganizeConfig(o config.HerdrOrganizeConfig) organize.Config {
	cfg := organize.Config{Default: o.Default, WorkspaceOrder: o.WorkspaceOrder}
	for _, r := range o.Rules {
		cfg.Rules = append(cfg.Rules, organize.Rule{Glob: r.Glob, Workspace: r.Workspace})
	}
	return cfg
}

// takeSnapshot reads the session once: workspaces, each one's tabs, and every
// pane, in herdr's own list order.
func takeSnapshot(ctx context.Context, c *herdr.Client) (organize.Snapshot, error) {
	wss, err := c.Workspaces(ctx)
	if err != nil {
		return organize.Snapshot{}, err
	}
	panes, err := c.Panes(ctx)
	if err != nil {
		return organize.Snapshot{}, err
	}
	snap := organize.Snapshot{Workspaces: wss, Panes: panes, Tabs: make(map[string][]herdr.Tab, len(wss))}
	for _, w := range wss {
		tabs, err := c.Tabs(ctx, w.WorkspaceID)
		if err != nil {
			return organize.Snapshot{}, err
		}
		snap.Tabs[w.WorkspaceID] = tabs
	}
	return snap, nil
}

// organizeReport renders a plan as human text.
type organizeReport struct {
	w    io.Writer
	home string
}

func (r organizeReport) printf(format string, a ...any) { _, _ = fmt.Fprintf(r.w, format, a...) }

// safe clips and neutralizes text herdr reports (titles and cwds are chosen by
// whatever runs in a pane, so they are untrusted).
func (r organizeReport) safe(s string) string { return termsafe.SafeLine(s) }

func (r organizeReport) tab(title, id string) string {
	return `"` + truncate(r.safe(title), titleCols) + `" [` + r.safe(id) + `]`
}

func (r organizeReport) path(p string) string {
	if r.home != "" && (p == r.home || strings.HasPrefix(p, r.home+"/")) {
		p = "~" + strings.TrimPrefix(p, r.home)
	}
	return r.safe(p)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (r organizeReport) dryRun(cfg config.HerdrOrganizeConfig, snap organize.Snapshot, plan organize.Plan, explain bool) {
	oc := toOrganizeConfig(cfg)
	if explain {
		r.explain(cfg, oc, plan)
	}
	for _, warn := range plan.Warnings {
		r.printf("warning: %s\n", r.safe(warn))
	}

	pending, blocked := 0, 0
	for _, m := range plan.Moves {
		if m.Blocked {
			blocked++
			r.printf("blocked  %s  %s -> %s: %s\n", r.tab(m.Title, m.TabID), r.safe(m.From), r.safe(m.To), m.BlockedReason)
			r.printf("         fix: open another tab in %s, or move it by hand\n", r.safe(m.From))
			continue
		}
		pending++
		r.printf("move     %s  %s -> %s  %s\n", r.tab(m.Title, m.TabID), r.safe(m.From), r.safe(m.To), r.path(m.CWD))
	}

	reorders := 0
	if pending == 0 {
		for _, ro := range organize.Reorders(snap, plan) {
			for _, s := range ro.Steps {
				reorders++
				r.printf("order    %s: %s -> position %d\n", r.safe(ro.Workspace), r.tab(s.Title, s.TabID), s.Position+1)
			}
		}
	} else {
		r.printf("tab order will be rechecked after the moves\n")
	}
	if _, to, changed := organize.WorkspaceOrderChange(snap, plan); changed {
		reorders++
		r.printf("order    workspaces: %s\n", r.safe(strings.Join(to, ", ")))
	}

	if n := len(plan.Unmatched); n > 0 {
		listed := make([]string, 0, unmatchedListed+1)
		for i, u := range plan.Unmatched {
			if i == unmatchedListed {
				listed = append(listed, fmt.Sprintf("and %d more", n-unmatchedListed))
				break
			}
			listed = append(listed, r.tab(u.Title, u.TabID))
		}
		verb := "matched no rule and go to"
		r.printf("unmatched: %s %s %q: %s  (--explain shows why)\n", plural(n, "tab", "tabs"), verb, r.safe(cfg.Default), strings.Join(listed, ", "))
	}

	switch {
	case pending > 0:
		r.printf("re-run with --apply to move them\n")
	case reorders > 0:
		r.printf("re-run with --apply to reorder them\n")
	case blocked > 0:
		r.printf("nothing to apply; %s blocked (see above)\n", plural(blocked, "move is", "moves are"))
	default:
		r.printf("organized: %s in %s; nothing to do\n", plural(len(plan.Assignments), "tab", "tabs"), plural(len(snap.Workspaces), "workspace", "workspaces"))
	}
}

// explain prints, for every tab, the rule that caught it and where it sits and
// goes, then a per-rule tally and any rule workspace workspace_order omits.
func (r organizeReport) explain(cfg config.HerdrOrganizeConfig, oc organize.Config, plan organize.Plan) {
	for _, a := range plan.Assignments {
		r.printf("%s  %s\n", r.tab(a.Title, a.TabID), r.path(a.CWD))
		if a.Rule >= 0 {
			r.printf("    rule %d %q -> %s  (in %s)\n", a.Rule+1, r.safe(cfg.Rules[a.Rule].Glob), r.safe(a.To), r.safe(a.From))
			continue
		}
		r.printf("    no rule matched -> %s (default)  (in %s)\n", r.safe(a.To), r.safe(a.From))
		r.printf("    key: %q\n", r.safe(a.Key))
	}
	for i, rule := range cfg.Rules {
		r.printf("rule %d %q -> %s: %s\n", i+1, r.safe(rule.Glob), r.safe(rule.Workspace), plural(plan.RuleHits[i], "tab", "tabs"))
	}
	for _, label := range organize.MissingFromOrder(oc) {
		r.printf("warning: workspace %q is named by a rule but is not in workspace_order\n", r.safe(label))
	}
}

// The JSON wire shape (ADR-0008). Field names are pinned by a golden test.
type organizeJSON struct {
	Plan   organizePlanJSON   `json:"plan"`
	Result organizeResultJSON `json:"result"`
}

type organizePlanJSON struct {
	Moves     []organizeMoveJSON      `json:"moves"`
	Layout    []organizeLayoutJSON    `json:"layout"`
	Unmatched []organizeUnmatchedJSON `json:"unmatched"`
	Warnings  []string                `json:"warnings"`
}

type organizeMoveJSON struct {
	TerminalID    string `json:"terminal_id"`
	TabID         string `json:"tab_id"`
	Title         string `json:"title"`
	CWD           string `json:"cwd"`
	From          string `json:"from"`
	To            string `json:"to"`
	Blocked       bool   `json:"blocked"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}

type organizeLayoutJSON struct {
	Workspace string                  `json:"workspace"`
	Tabs      []organizeLayoutTabJSON `json:"tabs"`
}

type organizeLayoutTabJSON struct {
	TerminalID string `json:"terminal_id"`
	Title      string `json:"title"`
}

type organizeUnmatchedJSON struct {
	TerminalID string `json:"terminal_id"`
	TabID      string `json:"tab_id"`
	Title      string `json:"title"`
	CWD        string `json:"cwd"`
	Key        string `json:"key"`
}

// organizeResultJSON is what a run did. A dry run applies nothing; Blocked is
// the moves herdr will refuse.
type organizeResultJSON struct {
	Applied []organizeMoveJSON `json:"applied"`
	Blocked []organizeMoveJSON `json:"blocked"`
	NotRun  []organizeMoveJSON `json:"not_run"`
	Error   string             `json:"error"`
}

func moveJSON(m organize.Move) organizeMoveJSON {
	return organizeMoveJSON{
		TerminalID: m.TerminalID, TabID: m.TabID, Title: m.Title, CWD: m.CWD,
		From: m.From, To: m.To, Blocked: m.Blocked, BlockedReason: m.BlockedReason,
	}
}

func writeOrganizeJSON(out io.Writer, plan organize.Plan) error {
	doc := organizeJSON{
		Plan: organizePlanJSON{
			Moves: []organizeMoveJSON{}, Layout: []organizeLayoutJSON{},
			Unmatched: []organizeUnmatchedJSON{}, Warnings: []string{},
		},
		Result: organizeResultJSON{Applied: []organizeMoveJSON{}, Blocked: []organizeMoveJSON{}, NotRun: []organizeMoveJSON{}},
	}
	for _, m := range plan.Moves {
		doc.Plan.Moves = append(doc.Plan.Moves, moveJSON(m))
		if m.Blocked {
			doc.Result.Blocked = append(doc.Result.Blocked, moveJSON(m))
		}
	}
	for _, lw := range plan.Layout.Workspaces {
		l := organizeLayoutJSON{Workspace: lw.Label, Tabs: []organizeLayoutTabJSON{}}
		for _, t := range lw.Tabs {
			l.Tabs = append(l.Tabs, organizeLayoutTabJSON{TerminalID: t.TerminalID, Title: t.Title})
		}
		doc.Plan.Layout = append(doc.Plan.Layout, l)
	}
	for _, u := range plan.Unmatched {
		doc.Plan.Unmatched = append(doc.Plan.Unmatched, organizeUnmatchedJSON{
			TerminalID: u.TerminalID, TabID: u.TabID, Title: u.Title, CWD: u.CWD, Key: u.Key,
		})
	}
	doc.Plan.Warnings = append(doc.Plan.Warnings, plan.Warnings...)

	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
