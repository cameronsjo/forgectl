package exec

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/tmux/tmuxesc"
)

// TestMapOpaque_RespellsAndRefusesNonOpaque pins MapOpaque's promises. An
// opaque argument comes back opaque, holding the transform of its payload.
// Anything else, or the zero Transform, comes back as the zero Arg, which
// validate refuses, so re-spelling a constant fails loudly. That the
// transform runs exactly once however often the result is revealed is pinned
// in sealed's own tests (TestMap_RunsTheTransformOnce), the only package that
// can mint a counting Transform.
//
// Mutations that turn it red: drop the argOpaque check (the fixed and
// separator cases stay set); return Arg{v: a.v, kind: argOpaque} (the payload
// is not re-spelled).
func TestMapOpaque_RespellsAndRefusesNonOpaque(t *testing.T) {
	const payload = "/w/x#(y);"
	got := MapOpaque(Opaque(payload), TmuxDirOperand())
	if !got.Secret() || !got.Equal(Opaque(tmuxesc.DirOperand(payload))) {
		t.Fatal("MapOpaque did not return an opaque argument holding the transformed payload")
	}
	if got.Equal(Opaque(payload)) {
		t.Fatal("MapOpaque returned the payload unchanged")
	}

	for name, a := range map[string]Arg{
		"fixed":     MustFixed("-c"),
		"separator": EndOfOptions(),
		"zero":      {},
	} {
		if MapOpaque(a, TmuxDirOperand()).set() {
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
