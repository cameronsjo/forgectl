// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// TestTasksLs_AFailedCacheWriteIsReportedOnStderr: the command succeeds, but a
// cache that could not be written means the next network outage has no
// fallback. Under the default config the global logger discards everything,
// so a warning sent only through it would be no warning at all.
func TestTasksLs_AFailedCacheWriteIsReportedOnStderr(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	closer := config.SetupLogger(config.Config{}) // the default: log_level unset
	t.Cleanup(func() { _ = closer.Close() })

	isolateTasksConfigDir(t)
	cachePath, err := config.TasksCachePath()
	if err != nil {
		t.Fatalf("config.TasksCachePath: %v", err)
	}
	// A non-empty directory where the cache file belongs: nothing can be
	// renamed over it, so the save fails after the fetch succeeded.
	if err := os.MkdirAll(filepath.Join(cachePath, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, runner := withFakeTasksBackend(t, fakeVikunjaHandler(t))
	deps := module.Deps{Runner: runner, Theme: theme.Default()}
	stdout, stderr, err := runTasksCmd(t, deps, "ls")
	if err != nil {
		t.Fatalf("ls with an unwritable cache = %v, want success: the data is in hand", err)
	}
	if !strings.Contains(stdout, "write the plan") {
		t.Fatalf("stdout = %q, want the fetched tasks", stdout)
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "cache") || !strings.Contains(lines[0], "could not be written") {
		t.Fatalf("stderr = %q, want one plain line saying the cache could not be written", stderr)
	}
	if strings.Contains(stderr, tasksTestFakeToken) {
		t.Fatalf("stderr carries the token: %q", stderr)
	}
}

// TestTasksLs_AWrittenCacheSaysNothing: the line is for a failure only.
func TestTasksLs_AWrittenCacheSaysNothing(t *testing.T) {
	isolateTasksConfigDir(t)
	_, runner := withFakeTasksBackend(t, fakeVikunjaHandler(t))
	deps := module.Deps{Runner: runner, Theme: theme.Default()}
	_, stderr, err := runTasksCmd(t, deps, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q after a cache that was written, want nothing", stderr)
	}
}
