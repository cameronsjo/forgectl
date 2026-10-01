package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// menuTextMaxRunes caps a free-text header value (the project name) in
// `menu` output, escaped first, as status caps its free text.
const menuTextMaxRunes = 200

// newMenuCmd is `forgectl menu` (forgectl#730 item 5): the hub's contents
// without a TTY. It reads exactly what bare `forgectl` reads — the same
// header sources under the same budget, the same shell-history ranking, the
// same rows from collectHubSections — and prints them. It never runs a row
// and never opens a screen, so an agent gets the hub under ADR-0008.
//
// Like version, it is host plumbing registered outside the module registry:
// it describes the hub rather than being a row on it.
func newMenuCmd(deps module.Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "menu",
		Short: "Print the hub's contents: status, pinned, recent, and every command",
		Long: `Print what bare forgectl shows, without opening it: the status line, the
pinned commands, the recent ones, and every other command with its subverbs.
Nothing is run. --json emits the same content as one document.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sec, header := gatherMenu(cmd.Context(), deps, cmd.Root())
			if asJSON {
				return menuEncoder(cmd.OutOrStdout()).Encode(menuDocument(sec, header))
			}
			return writeMenuText(cmd.OutOrStdout(), menuDocument(sec, header), header)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"header","first_run","pinned","recent","commands"} to stdout (see docs/commands/menu.md)`)
	return cmd
}

