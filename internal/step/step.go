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

// Def is a step verb's definition: the runner that executes it and the
// variables it exports into the Context. Declaring both together keeps a
// verb's execution and its plan-time exports from drifting apart — adding a
// verb is one registry entry.
type Def struct {
	Runner  Runner
	Exports []string
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
