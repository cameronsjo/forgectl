package cli

import (
	"fmt"
	"io"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// `surface launch --dry-run` and `surface close --dry-run` run every check the
// real verb runs before it writes, print what it would do, and change nothing:
// no worktree, no ledger row, no branch, no herdr or tmux call that creates or
// closes a workspace. The preview never prints the harness path, its
// arguments, its environment or a brief: surface keeps those out of anything a
// manager or a log could read, and a preview is read by both.

// ledgerRowWouldCreate is a launch preview's ledger_row for a free name. A
// taken name is refused before the preview prints.
const ledgerRowWouldCreate = "would-create"

// launchPlan is `surface launch --dry-run --json`. Additive changes only
// (ADR-0008 rule 2).
type launchPlan struct {
	// DryRun is always true: nothing was created.
	DryRun  bool   `json:"dry_run"`
	Surface string `json:"surface"`
	Name    string `json:"name"`
	// Target is the resolved directory the harness would start in. For a
	// worker it is the repository; the harness starts in the new worktree.
	Target  string `json:"target"`
	Harness string `json:"harness"`
	// RunDirectory is where a claude session would actually start: the
	// target's repository settings root when the launch would move there
	// (launch.SettingsRoot, cadence-ecosystem#608), otherwise Target. Present
	// for a plain claude launch only, mirroring `launch which --json`.
	RunDirectory string `json:"run_directory,omitempty"`
	// Worker is set for a --worktree launch.
	Worker *workerLaunchPlan `json:"worker,omitempty"`
}

// workerLaunchPlan is the worker half of a launch preview.
type workerLaunchPlan struct {
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	// BranchFrom is "local" (an existing branch), "origin" (a tracking branch
	// from origin/<branch>) or "new" (a new branch at the repository's GitHub
	// default branch head, read when the launch runs, not by the preview).
	BranchFrom string `json:"branch_from"`
	// LedgerRow is "would-create". A name already in the ledger is refused
	// before the preview prints, as the launch would refuse it.
	LedgerRow string `json:"ledger_row"`
	// Brief is true when --brief was given and passed its checks.
	Brief bool `json:"brief"`
}

// renderLaunchPlan prints the preview as key=value lines or one JSON object.
func renderLaunchPlan(out io.Writer, p launchPlan, asJSON bool) error {
	if asJSON {
		return writeJSON(out, p)
	}
	w := &stickyWriter{w: out}
	w.printf("dry_run=true\nsurface=%s\nname=%s\ntarget=%s\nharness=%s\n",
		termsafe.SafeLineMax(p.Surface, 64), termsafe.SafeLineMax(p.Name, 64),
		safeColumnPath(p.Target), termsafe.SafeLineMax(p.Harness, 64))
	if p.RunDirectory != "" {
		w.printf("run_directory=%s\n", safeColumnPath(p.RunDirectory))
	}
	if wp := p.Worker; wp != nil {
		w.printf("worktree=%s\nbranch=%s\nbranch_from=%s\nledger_row=%s\nbrief=%t\n",
			safeColumnPath(wp.Worktree), termsafe.SafeLineMax(wp.Branch, 120), wp.BranchFrom, wp.LedgerRow, wp.Brief)
	}
	return w.err
}

// closePlan is `surface close --dry-run --json`. Additive changes only.
type closePlan struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// DryRun is always true: nothing was closed or removed.
	DryRun bool `json:"dry_run"`
	// Refused is true when the real close would refuse and touch nothing.
	Refused bool `json:"refused"`
	// Workspace is "would-close", "closed-earlier", "none" or "refused". herdr
	// is not asked: whether it could be read, or still holds the workspace,
	// is known only when close runs.
	Workspace string `json:"workspace"`
	// Worktree is "would-remove", "would-keep", "gone" or "untouched".
	Worktree string `json:"worktree"`
	// KeptBecause names every removal check that failed, or --keep-worktree.
	KeptBecause []string `json:"kept_because,omitempty"`
	// WouldForget reports that the ledger row would be removed.
	WouldForget bool   `json:"would_forget"`
	Reason      string `json:"reason,omitempty"`
	Note        string `json:"note,omitempty"`
}

// renderClosePlan prints the preview, in text or JSON, whether or not close
// would refuse. A refusal is then exit 1, as the real close exits, so
// `close --dry-run && close` stops where close would. Text mode prints the
// preview lines first and the error after them, as JSON mode prints the object
// first.
func renderClosePlan(out io.Writer, p closePlan, asJSON bool) error {
	if asJSON {
		if err := writeJSON(out, p); err != nil {
			return err
		}
		if p.Refused {
			return newSilentCodedError(1)
		}
		return nil
	}
	w := &stickyWriter{w: out}
	w.printf("dry_run=true\nname=%s\nbranch=%s\nrefused=%t\nworkspace=%s\nworktree=%s\nwould_forget=%t\n",
		termsafe.SafeLineMax(p.Name, 64), termsafe.SafeLineMax(p.Branch, 120), p.Refused, p.Workspace, p.Worktree, p.WouldForget)
	for _, k := range p.KeptBecause {
		w.printf("kept: %s\n", termsafe.SafeLineMax(k, 300))
	}
	if p.Note != "" {
		w.printf("note: %s\n", termsafe.SafeLineMax(p.Note, 300))
	}
	if w.err != nil {
		return w.err
	}
	if p.Refused {
		return WithExitCode(fmt.Errorf("worker %s: close would refuse, nothing was touched: %s",
			termsafe.SafeLineMax(p.Name, 64), termsafe.SafeLineMax(p.Reason, 300)), 1)
	}
	return nil
}