// menuEncoder is the terminal-safe encoder `menu --json` writes through, with
// HTML escaping off so a usage line reads "pr <ref>" rather than
// "pr \u003cref\u003e" (as `launch stats --json` does).
func menuEncoder(w io.Writer) *json.Encoder {
	enc := termsafe.JSONEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// gatherMenu reads the hub's live state: its sections and its header.
func gatherMenu(ctx context.Context, deps module.Deps, root *cobra.Command) (hubSections, tui.HubHeader) {
	if ctx == nil {
		ctx = context.Background()
	}
	var client *tmux.Client
	if deps.Runner != nil {
		client = tmux.New(deps.Runner)
	}
	header, entries := hubState(ctx, deps, client)
	return collectHubSections(root, configFilePresent(), recentCommands(root, entries, hubRecentLimit)), header
}

// menuJSON is the `menu --json` shape. Every key is always present; fields
// may be added, existing ones do not change (ADR-0008).
type menuJSON struct {
	Header   menuHeaderJSON `json:"header"`
	FirstRun bool           `json:"first_run"`
	Pinned   []menuRowJSON  `json:"pinned"`
	Recent   []menuRowJSON  `json:"recent"`
	Commands []menuRowJSON  `json:"commands"`
}

// menuHeaderJSON is the hub's status line as data. A field whose source was
// unavailable (or missed the budget) is null, never a confident zero.
type menuHeaderJSON struct {
	Project      *string          `json:"project"`
	Branch       *string          `json:"branch"`
	TmuxSessions *int             `json:"tmux_sessions"`
	Reviews      *menuReviewsJSON `json:"reviews"`
}

type menuReviewsJSON struct {
	Running int `json:"running"`
	Queued  int `json:"queued"`
}

// menuRowJSON is one hub row or drill-down leaf. Argv is the command path
// (what to run, after `forgectl`); when NeedsArgs is true, Usage names the
// positional to append. Usage also names optional positionals, which
// NeedsArgs does not count. Leaves are the subverbs the hub's drill-down
// shows.
type menuRowJSON struct {
	Command     string        `json:"command"`
	Argv        []string      `json:"argv"`
	Description string        `json:"description"`
	Usage       string        `json:"usage"`
	NeedsArgs   bool          `json:"needs_args"`
	Leaves      []menuRowJSON `json:"leaves"`
}

// menuDocument converts the hub's sections and header to the wire shape.
func menuDocument(sec hubSections, header tui.HubHeader) menuJSON {
	doc := menuJSON{
		Header:   menuHeader(header),
		FirstRun: sec.firstRun,
		Pinned:   []menuRowJSON{},
		Recent:   []menuRowJSON{},
		Commands: []menuRowJSON{},
	}
	for _, e := range sec.pinned {
		doc.Pinned = append(doc.Pinned, menuModuleRow(e))
	}
	for _, e := range sec.recent {
		doc.Recent = append(doc.Recent, menuRecentRow(e))
	}
	for _, e := range sec.rest {
		doc.Commands = append(doc.Commands, menuModuleRow(e))
	}
	return doc
}

// menuHeader applies tui.HubHeader's availability rules: an empty project
// omits both project and branch, a negative count is not a count.
func menuHeader(h tui.HubHeader) menuHeaderJSON {
	var out menuHeaderJSON
	if project := termsafe.SafeLineMax(h.Project, menuTextMaxRunes); project != "" {
		out.Project = &project
		if branch := termsafe.SafeLineMax(h.Branch, menuTextMaxRunes); branch != "" {
			out.Branch = &branch
		}
	}
	if h.HasTmux && h.TmuxSessions >= 0 {
		n := h.TmuxSessions
		out.TmuxSessions = &n
	}
	if h.HasReviews && h.ReviewsRunning >= 0 && h.ReviewsQueued >= 0 {
		out.Reviews = &menuReviewsJSON{Running: h.ReviewsRunning, Queued: h.ReviewsQueued}
	}
	return out
}

// menuModuleRow is a module row: its own path is its name.
func menuModuleRow(e tui.HubEntry) menuRowJSON {
	argv := []string{e.Name}
	return menuRowJSON{
		Command:     e.Name,
		Argv:        argv,
		Description: e.Short,
		Usage:       menuUsage(nil, e.Use),
		NeedsArgs:   usageRequiresArg(e.Use),
		Leaves:      menuLeaves(argv, e.Leaves),
	}
}

// menuRecentRow is a recent row: a resolved command path, never history text.
func menuRecentRow(e tui.HubEntry) menuRowJSON {
	argv := append([]string(nil), e.Argv...)
	var parents []string
	if len(argv) > 0 {
		parents = argv[:len(argv)-1]
	}
	return menuRowJSON{
		Command:     strings.Join(argv, " "),
		Argv:        argv,
		Description: e.Short,
		Usage:       menuUsage(parents, e.Use),
		NeedsArgs:   usageRequiresArg(e.Use),
		Leaves:      []menuRowJSON{},
	}
}

// menuLeaves converts a drill-down list under prefix. The synthetic self leaf
// is not a subverb — it is the parent's own invocation, which the parent row's
// Usage already says — so it is left out.
func menuLeaves(prefix []string, leaves []tui.HubLeaf) []menuRowJSON {
	out := []menuRowJSON{}
	for _, l := range leaves {
		if l.Self {
			continue
		}
		argv := append(append([]string(nil), prefix...), l.Name)
		out = append(out, menuRowJSON{
			Command:     strings.Join(argv, " "),
			Argv:        argv,
			Description: l.Short,
			Usage:       menuUsage(prefix, l.Use),
			NeedsArgs:   usageRequiresArg(l.Use),
			Leaves:      menuLeaves(argv, l.Leaves),
		})
	}
	return out
}

// usageRequiresArg reports whether a Use line names a required positional: a
// <…> group that is not inside an optional [...] one. `pr <ref>` and
// `projects worktree <query> [branch]` require one; `docs list [dir|file ...]`
// and `pr reviewed [<ref>]` do not. That is needs_args: whether running the
// row's argv alone is a usage error, not whether it can take an argument.
func usageRequiresArg(use string) bool {
	_, rest, _ := strings.Cut(use, " ")
	depth := 0
	for _, r := range rest {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case '<':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// menuUsage is the full invocation a row's Use line describes:
// "forgectl pr findings <ref>".
func menuUsage(parents []string, use string) string {
	parts := append([]string{meta.AppName}, parents...)
	return strings.Join(append(parts, use), " ")
}

// writeMenuText is `menu`'s human form: the hub's status line when there is
// one (header.Line, escaped and capped as the hub draws it),
// then each section under its heading, one row per line — usage, then
// description — with each leaf indented under its row. Plain text only, so it
// stays line-oriented and grep-safe off a TTY (ADR-0008 rule 5).
func writeMenuText(w io.Writer, doc menuJSON, header tui.HubHeader) error {
	var b strings.Builder
	if line := header.Line(); line != "" {
		b.WriteString(meta.AppName + " · " + line + "\n\n")
	}
	if doc.FirstRun {
		_, _ = fmt.Fprintf(&b, "%s init  %s\n\n", meta.AppName, hubInitShort)
	}
	sections := []struct {
		title string
		rows  []menuRowJSON
	}{
		{"pinned", doc.Pinned},
		{"recent", doc.Recent},
		{fmt.Sprintf("all commands (%d)", len(doc.Commands)), doc.Commands},
	}
	first := true
	for _, s := range sections {
		if len(s.rows) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		b.WriteString(s.title + "\n")
		writeMenuRows(&b, s.rows, 1)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeMenuRows(b *strings.Builder, rows []menuRowJSON, depth int) {
	for _, r := range rows {
		_, _ = fmt.Fprintf(b, "%s%s  %s\n", strings.Repeat("  ", depth), r.Usage, r.Description)
		writeMenuRows(b, r.Leaves, depth+1)
	}
}
