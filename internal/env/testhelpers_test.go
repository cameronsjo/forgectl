package env

import (
	"strings"
	"testing"
)

// assertNoSecretInOutput fails the test if sentinel appears anywhere in
// outputs. except, when non-empty, is the one output allowed to contain it
// — typically a Document's own Bytes() (the raw file content necessarily
// carries the value), as opposed to Redacted() output or an error string,
// neither of which should ever carry a value. See the plan's "Logging
// discipline" section: this is the per-package value-bearing-test guard,
// scoped here to Document/Locate/write's return values and error strings
// (no cobra harness or slog capture exists yet in this package — those land
// with the CLI surface commit).
func assertNoSecretInOutput(t *testing.T, sentinel, except string, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		if out == except {
			continue
		}
		if strings.Contains(out, sentinel) {
			t.Fatalf("output leaked sentinel value %q: %q", sentinel, out)
		}
	}
}
