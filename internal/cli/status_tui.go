package cli

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bench"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/status"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// errStatusTUINeedsTerminal is the --tui refusal off a terminal. It points at
// the forms an agent or a script can read (ADR-0008: no TTY interaction
// without a TTY).
var errStatusTUINeedsTerminal = errors.New("status --tui needs a terminal on stdin and stdout; use status --json (or plain status) instead")

// statusTUIRuntime is the seam `status --tui` is tested through: the two
// terminal checks, the cockpit itself, and the run of a `pr <ref>` the
// operator chose in it.
type statusTUIRuntime struct {
	stdinIsTerminal  func(io.Reader) bool
	stdoutIsTerminal func(io.Writer) bool
	run              func(ctx context.Context, opts tui.CockpitOptions) (tui.Action, error)
	// runVerb hands the argv the operator chose to whatever runs it once
	// the cockpit has released the terminal.
	runVerb func(cmd *cobra.Command, th theme.Theme, argv []string) error
}

func productionStatusTUIRuntime() statusTUIRuntime {
	return statusTUIRuntime{
		stdinIsTerminal:  docsReadInputIsTerminal,
		stdoutIsTerminal: docsReadOutputIsTerminal,
		run:              tui.RunCockpit,
		runVerb:          deferHubVerb,
	}
}

// runStatusCockpit opens the cockpit over src, then performs the action the
// operator chose in it, if any.
func runStatusCockpit(cmd *cobra.Command, src statusSources, th theme.Theme, rt statusTUIRuntime, timeout time.Duration) error {
	if !rt.stdinIsTerminal(cmd.InOrStdin()) || !rt.stdoutIsTerminal(cmd.OutOrStdout()) {
		return errStatusTUINeedsTerminal
	}
	act, err := rt.run(cmd.Context(), tui.CockpitOptions{
		Sources:   statusCockpitSources(src, timeout),
		BuildArgv: hubPickerArgv(cmd.Root()),
		Theme:     th,
	})
	if err != nil {
		return err
	}
	if act.Kind != tui.ActionRunVerb || len(act.Argv) == 0 {
		return nil
	}
	return rt.runVerb(cmd, th, act.Argv)
}

// statusCockpitSources gives the cockpit one loader per section, each under
// timeout and each tracked until its source goroutine returns. Only git
// refreshes on the cockpit's timer: it reads local clones (projects
// discovery runs git through gitenv's local posture) and makes no network
// call. prs runs gh searches, and clean and bench walk the disk or probe
// services, so those refresh only when the operator asks.
func statusCockpitSources(src statusSources, timeout time.Duration) []tui.CockpitSource {
	return []tui.CockpitSource{
		statusIdxGit: {
			Name: statusSectionLabels[statusIdxGit],
			Auto: true,
			Load: statusCockpitLoad(timeout, src.Git, func(s status.Section[statusGitJSON]) tui.CockpitSection {
				return cockpitSection(statusReportJSON{Git: s}, statusIdxGit)
			}),
		},
		statusIdxPRs: {
			Name: statusSectionLabels[statusIdxPRs],
			Load: statusCockpitLoad(timeout, src.PRs, func(s status.Section[prDashJSON]) tui.CockpitSection {
				return cockpitSection(statusReportJSON{PRs: s}, statusIdxPRs)
			}),
		},
		statusIdxClean: {
			Name: statusSectionLabels[statusIdxClean],
			Load: statusCockpitLoad(timeout, src.Clean, func(s status.Section[statusCleanJSON]) tui.CockpitSection {
				return cockpitSection(statusReportJSON{Clean: s}, statusIdxClean)
			}),
		},
		statusIdxBench: {
			Name: statusSectionLabels[statusIdxBench],
			Load: statusCockpitLoad(timeout, src.Bench, func(s status.Section[bench.Report]) tui.CockpitSection {
				return cockpitSection(statusReportJSON{Bench: s}, statusIdxBench)
			}),
		},
	}
}

