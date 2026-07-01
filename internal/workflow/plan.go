package workflow

import (
	"fmt"
	"log/slog"
)

// PlanStep is one fully-resolved step: every ${} reference that could be
// resolved from params (prior-step exports resolve later, during execute,
// since they don't exist yet at plan time) has been interpolated. Plan is the
// artifact --dry-run prints and never executes.
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

// Plan is the ordered, resolved step sequence a workflow run will execute.
type Plan struct {
	Name    string
	Version string
	Steps   []PlanStep
}

// BuildPlan merges cliParams over each param's default, enforces required
// params, seeds a Context with the result, and interpolates every step field
// against it (ADR-0002's "resolve" stage). Fields that reference a
// not-yet-produced export (e.g. ${workspace} before the worktree step runs)
// are left uninterpolated here — Interpolate only sees param-derived
// variables until execute merges in each step's exports — so BuildPlan
// returns an error only for a genuinely unresolvable reference (unknown
// param name, missing required param).
func BuildPlan(wf Workflow, cliParams map[string]string) (Plan, error) {
	slog.Debug("Preparing to build plan.", "workflowName", wf.Name, "workflowVersion", wf.Version, "stepCount", len(wf.Steps))

	resolved, err := resolveParams(wf.Params, cliParams)
	if err != nil {
		slog.Error("Failed to resolve workflow params.", "workflowName", wf.Name, "error", err)
		return Plan{}, err
	}
	slog.Debug("Resolved workflow params.", "paramCount", len(resolved))

	ctx := NewContext(resolved)
	// Mark every variable a step exports as deferred, so a field that
	// references a not-yet-produced export (e.g. ${workspace} before the
	// worktree step runs) renders as the literal ${...} at plan time and is
	// resolved during execute. The export vocabulary comes from the same
	// registry the Executor runs, so a verb's exports live in one place.
	reg := defaultRegistry(nil) // nil: exports are glob-independent; runners are unused here
	for _, s := range wf.Steps {
		for _, exp := range reg[s.Uses].Exports {
			ctx.Defer(exp)
		}
	}

	steps := make([]PlanStep, 0, len(wf.Steps))
	for i, s := range wf.Steps {
		ps, err := planStep(ctx, s)
		if err != nil {
			slog.Error("Failed to plan step.", "workflowName", wf.Name, "stepIndex", i, "stepUse", s.Uses, "error", err)
			return Plan{}, fmt.Errorf("step %d (%s): %w", i, s.Uses, err)
		}
		steps = append(steps, ps)
	}

	slog.Debug("Successfully built plan.", "workflowName", wf.Name, "resolvedStepCount", len(steps))
	return Plan{Name: wf.Name, Version: wf.Version, Steps: steps}, nil
}

// resolveParams merges cliParams over each declared param's default and
// enforces required params (ADR-0002). A cliParam not declared in the
// workflow is passed through as-is — it becomes available to ${} references
// but isn't validated against a Param declaration.
func resolveParams(declared map[string]Param, cliParams map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(declared)+len(cliParams))
	for name, p := range declared {
		if v, ok := cliParams[name]; ok {
			out[name] = v
			continue
		}
		if p.Required {
			slog.Warn("Missing required param.", "param", name)
			return nil, fmt.Errorf("missing required param %q", name)
		}
		out[name] = p.Default
	}
	for name, v := range cliParams {
		if _, ok := declared[name]; !ok {
			out[name] = v
		}
	}
	return out, nil
}

// planStep interpolates every field of one Step against ctx.
func planStep(ctx *Context, s Step) (PlanStep, error) {
	var err error
	ps := PlanStep{Uses: s.Uses}

	if ps.Mode, err = ctx.Interpolate(s.Mode); err != nil {
		return PlanStep{}, err
	}
	if ps.Repo, err = ctx.Interpolate(s.Repo); err != nil {
		return PlanStep{}, err
	}
	if ps.Ref, err = ctx.Interpolate(s.Ref); err != nil {
		return PlanStep{}, err
	}
	if ps.Globs, err = ctx.InterpolateAll(s.Globs); err != nil {
		return PlanStep{}, err
	}
	if ps.Skill, err = ctx.Interpolate(s.Skill); err != nil {
		return PlanStep{}, err
	}
	if ps.Posture, err = ctx.Interpolate(s.Posture); err != nil {
		return PlanStep{}, err
	}
	if ps.From, err = ctx.Interpolate(s.From); err != nil {
		return PlanStep{}, err
	}
	if ps.To, err = ctx.Interpolate(s.To); err != nil {
		return PlanStep{}, err
	}
	if ps.Cmd, err = ctx.Interpolate(s.Cmd); err != nil {
		return PlanStep{}, err
	}
	if ps.Args, err = ctx.InterpolateAll(s.Args); err != nil {
		return PlanStep{}, err
	}
	return ps, nil
}
