package workflow

import "fmt"

// GuardedValues pulls the values of a step verb's guarded fields (a
// step.Def.GuardedFields list, named by their Go field name) off a parsed Step,
// returning them keyed by field name for the bless-time injection guard. A
// scalar field contributes one element; a slice field (Args, Globs) contributes
// all of them.
//
// This switch is the ONE place that knows a step's field names, so the registry
// and the guard agree on what "Cmd" means. An unknown name is a HARD ERROR, not
// a silent skip: a typo'd GuardedFields entry ("Glob" for "Globs") would
// otherwise quietly disable the guard on the very field it meant to protect.
func GuardedValues(s Step, fields []string) (map[string][]string, error) {
	out := make(map[string][]string, len(fields))
	for _, f := range fields {
		switch f {
		case "Repo":
			out[f] = []string{s.Repo}
		case "Ref":
			out[f] = []string{s.Ref}
		case "Globs":
			out[f] = s.Globs
		case "Skill":
			out[f] = []string{s.Skill}
		case "Posture":
			out[f] = []string{s.Posture}
		case "Mode":
			out[f] = []string{s.Mode}
		case "From":
			out[f] = []string{s.From}
		case "To":
			out[f] = []string{s.To}
		case "Cmd":
			out[f] = []string{s.Cmd}
		case "Args":
			out[f] = s.Args
		default:
			return nil, fmt.Errorf("step verb %q declares guarded field %q, which is not a workflow step field — fix the registry entry (a typo here would silently disable the param-injection guard)", s.Uses, f)
		}
	}
	return out, nil
}