// statusCockpitLoad adapts one status source to a cockpit loader.
func statusCockpitLoad[T any](timeout time.Duration, src status.Source[T], convert func(status.Section[T]) tui.CockpitSection) func(context.Context) (tui.CockpitSection, <-chan struct{}) {
	return func(ctx context.Context) (tui.CockpitSection, <-chan struct{}) {
		sec, done := status.CollectTracked(ctx, timeout, src)
		return convert(sec), done
	}
}

// cockpitSnapshot converts a whole report, the same way each loader converts
// its own section.
func cockpitSnapshot(r statusReportJSON) tui.CockpitSnapshot {
	snap := tui.CockpitSnapshot{Sections: make([]tui.CockpitSection, 0, statusSectionCount)}
	for i := range statusSectionCount {
		snap.Sections = append(snap.Sections, cockpitSection(r, i))
	}
	return snap
}

// cockpitSection converts section i of r: the shared view (state, error,
// notes, headline), then the section's full row list.
func cockpitSection(r statusReportJSON, i int) tui.CockpitSection {
	v := statusSectionViews(r)[i]
	sec := tui.CockpitSection{
		Name:     v.Label,
		Error:    v.Error,
		Notes:    v.Notes,
		Headline: v.Headline,
	}
	switch v.State {
	case status.StateOK:
		sec.State = tui.CockpitOK
	case status.StateDegraded:
		sec.State = tui.CockpitDegraded
	default:
		sec.State = tui.CockpitFailed
		return sec
	}
	switch i {
	case statusIdxGit:
		sec.Rows = cockpitGitRows(*r.Git.Data)
	case statusIdxPRs:
		sec.Rows = cockpitPRRows(*r.PRs.Data)
	case statusIdxBench:
		sec.Rows = cockpitBenchRows(*r.Bench.Data)
	}
	return sec
}

// cockpitGitRows lists every local clone, in discovery order. A plain
// directory under the projects root is counted in the headline but is not a
// row: there is no working tree to show.
func cockpitGitRows(g statusGitJSON) []tui.CockpitRow {
	rows := make([]tui.CockpitRow, 0, len(g.Projects))
	for _, p := range g.Projects {
		detail := "[status unknown]"
		switch p.Status.State {
		case projects.StatusNotRepo:
			continue
		case projects.StatusOK:
			detail = p.Status.Label()
		}
		rows = append(rows, tui.CockpitRow{Kind: tui.CockpitRowProject, Label: p.Name, Detail: detail, Path: p.Path})
	}
	return rows
}

// cockpitPRRows lists the PRs awaiting you, then your open PRs, as rows that
// open `pr <ref>`, then your active reviews as plain rows.
func cockpitPRRows(d prDashJSON) []tui.CockpitRow {
	rows := make([]tui.CockpitRow, 0, len(d.AwaitingYou)+len(d.YourOpen)+len(d.ActiveReviews))
	for _, p := range d.AwaitingYou {
		rows = append(rows, tui.CockpitRow{Kind: tui.CockpitRowPR, Label: p.Ref, Detail: "awaiting you · " + p.Title, Ref: p.Ref})
	}
	for _, p := range d.YourOpen {
		rows = append(rows, tui.CockpitRow{Kind: tui.CockpitRowPR, Label: p.Ref, Detail: "yours · " + p.Title, Ref: p.Ref})
	}
	for _, a := range d.ActiveReviews {
		rows = append(rows, tui.CockpitRow{Kind: tui.CockpitRowInfo, Label: a.Ref, Detail: "active review · " + a.Phase})
	}
	return rows
}

// cockpitBenchRows is one plain row per bench component.
func cockpitBenchRows(b bench.Report) []tui.CockpitRow {
	rows := make([]tui.CockpitRow, 0, 2)
	for _, c := range []bench.Component{b.Hearth, b.Chronicle} {
		rows = append(rows, tui.CockpitRow{
			Kind:   tui.CockpitRowInfo,
			Label:  termsafe.SafeLineMax(c.Name, statusNameMaxRunes),
			Detail: termsafe.SafeLineMax(string(c.State)+" — "+c.Reason, statusReasonMaxRunes),
		})
	}
	return rows
}
