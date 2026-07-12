// Package step defines the neutral step contract for workflow verbs: the
// runner signature, the per-verb definition (runner + exports), and the
// registry shape. It lives below both the workflow engine and the domain
// modules that contribute verbs, so a module can register a step without
// importing internal/workflow and the engine never imports modules. Imports
// are deliberately limited to internal/exec + stdlib.
package step

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// ErrNotYetWired is returned by a step runner that is registered but not yet
// implemented (launch, collect — full clean-room execution is the follow-on,
// per the design doc's spike scope).
var ErrNotYetWired = errors.New("step not yet wired")

// Runner executes one resolved PlanStep against wctx, using run for any
// process it needs to shell out to. It may call wctx.Set to export variables
// for later steps (worktree exports ${workspace}, launch exports ${review}).
type Runner func(ctx context.Context, run exec.Runner, wctx *Context, s PlanStep) error

// Def is a step verb's definition: the runner that executes it, the variables
// it exports into the Context, and the fields whose values must never carry a
// CLI param in a blessed workflow. Declaring all three together keeps a verb's
// execution, its plan-time exports, and its injection surface from drifting
// apart — adding a verb is one registry entry.
type Def struct {
	Runner  Runner
	Exports []string

	// GuardedFields names this verb's param-hostile fields by their PlanStep Go
	// field name ("Cmd", "Args", "Globs", "Skill", …). A blessing signs a
	// workflow FILE's bytes, but ${} references interpolate at run time — so a
	// field whose value drives execution or enforces a security control must not
	// be runtime-injectable, or the human blessed one thing and the agent runs
	// another. `workflow bless` refuses any ${param} in these fields (only an
	// earlier step's export, which is step-produced rather than CLI-supplied, is
	// allowed).
	//
	// The rule when adding a verb: if a field's value chooses WHAT RUNS (run's
	// cmd/args), WHAT A LAUNCHED AGENT DOES (launch's skill/mode/posture), or
	// WHAT A SECURITY CONTROL COVERS (strip's globs — the clean-room redaction
	// list), it is guarded. A field that merely names DATA (worktree's repo/ref)
	// is NOT guarded: `--param repo=owner/x` is the intended parameterization,
	// and the sandbox plus the strip-list — not the blessing — are what contain
	// whatever that repo holds.
	//
	// A name here that is not a real step field is a hard error at bless time,
	// never a silent skip: a typo must not quietly disable the guard.
	GuardedFields []string
}

// Registry maps a step's `uses` value to its definition.
type Registry map[string]Def

// PlanStep is one fully-resolved step: every ${} reference that could be
// resolved from params (prior-step exports resolve later, during execute,
// since they don't exist yet at plan time) has been interpolated. The
// workflow Plan is the artifact --dry-run prints and never executes.
type PlanStep struct {
	Uses    string
	Repo    string
	Ref     string
	Globs   []string
	Skill   string
	Posture string
	Mode    string
	From    string
	To      string
	Cmd     string
	Args    []string
}
