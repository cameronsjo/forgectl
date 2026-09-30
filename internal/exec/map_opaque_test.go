package exec

import "testing"

// TestMapOpaque_RespellsOnceAndRefusesNonOpaque pins MapOpaque's two
// promises. An opaque argument comes back opaque, holding f of its payload,
// with f run exactly once however often the result is revealed. Anything
// else comes back as the zero Arg, which validate refuses, so re-spelling a
// constant fails loudly instead of passing it through.
//
// Mutations that turn it red: return a unchanged for a non-opaque input (the
// fixed and separator cases stay set), or return Arg{reveal: func() string {
// return f(a.reveal()) }, kind: argOpaque} (f runs on every reveal).
func TestMapOpaque_RespellsOnceAndRefusesNonOpaque(t *testing.T) {
	calls := 0
	got := MapOpaque(Opaque("/w/x#(y);"), func(s string) string {
		calls++
		return s + "!"
	})
	if !got.Secret() || !got.Equal(Opaque("/w/x#(y);!")) {
		t.Fatal("MapOpaque did not return an opaque argument holding f(payload)")
	}
	_ = got.reveal()
	_ = got.reveal()
	if calls != 1 {
		t.Fatalf("f ran %d times, want exactly once", calls)
	}

	for name, a := range map[string]Arg{
		"fixed":     MustFixed("-c"),
		"separator": EndOfOptions(),
		"zero":      {},
	} {
		if MapOpaque(a, func(s string) string { return s }).set() {
			t.Errorf("MapOpaque(%s) is set; want the zero Arg validate refuses", name)
		}
	}
}
