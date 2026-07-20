package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// FuzzLoadState feeds arbitrary bytes as the run-state sidecar's on-disk
// contents. The sidecar (internal/workflow/state.go) is the one piece of
// workflow-run state an agent can write without ever touching the blessing
// ceremony — ADR-0007 refuses to rehydrate anything sensitive from it for
// exactly that reason. The invariant under fuzz is not "never errors"
// (malformed TOML, or a schema this binary doesn't understand, SHOULD error) —
// it is "never panics": LoadState must always return either a parsed RunState
// or a non-nil error, however hostile the bytes on disk.
//
// Each fuzz worker runs in its own process (go's native fuzzing forks worker
// processes rather than goroutines), so the shared HOME/XDG_CONFIG_HOME
// redirection and the single fuzz.state.toml path below are per-process and
// don't race across workers.
func FuzzLoadState(f *testing.F) {
	tmp := f.TempDir()
	origHome, hadHome := os.LookupEnv("HOME")
	origXDG, hadXDG := os.LookupEnv("XDG_CONFIG_HOME")
	if err := os.Setenv("HOME", tmp); err != nil {
		f.Fatalf("setenv HOME: %v", err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", tmp); err != nil {
		f.Fatalf("setenv XDG_CONFIG_HOME: %v", err)
	}
	f.Cleanup(func() {
		if hadHome {
			os.Setenv("HOME", origHome) //nolint:errcheck
		} else {
			os.Unsetenv("HOME") //nolint:errcheck
		}
		if hadXDG {
			os.Setenv("XDG_CONFIG_HOME", origXDG) //nolint:errcheck
		} else {
			os.Unsetenv("XDG_CONFIG_HOME") //nolint:errcheck
		}
	})

	dir, err := config.WorkflowStateDir()
	if err != nil {
		f.Fatalf("WorkflowStateDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.Fatalf("mkdir state dir: %v", err)
	}
	path := filepath.Join(dir, "fuzz.state.toml")

	f.Add([]byte(""))
	f.Add([]byte("schema = 1\nworkflow = \"fuzz\"\n"))
	f.Add([]byte("schema = 99999999999999\n"))
	f.Add([]byte("schema = -1\n"))
	f.Add([]byte(`schema = "not an int"`))
	f.Add([]byte("[[step]]\nindex = 1\n"))
	f.Add([]byte("\x00\x01\x02binary garbage\xff\xfe"))
	f.Add([]byte("schema = 1\n[[step]]\n[[step]]\nindex = 999999999999\n"))
	f.Add([]byte(strings.Repeat("a", 1<<16)))

	f.Fuzz(func(t *testing.T, data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write fuzz state bytes: %v", err)
		}
		// The assertion is survival: any error is acceptable, a panic is not.
		_, _, _ = LoadState("fuzz")
	})
}
