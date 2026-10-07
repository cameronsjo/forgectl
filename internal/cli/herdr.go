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
	var opts organizeOpts
	cmd := &cobra.Command{
		Use:   "organize",
		Short: "Group herdr tabs into workspaces by rule, and order them",
		Long: `organize reads your herdr session and reports how it would file each tab into
a workspace and in what order. Without --apply it changes nothing. It never
closes or renames a tab.

  forgectl herdr organize             report the plan (changes nothing)
  forgectl herdr organize --explain   also show which rule caught each tab
  forgectl herdr organize --apply     make the moves and reorder tabs
  forgectl herdr organize --json      the plan as one JSON object on stdout

--apply asks no confirmation, because every move can be undone by hand. It
restores your focus afterwards, to the tab: a focused pane inside a split tab is
not put back to pane grain. Two runs at once take turns.

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
			return runHerdrOrganize(cmd, deps, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.apply, "apply", false, "make the moves (needs the cameronsjo/herdr fork); focus is restored and nothing asks first")
	cmd.Flags().BoolVar(&opts.explain, "explain", false, "show which rule caught each tab, and why unmatched tabs matched nothing")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "emit the plan (and, with --apply, the result) as one JSON object on stdout; the human report goes to stderr")
	return cmd
}

type organizeOpts struct{ explain, asJSON, apply bool }

func runHerdrOrganize(cmd *cobra.Command, deps module.Deps, opts organizeOpts) error {
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
		// Joined on "; ", not errors.Join's newline: termsafe.Error flattens a
		// newline to a literal backslash-n, which a --json consumer then reads
		// as two characters (forgectl#1087).
		msgs := make([]string, len(problems))
		for i, p := range problems {
			msgs[i] = p.Error()
		}
		return WithExitCode(termsafe.Error(errors.New(strings.Join(msgs, "; "))), 2)
	}

	if !opts.apply {
		return organizeOnce(cmd, deps, opts)
	}

	// --apply needs the lock that serializes it against other organize runs.
	// Off Unix there is none, so it refuses before any herdr call (#732).
	if !herdrLockSupported {
		return WithExitCode(errors.New("organize --apply needs a file lock to keep two runs apart, and forgectl has one only on Unix; run it without --apply for the report"), 2)
	}

	// --apply gates twice, both before any change: the session (above), then
	// the fork's `tab move`. Then it serializes against other organize runs.
	if err := herdrCheckFork(cmd.Context(), deps.Runner); err != nil {
		return WithExitCode(termsafe.Error(forkRefusal(err)), 2)
	}
	lockPath, err := herdrLockPath()
	if err != nil {
		// Nothing has changed yet: the same class as the other pre-apply setup
		// failures, exit 2.
		return WithExitCode(termsafe.Error(err), 2)
	}
	notice := func() { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), lockWaitNotice) }
	return herdrWithLock(lockPath, notice, func() error { return organizeOnce(cmd, deps, opts) })
}

// organizeOnce reads the session, plans, and either reports (dry run) or
// applies. Under --apply it runs inside the lock, so the snapshot it plans from
// already reflects any organize run that held the lock before it.
func organizeOnce(cmd *cobra.Command, deps module.Deps, opts organizeOpts) error {
	cfg := deps.Cfg.Herdr.Organize
	ctx := cmd.Context()
	client := herdr.New(deps.Runner)
	snap, err := takeSnapshot(ctx, client)
	if err != nil {
		return termsafe.Error(err)
	}
	root, err := herdrProjectsRoot()
	if err != nil {
		return termsafe.Error(err)
	}
	plan := organize.BuildPlan(toOrganizeConfig(cfg), snap, root)

	human := cmd.OutOrStdout()
	if opts.asJSON {
		human = cmd.ErrOrStderr()
	}
	// With no resolvable home, paths print in full instead of shortened to ~.
	home, homeErr := herdrUserHome()
	if homeErr != nil {
		home = ""
	}
	r := organizeReport{w: human, home: home}

	if !opts.apply {
		r.dryRun(cfg, snap, plan, opts.explain)
		if opts.asJSON {
			return writeOrganizeJSON(cmd.OutOrStdout(), plan, nil, nil)
		}
		return nil
	}

	sum := summarize(snap, plan)
	r.preamble(cfg, plan, opts.explain)
	if sum.pending == 0 && sum.reorderCount == 0 {
		r.closing(sum, snap, plan)
		if opts.asJSON {
			return writeOrganizeJSON(cmd.OutOrStdout(), plan, &applyResult{}, nil)
		}
		return nil
	}

	res := applyPlan(ctx, client, snap, plan)
	runErr := r.applied(res, sum.pending)
	if opts.asJSON {
		if err := writeOrganizeJSON(cmd.OutOrStdout(), plan, &res, runErr); err != nil {
			return err
		}
		// The object on stdout already carries runErr (forgectl#862).
		return jsonVerdict(runErr, true)
	}
	return runErr // already terminal-safe, and possibly several lines
}

// organizeConfigProblem returns why organize cannot run from the config, or
// nil. Config load is tolerant and never validates, so the command does.
func organizeConfigProblem(c config.Config) error {
	o := c.Herdr.Organize
	if err := o.Validate(); err != nil {
		return err
	}
	// A glob the matcher cannot compile matches nothing, so its tabs would all
	// fall to the default with no sign of why. Refuse it here.
	for i, r := range o.Rules {
		if err := organize.CheckGlob(r.Glob); err != nil {
			return fmt.Errorf("[[herdr.organize.rule]] #%d (glob %q): %w", i+1, r.Glob, err)
		}
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
			fmt.Fprintf(&b, ". forgectl no longer reads ~/%s. Move its keys into config.toml: default and workspace_order go under [herdr.organize], and each [[rule]] becomes [[herdr.organize.rule]].", legacyOrganizeRulesFile)
		}
	}
	if _, ok := lookupHerdrEnv(legacyOrganizeRulesEnv); ok {
		fmt.Fprintf(&b, ". %s is set, and forgectl ignores it.", legacyOrganizeRulesEnv)
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
func (r organizeReport) safe(s string) string { return safeText(s) }

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

// planSummary is what a plan asks for, counted the same way for the dry run and
// for --apply.
type planSummary struct {
	pending      int // moves that can run
	blocked      int // moves herdr will refuse
	reorders     []organize.Reorder
	reorderCount int // tab moves plus a workspace reorder
	wsOrder      []string
	wsChanged    bool
}

func summarize(snap organize.Snapshot, plan organize.Plan) planSummary {
	var sum planSummary
	for _, m := range plan.Moves {
		if m.Blocked {
			sum.blocked++
		} else {
			sum.pending++
		}
	}
	// Tab order is only knowable once no move is pending.
	if sum.pending == 0 {
		sum.reorders = organize.Reorders(snap, plan)
		for _, ro := range sum.reorders {
			sum.reorderCount += len(ro.Steps)
		}
	}
	if _, to, changed := organize.WorkspaceOrderChange(snap, plan); changed {
		sum.wsOrder, sum.wsChanged = to, true
		sum.reorderCount++
	}
	return sum
}

// preamble prints what both modes open with: the --explain block, warnings,
// blocked moves, and the unmatched summary.
func (r organizeReport) preamble(cfg config.HerdrOrganizeConfig, plan organize.Plan, explain bool) {
	if explain {
		r.explain(cfg, toOrganizeConfig(cfg), plan)
	}
	for _, warn := range plan.Warnings {
		r.printf("warning: %s\n", r.safe(warn))
	}
	for _, m := range plan.Moves {
		if m.Blocked {
			r.blocked(m)
		}
	}
	r.unmatched(cfg, plan)
}

func (r organizeReport) blocked(m organize.Move) {
	r.printf("blocked  %s  %s -> %s: %s\n", r.tab(m.Title, m.TabID), r.safe(m.From), r.safe(m.To), r.safe(m.BlockedReason))
	r.printf("         fix: open another tab in %s, or move it by hand\n", r.safe(m.From))
}

func (r organizeReport) unmatched(cfg config.HerdrOrganizeConfig, plan organize.Plan) {
	n := len(plan.Unmatched)
	if n == 0 {
		return
	}
	listed := make([]string, 0, unmatchedListed+1)
	for i, u := range plan.Unmatched {
		if i == unmatchedListed {
			listed = append(listed, fmt.Sprintf("and %d more", n-unmatchedListed))
			break
		}
		listed = append(listed, r.tab(u.Title, u.TabID))
	}
	r.printf("unmatched: %s matched no rule and go to %q: %s  (--explain shows why)\n",
		plural(n, "tab", "tabs"), r.safe(cfg.Default), strings.Join(listed, ", "))
}

// closing ends a report with the line that matches what is pending. It never
// says "organized" while anything is pending.
func (r organizeReport) closing(sum planSummary, snap organize.Snapshot, plan organize.Plan) {
	switch {
	case sum.pending > 0:
		r.printf("re-run with --apply to move them\n")
	case sum.reorderCount > 0:
		r.printf("re-run with --apply to reorder them\n")
	case sum.blocked > 0:
		r.printf("nothing to apply; %s blocked (see above)\n", plural(sum.blocked, "move is", "moves are"))
	default:
		r.printf("organized: %s in %s; nothing to do\n", plural(len(plan.Assignments), "tab", "tabs"), plural(len(snap.Workspaces), "workspace", "workspaces"))
	}
}

func (r organizeReport) dryRun(cfg config.HerdrOrganizeConfig, snap organize.Snapshot, plan organize.Plan, explain bool) {
	sum := summarize(snap, plan)
	if explain {
		r.explain(cfg, toOrganizeConfig(cfg), plan)
	}
	for _, warn := range plan.Warnings {
		r.printf("warning: %s\n", r.safe(warn))
	}
	for _, m := range plan.Moves {
		if m.Blocked {
			r.blocked(m)
			continue
		}
		r.printf("move     %s  %s -> %s  %s\n", r.tab(m.Title, m.TabID), r.safe(m.From), r.safe(m.To), r.path(m.CWD))
	}
	if sum.pending > 0 {
		r.printf("tab order will be rechecked after the moves\n")
	}
	for _, ro := range sum.reorders {
		for _, s := range ro.Steps {
			r.printf("order    %s: %s -> position %d\n", r.safe(ro.Workspace), r.tab(s.Title, s.TabID), s.Position+1)
		}
	}
	if sum.wsChanged {
		r.printf("order    workspaces: %s\n", r.safe(strings.Join(sum.wsOrder, ", ")))
	}
	r.unmatched(cfg, plan)
	r.closing(sum, snap, plan)
}

// applied reports what --apply did and returns the error the command exits
// with: nil when the run finished cleanly. A failure prints as four lines, so
// the operator can see what ran, what did not, where focus went, and how to
// finish.
func (r organizeReport) applied(res applyResult, planned int) error {
	for _, w := range res.Warnings {
		r.printf("warning: %s\n", r.safe(w))
	}
	for _, m := range res.Applied {
		r.printf("moved    %s  %s -> %s\n", r.tab(m.Title, m.TabID), r.safe(m.From), r.safe(m.To))
	}
	for _, m := range res.AlreadyPlaced {
		r.printf("in place %s  already in %s\n", r.tab(m.Title, m.TabID), r.safe(m.To))
	}
	for _, s := range res.Reordered {
		if s.TabID == "" { // a workspace step: Title is its label
			r.printf("ordered  workspace %q -> position %d\n", r.safe(s.Title), s.Position+1)
			continue
		}
		r.printf("ordered  %s -> position %d\n", r.tab(s.Title, s.TabID), s.Position+1)
	}

	if res.Err == nil {
		r.printf("applied: %s, %s\n", plural(len(res.Applied), "move", "moves"), plural(len(res.Reordered), "reorder", "reorders"))
		switch {
		case res.FocusErr != nil:
			return errors.New("could not restore focus: " + r.safe(res.FocusErr.Error()))
		case res.FocusTab != "":
			r.printf("focus restored to %s\n", r.tab(res.FocusTitle, res.FocusTab))
		}
		return nil
	}

	notRun := make([]string, 0, len(res.NotRun)+len(res.Skipped))
	for _, m := range res.NotRun {
		notRun = append(notRun, r.tab(m.Title, m.TabID))
	}
	notRun = append(notRun, res.Skipped...)
	if len(notRun) == 0 {
		notRun = append(notRun, "none")
	}
	focus := "focus was not changed"
	switch {
	case res.FocusErr != nil:
		focus = "could not restore focus: " + r.safe(res.FocusErr.Error())
	case res.FocusTab != "":
		focus = "focus restored to " + r.tab(res.FocusTitle, res.FocusTab)
	}
	first := r.safe(res.Err.Error())
	if res.Stage != "" {
		first = r.safe(res.Stage) + " failed: " + first
	}
	// The summary is several lines, so it is built from pieces that are each
	// already safe: termsafe.Error would flatten the newlines.
	return errors.New(strings.Join([]string{
		first,
		fmt.Sprintf("applied: %d of %d moves; not run: %s", len(res.Applied), planned, strings.Join(notRun, ", ")),
		focus,
		"the plan is recomputed on every run; re-run forgectl herdr organize --apply to finish",
	}, "\n"))
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

// writeOrganizeJSON encodes the plan and the run's result. res is nil for a dry
// run, which applies nothing; runErr is the error the run ended with, if any.
func writeOrganizeJSON(out io.Writer, plan organize.Plan, res *applyResult, runErr error) error {
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
	if res != nil {
		for _, m := range res.Applied {
			doc.Result.Applied = append(doc.Result.Applied, moveJSON(m))
		}
		for _, m := range res.NotRun {
			doc.Result.NotRun = append(doc.Result.NotRun, moveJSON(m))
		}
	}
	if runErr != nil {
		doc.Result.Error = runErr.Error()
	}

	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
