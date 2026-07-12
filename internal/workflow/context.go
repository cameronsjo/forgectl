package workflow

import "github.com/cameronsjo/forgectl/internal/step"

// The step contract (Context, PlanStep, runner/def/registry shapes) moved to
// internal/step so domain modules can contribute step verbs without importing
// this package (ADR-0005). The old names stay as aliases — every existing
// reference (engine internals, tests, the CLI layer) keeps compiling against
// the workflow package.

// Context is the shared variable table threaded through resolve/plan/execute.
type Context = step.Context

// PlanStep is one fully-resolved step of a Plan.
type PlanStep = step.PlanStep

// StepRunner executes one resolved PlanStep. New code should use step.Runner.
type StepRunner = step.Runner

// StepDef is a step verb's definition. New code should use step.Def.
type StepDef = step.Def

// StepRegistry maps a step's `uses` value to its definition. New code should
// use step.Registry.
type StepRegistry = step.Registry

// ErrNotYetWired is returned by a registered-but-unimplemented step runner.
var ErrNotYetWired = step.ErrNotYetWired

// NewContext builds a Context seeded with the given variables (typically the
// resolved params). A nil seed is treated as empty.
func NewContext(seed map[string]string) *Context {
	return step.NewContext(seed)
}
