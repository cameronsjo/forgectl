package cli

import (
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestProductionDeps_FillsEverySeam pins that the dependency set Execute hands
// to every module constructor is complete. A seam production forgets to fill
// is a nil interface whichever module first needs it discovers at its own call
// site, at which point the failure is far from the wiring that caused it.
func TestProductionDeps_FillsEverySeam(t *testing.T) {
	boundary := &config.LegacyMigrationBoundary{}
	deps := productionDeps(config.Config{}, boundary)

	// Enumerated by reflection rather than by name. A named list only pins the
	// seams someone remembered to add to it, and the failure this guards against
	// is precisely the one nobody remembered — a field added to Deps and never
	// wired into productionDeps. Reflection makes a new seam opt out explicitly
	// instead of being missed silently.
	v := reflect.ValueOf(deps)
	if v.NumField() == 0 {
		t.Fatal("Deps has no fields; the reflection below would pass vacuously")
	}
	for i := range v.NumField() {
		field, name := v.Field(i), v.Type().Field(i).Name
		switch field.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface,
			reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
			if field.IsNil() {
				t.Errorf("%s is nil; whichever module first needs it discovers that at its own call site, far from this wiring", name)
			}
		default:
			// A non-nil-able field (a string, an int, a value struct) has no
			// unfilled state to detect — its zero value may be legitimate.
		}
	}

	if deps.LegacyBoundary != boundary {
		t.Error("LegacyBoundary was not passed through")
	}

	// The two runners are deliberately different types: the sensitive seam is a
	// separate interface precisely so a caller cannot reach the argv-logging
	// runner by accident when it meant the bounded one.
	if _, ok := deps.SensitiveRunner.(exec.Runner); ok {
		t.Error("the sensitive runner also satisfies Runner; the seams must stay distinct")
	}
}
