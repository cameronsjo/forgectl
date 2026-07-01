package workflow

// Test Plan for internal/workflow/context.go
//
// Context.Interpolate (Classification: pure logic)
//   [x] Happy: no "${" in input returns the input unchanged (fast path)
//   [x] Happy: a resolved variable substitutes its value
//   [x] Happy: a deferred variable passes through as the literal ${name}
//   [x] Unhappy: a genuinely-unknown variable (not set, not deferred) errors
//   [x] Boundary: two adjacent references ${a}${b} both resolve
//   [x] Boundary: an unterminated "${" (no closing "}") errors
//
// Context.InterpolateAll (Classification: pure logic)
//   [x] Happy: resolves every element of a slice
//   [x] Unhappy: propagates the first element's interpolation error
//
// Context.Defer / Get / Set (Classification: pure logic)
//   [x] Happy: Set then Get round-trips a value
//   [x] Happy: Defer marks a name so Interpolate treats it as pass-through, not error

import "testing"

func TestContext_Interpolate_NoPlaceholder_ReturnsInputUnchanged(t *testing.T) {
	ctx := NewContext(nil)
	in := "no interpolation needed here"
	got, err := ctx.Interpolate(in)
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if got != in {
		t.Errorf("Interpolate(%q) = %q, want unchanged", in, got)
	}
}

func TestContext_Interpolate_ResolvedVariable_Substitutes(t *testing.T) {
	ctx := NewContext(map[string]string{"repo": "cameronsjo/forgectl"})
	got, err := ctx.Interpolate("clone ${repo} now")
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if want := "clone cameronsjo/forgectl now"; got != want {
		t.Errorf("Interpolate = %q, want %q", got, want)
	}
}

func TestContext_Interpolate_DeferredVariable_PassesThroughAsLiteral(t *testing.T) {
	ctx := NewContext(nil)
	ctx.Defer("workspace")

	got, err := ctx.Interpolate("${workspace}/review.md")
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if want := "${workspace}/review.md"; got != want {
		t.Errorf("Interpolate = %q, want literal %q (deferred, not yet exported)", got, want)
	}
}

func TestContext_Interpolate_UnknownVariable_Errors(t *testing.T) {
	ctx := NewContext(nil)
	_, err := ctx.Interpolate("${nope}")
	if err == nil {
		t.Fatal("expected an error for an unset, non-deferred variable")
	}
}

func TestContext_Interpolate_AdjacentReferences_BothResolve(t *testing.T) {
	ctx := NewContext(map[string]string{"a": "AAA", "b": "BBB"})
	got, err := ctx.Interpolate("${a}${b}")
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if want := "AAABBB"; got != want {
		t.Errorf("Interpolate(${a}${b}) = %q, want %q", got, want)
	}
}

func TestContext_Interpolate_UnterminatedPlaceholder_Errors(t *testing.T) {
	ctx := NewContext(map[string]string{"a": "AAA"})
	_, err := ctx.Interpolate("prefix ${a is missing its close")
	if err == nil {
		t.Fatal("expected an error for an unterminated ${...}")
	}
}

func TestContext_InterpolateAll_ResolvesEveryElement(t *testing.T) {
	ctx := NewContext(map[string]string{"who": "world"})
	got, err := ctx.InterpolateAll([]string{"hello", "${who}", "literal"})
	if err != nil {
		t.Fatalf("InterpolateAll: %v", err)
	}
	want := []string{"hello", "world", "literal"}
	if len(got) != len(want) {
		t.Fatalf("InterpolateAll = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("InterpolateAll[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestContext_InterpolateAll_PropagatesElementError(t *testing.T) {
	ctx := NewContext(nil)
	_, err := ctx.InterpolateAll([]string{"ok", "${nope}"})
	if err == nil {
		t.Fatal("expected InterpolateAll to propagate the unknown-variable error")
	}
}

func TestContext_SetGet_RoundTrips(t *testing.T) {
	ctx := NewContext(nil)
	ctx.Set("workspace", "/tmp/sandbox")

	got, ok := ctx.Get("workspace")
	if !ok {
		t.Fatal("Get: expected ok=true after Set")
	}
	if got != "/tmp/sandbox" {
		t.Errorf("Get = %q, want /tmp/sandbox", got)
	}

	if _, ok := ctx.Get("never-set"); ok {
		t.Error("Get: expected ok=false for a variable never Set")
	}
}
