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
	// cmd/args), WHAT A LAUNCHED AGENT DOES (launch's skill/mode/posture),
	// WHAT A SECURITY CONTROL COVERS (strip's globs — the clean-room redaction
	// list), or WHERE OUTPUT IS WRITTEN (collect's to — the write-destination /
	// path sink that chooses where produced bytes land), it is guarded. A field
	// that merely names DATA to READ (worktree's repo/ref) is NOT guarded:
	// `--param repo=owner/x` is the intended parameterization, and the sandbox
	// plus the strip-list — not the blessing — are what contain whatever that
	// repo holds.
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

// scalarFieldPtrs returns pointers to every interpolable SCALAR string field, in
// a fixed order. Uses is deliberately excluded — it is the verb selector, never
// ${}-interpolated. This is the single enumeration the three consumers of a
// step's fields share (plan-time interpolation, input hashing, and the
// export-safety scan), so a field added to the struct but forgotten in one place
// can't silently drop out of the others.
func (s *PlanStep) scalarFieldPtrs() []*string {
	return []*string{&s.Repo, &s.Ref, &s.Skill, &s.Posture, &s.Mode, &s.From, &s.To, &s.Cmd}
}

// sliceFieldPtrs returns pointers to every interpolable SLICE field, in a fixed
// order — the slice counterpart of scalarFieldPtrs.
func (s *PlanStep) sliceFieldPtrs() []*[]string {
	return []*[]string{&s.Globs, &s.Args}
}

// ScalarFields returns the values of every interpolable scalar field, in the
// shared fixed order. Read-only consumers (hashing, the export scan) use this.
func (s PlanStep) ScalarFields() []string {
	ptrs := (&s).scalarFieldPtrs()
	out := make([]string, len(ptrs))
	for i, p := range ptrs {
		out[i] = *p
	}
	return out
}

// SliceFields returns the values of every interpolable slice field, in the
// shared fixed order.
func (s PlanStep) SliceFields() [][]string {
	ptrs := (&s).sliceFieldPtrs()
	out := make([][]string, len(ptrs))
	for i, p := range ptrs {
		out[i] = *p
	}
	return out
}

// Interpolate resolves every interpolable field of the step in place against
// interp (a per-field string transform, e.g. Context.Interpolate). Uses is left
// untouched. It walks the shared field enumeration, so interpolation, hashing,
// and the export scan can never disagree on which fields carry ${} references.
func (s *PlanStep) Interpolate(interp func(string) (string, error)) error {
	for _, p := range s.scalarFieldPtrs() {
		v, err := interp(*p)
		if err != nil {
			return err
		}
		*p = v
	}
	for _, p := range s.sliceFieldPtrs() {
		in := *p
		if len(in) == 0 {
			continue
		}
		out := make([]string, len(in))
		for i, v := range in {
			r, err := interp(v)
			if err != nil {
				return err
			}
			out[i] = r
		}
		*p = out
	}
	return nil
}
