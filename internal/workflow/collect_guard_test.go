package workflow

import "testing"

// TestBuiltinRegistry_CollectGuardsOnlyTo pins the exact GuardedFields set the
// harden/blessing-param-guard change adds for collect. GuardedValues and
// TestGuardedValues_RegistryDefsNameRealFields only assert the declared names
// are REAL step fields — neither would catch a future edit that widens the
// set to include From (over-guarding a field that merely names data to read)
// or drops To (silently reopening the write-sink injection this branch closes).
func TestBuiltinRegistry_CollectGuardsOnlyTo(t *testing.T) {
	def := builtinRegistry()["collect"]
	want := []string{"To"}
	if len(def.GuardedFields) != len(want) {
		t.Fatalf("collect GuardedFields = %v, want exactly %v", def.GuardedFields, want)
	}
	for i, f := range want {
		if def.GuardedFields[i] != f {
			t.Errorf("collect GuardedFields[%d] = %q, want %q", i, def.GuardedFields[i], f)
		}
	}
}

// TestGuardedValues_CollectGuardsToNotFrom exercises the real registry entry
// end to end: a Step carrying both From and To populated must surface To (the
// write destination) as a guarded value while never surfacing From (the read
// source) at all — mirroring the registry comment in exec.go that collect's
// `from` merely names a data path to read.
func TestGuardedValues_CollectGuardsToNotFrom(t *testing.T) {
	def := builtinRegistry()["collect"]
	s := Step{Uses: "collect", From: "${source}", To: "${dest}"}

	got, err := GuardedValues(s, def.GuardedFields)
	if err != nil {
		t.Fatalf("GuardedValues: %v", err)
	}
	vals, ok := got["To"]
	if !ok || len(vals) != 1 || vals[0] != "${dest}" {
		t.Errorf(`GuardedValues result["To"] = %v, ok=%v, want ["${dest}"], true`, vals, ok)
	}
	if _, ok := got["From"]; ok {
		t.Error("From must not appear in the guarded values — collect's registry entry only guards To")
	}
}
