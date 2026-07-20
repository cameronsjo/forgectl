package workflow

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Plan is the ordered, resolved step sequence a workflow run will execute.
// Its PlanStep elements are the neutral step contract's type (internal/step),
// aliased into this package — see context.go's alias block.
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
//
// registry is the MERGED vocabulary from NewRegistry — the same one the
// Executor runs — so a module-contributed verb's exports (launch's
// ${review}) defer at plan time exactly like a builtin's (ADR-0005).
func BuildPlan(wf Workflow, cliParams map[string]string, registry StepRegistry) (Plan, error) {
	slog.Debug("Preparing to build plan.", "workflowName", wf.Name, "workflowVersion", wf.Version, "stepCount", len(wf.Steps))

	resolved, err := resolveParams(wf.Params, cliParams)
	if err != nil {
		slog.Error("Failed to resolve workflow params.", "workflowName", wf.Name, "error", err)
		return Plan{}, err
	}
	slog.Debug("Resolved workflow params.", "paramCount", len(resolved))

	// Params and exports share ONE variable namespace at execution time (the
	// Context), and an export only overwrites its name if its step actually
	// Sets it — so a param named after an export could survive into later
	// steps if an exporting step ever succeeded without setting it. That would
	// let a CLI-supplied value ride a name the bless-time injection guard
	// (#10) trusts as step-produced. Refuse the collision outright.
	for i, s := range wf.Steps {
		for _, exp := range registry[s.Uses].Exports {
			if _, isParam := wf.Params[exp]; isParam {
				slog.Error("Param name collides with a step export.", "workflowName", wf.Name, "param", exp, "stepIndex", i, "stepUse", s.Uses)
				return Plan{}, fmt.Errorf("param %q collides with the %q export of step %d (%s): params and step exports share one namespace", exp, exp, i, s.Uses)
			}
		}
	}

	ctx := NewContext(resolved)
	// Mark every variable a step exports as deferred, so a field that
	// references a not-yet-produced export (e.g. ${workspace} before the
	// worktree step runs) renders as the literal ${...} at plan time and is
	// resolved during execute. The export vocabulary comes from the same
	// merged registry the Executor runs, so a verb's exports live in one place.
	for _, s := range wf.Steps {
		for _, exp := range registry[s.Uses].Exports {
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
// enforces required params (ADR-0002). An undeclared --param is REJECTED rather
// than passed through: nothing should silently accept a param the workflow never
// declared, and (with blessing, #10) an unchecked passthrough would be an
// agent-controllable injection into a blessed run step's ${} references. Unknown
// names are sorted so the error message is deterministic when several are given.
func resolveParams(declared map[string]Param, cliParams map[string]string) (map[string]string, error) {
	var unknown []string
	for name := range cliParams {
		if _, ok := declared[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		slog.Debug("Rejecting undeclared params.", "params", unknown)
		quoted := make([]string, len(unknown))
		for i, name := range unknown {
			quoted[i] = fmt.Sprintf("%q", name)
		}
		return nil, fmt.Errorf("unknown param %s: not declared by this workflow", strings.Join(quoted, ", "))
	}

	out := make(map[string]string, len(declared))
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
	return out, nil
}

// planStep interpolates every field of one Step against ctx.
func planStep(ctx *Context, s Step) (PlanStep, error) {
	return interpolatePlanStep(ctx, PlanStep{
		Uses:    s.Uses,
		Repo:    s.Repo,
		Ref:     s.Ref,
		Globs:   s.Globs,
		Skill:   s.Skill,
		Posture: s.Posture,
		Mode:    s.Mode,
		From:    s.From,
		To:      s.To,
		Cmd:     s.Cmd,
		Args:    s.Args,
	})
}

// interpolatePlanStep resolves every ${} reference in a step's fields against
// ctx. It runs twice per step: once at plan time (where a deferred export
// passes through as the literal ${name}), and again in Executor.Run against
// the live Context just before dispatch — where nothing is deferred, so a
// reference to an export whose step hasn't run yet fails loudly instead of
// reaching a command as the literal string "${name}".
func interpolatePlanStep(ctx *Context, in PlanStep) (PlanStep, error) {
	// Copy the struct, then interpolate every field through the SHARED field
	// enumeration (step.PlanStep.Interpolate). Slice fields are replaced with
	// fresh slices, so in's backing arrays are never mutated. Uses is not
	// interpolated. Keeping the field list in one place means hashing and the
	// export scan can never drift from what actually gets interpolated.
	ps := in
	if err := ps.Interpolate(ctx.Interpolate); err != nil {
		return PlanStep{}, err
	}
	return ps, nil
}
