package step

import (
	"fmt"
	"strings"
)

// Context is the shared variable table threaded through resolve/plan/execute:
// resolved params seed it, and each step's exports are merged in as they run
// so later steps can interpolate ${workspace}, ${review}, etc. (ADR-0002).
//
// A variable can also be *deferred*: a name a later step will export at execute
// time but that isn't known yet at plan time. A ${name} reference to a deferred
// variable interpolates to the literal ${name}, leaving it for the execute
// stage — distinct from an unknown variable, which is an error.
type Context struct {
	vars     map[string]string
	deferred map[string]bool
}

// NewContext builds a Context seeded with the given variables (typically the
// resolved params). A nil seed is treated as empty.
func NewContext(seed map[string]string) *Context {
	c := &Context{vars: make(map[string]string, len(seed)), deferred: make(map[string]bool)}
	for k, v := range seed {
		c.vars[k] = v
	}
	return c
}

// Set records a variable — used both to seed params and to merge a step's
// exports after it runs.
func (c *Context) Set(name, value string) {
	c.vars[name] = value
}

// Defer marks name as a variable a later step will export at execute time, so
// a plan-time ${name} reference passes through as the literal ${name} instead
// of erroring as unknown. This is how BuildPlan renders forward references
// (e.g. ${workspace} before the worktree step runs) without resolving them.
func (c *Context) Defer(name string) {
	c.deferred[name] = true
}

// Get returns a variable's value and whether it was set.
func (c *Context) Get(name string) (string, bool) {
	v, ok := c.vars[name]
	return v, ok
}

// nextRef locates the next ${name} in s at or after from. It returns the name,
// the index of the opening "${", the index just past the closing "}", and
// whether one was found. unterminated is true when a "${" has no closing "}" —
// the one case Interpolate treats as an error while Refs simply stops. It is the
// single boundary scanner both Interpolate and Refs share, so they can never
// disagree on what counts as a reference.
func nextRef(s string, from int) (name string, start, after int, ok, unterminated bool) {
	idx := strings.Index(s[from:], "${")
	if idx < 0 {
		return "", 0, 0, false, false
	}
	start = from + idx
	end := strings.Index(s[start:], "}")
	if end < 0 {
		return "", start, 0, false, true
	}
	end += start
	return s[start+2 : end], start, end + 1, true, false
}

// Interpolate resolves every ${var} reference in s against the Context. A
// reference to a deferred export passes through as the literal ${var}; any
// other unresolved variable is an error — referencing a param or export that
// was never set (or deferred) is a bug in the file, not something to silently
// pass through.
func (c *Context) Interpolate(s string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil // fast path: no interpolation, no builder allocation
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		name, start, after, ok, unterminated := nextRef(s, i)
		if unterminated {
			return "", fmt.Errorf("unterminated ${...} in %q", s)
		}
		if !ok {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i:start])
		switch val, has := c.Get(name); {
		case has:
			b.WriteString(val)
		case c.deferred[name]:
			b.WriteString("${" + name + "}")
		default:
			return "", fmt.Errorf("unknown variable ${%s} in %q", name, s)
		}
		i = after
	}
	return b.String(), nil
}

// Refs returns the names of every ${name} reference in s, using the same
// boundary scan as Interpolate. A malformed/unterminated reference is skipped —
// Interpolate is the authority that errors on those. It is a package function
// (no Context state needed) so the workflow engine's export-safety scan and
// interpolation share one parser.
func Refs(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		name, _, after, ok, unterminated := nextRef(s, i)
		if !ok || unterminated {
			break
		}
		out = append(out, name)
		i = after
	}
	return out
}

// InterpolateAll resolves ${} references across a slice of strings, e.g. a
// step's globs/args.
func (c *Context) InterpolateAll(ss []string) ([]string, error) {
	out := make([]string, len(ss))
	for i, s := range ss {
		v, err := c.Interpolate(s)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
