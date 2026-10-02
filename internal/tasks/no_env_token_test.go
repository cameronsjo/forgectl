package tasks

import (
	"os"
	"path/filepath"
	"slices"
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
//
// It reads every non-test source file in the package, not a list of names. A
// list covers the files that existed when it was written, and the file most
// likely to gain an environment read is the one added after.
func TestTokenHasNoEnvironmentSource(t *testing.T) {
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	var files []string
	for _, file := range all {
		if !strings.HasSuffix(file, "_test.go") {
			files = append(files, file)
		}
	}
	// A glob that matched nothing, or ran in the wrong directory, would pass
	// with zero files read. These are the files that hold or send the token.
	for _, must := range []string{
		"token.go", "client.go", "write.go", "mcp.go",
		"structured.go", "hostpin.go", "cache.go",
		"complete.go", "trailer.go",
	} {
		if !slices.Contains(files, must) {
			t.Errorf("%s is not among the sources this test read: %v", must, files)
		}
	}
	for _, file := range files {
		src, err := os.ReadFile(file) //nolint:gosec // G304: `file` is a name the glob above returned from the package directory, not external input
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
