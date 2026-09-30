package exec

import "testing"

// TestMapOpaque_RespellsOnceAndRefusesNonOpaque pins MapOpaque's promises. An
// opaque argument comes back opaque, holding the transform of its payload,
// with the transform run exactly once however often the result is revealed.
// Anything else, or the zero Transform, comes back as the zero Arg, which
// validate refuses, so re-spelling a constant fails loudly.
//
// Mutations that turn it red: return a unchanged for a non-opaque input (the
// fixed and separator cases stay set), or return Arg{reveal: func() string {
// return t.apply(a.reveal()) }, kind: argOpaque} (the transform runs on every
// reveal).
func TestMapOpaque_RespellsOnceAndRefusesNonOpaque(t *testing.T) {
	calls := 0
	counting := Transform{apply: func(s string) string {
		calls++
		return s + "!"
	}}
	got := MapOpaque(Opaque("/w/x#(y);"), counting)
	if !got.Secret() || !got.Equal(Opaque("/w/x#(y);!")) {
		t.Fatal("MapOpaque did not return an opaque argument holding the transformed payload")
	}
	_ = got.reveal()
	_ = got.reveal()
	if calls != 1 {
		t.Fatalf("the transform ran %d times, want exactly once", calls)
	}

	for name, a := range map[string]Arg{
		"fixed":     MustFixed("-c"),
		"separator": EndOfOptions(),
		"zero":      {},
	} {
		if MapOpaque(a, counting).set() {
			t.Errorf("MapOpaque(%s) is set; want the zero Arg validate refuses", name)
		}
	}
	if MapOpaque(Opaque("x"), Transform{}).set() {
		t.Error("MapOpaque with the zero Transform is set; want the zero Arg validate refuses")
	}
}

// TestTmuxDirOperandTransform pins the one shipped Transform to the shared
// tmux -c spelling.
func TestTmuxDirOperandTransform(t *testing.T) {
	if !MapOpaque(Opaque("/a#(b);"), TmuxDirOperand()).Equal(Opaque(`/a##(b)\;`)) {
		t.Fatal("TmuxDirOperand did not apply the tmux -c escape")
	}
}
