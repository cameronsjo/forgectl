package tasks

import (
	"os"
	"strings"
	"testing"
)

// TestTokenHasNoEnvironmentSource turns ADR 0009 §7's prose into a control.
//
// The ADR states that the token has exactly two sources — the macOS keychain
// and a mounted file — and that an environment variable must never become a
// third, because an env var is readable through `docker inspect`, through
// /proc/<pid>/environ, and in the rendered compose file on disk. Adding one
// would re-open both runtime readers, and it would work perfectly: nothing
// downstream would fail, no test would go red, and the regression would be
// invisible until someone read the file.
//
// A sentence in an ADR is not a control. This is.
func TestTokenHasNoEnvironmentSource(t *testing.T) {
	for _, file := range []string{"token.go", "client.go", "write.go", "mcp.go"} {
		src, err := os.ReadFile(file) //nolint:gosec // G304: `file` is a literal from the loop above, not external input
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, forbidden := range []string{"os.Getenv", "os.LookupEnv", "os.Environ"} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s calls %s — ADR 0009 §7: the token has two sources (keychain, mounted file) and an "+
					"environment variable must never become a third", file, forbidden)
			}
		}
	}
}
