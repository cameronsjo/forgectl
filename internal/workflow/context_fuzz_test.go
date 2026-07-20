package workflow

import (
	"strings"
	"testing"
)

// FuzzRefsInterpolateAgreement pins the contract nextRef exists to guarantee
// (internal/step/context.go): Refs and Interpolate share one boundary scanner,
// so they can never disagree about where a ${name} reference starts and ends.
// The property under fuzz: for any string s, pre-setting every name Refs(s)
// finds as a Context variable must make Interpolate(s) succeed — UNLESS s
// contains an unterminated "${" (no closing "}"), which both funcs stop at,
// Refs silently and Interpolate with an error. If they ever disagree — Refs
// finds a name Interpolate doesn't (or vice versa) — Interpolate fails with
// "unknown variable" instead of "unterminated", which this test catches.
func FuzzRefsInterpolateAgreement(f *testing.F) {
	seeds := []string{
		"",
		"no refs here",
		"${a}",
		"${a}${b}",
		"prefix${a}suffix${b}suffix",
		"${a}${a}",
		"${}",
		"${unterminated",
		"a${",
		"$}{a}",
		"${a}${unterminated",
		"${a}}",
		"${{a}}",
		"$${a}",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		refs := Refs(s)

		ctx := NewContext(nil)
		for _, name := range refs {
			ctx.Set(name, "x")
		}

		if _, err := ctx.Interpolate(s); err != nil {
			if !strings.Contains(err.Error(), "unterminated") {
				t.Fatalf("Refs/Interpolate disagreed on %q: Refs=%v, Interpolate error=%v (want only an \"unterminated\" error once every Refs name is set)", s, refs, err)
			}
		}
	})
}
