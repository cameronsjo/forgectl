// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// TestTasksReads_StalenessCeiling pins the #1289 ceiling for ls, ready and
// show, in text and --json: a cache older than 72h is refused (nothing on
// stdout, the STALE DATA notice and the refusal on stderr, exit 7) unless
// --allow-stale is passed; a younger cache is served with the notice.
func TestTasksReads_StalenessCeiling(t *testing.T) {
	isolateTasksConfigDir(t)
	_, runner := withFakeTasksBackend(t, fakeVikunjaHandler(t))
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	if _, stderr, err := runTasksCmd(t, deps, "ls"); err != nil || stderr != "" {
		t.Fatalf("seeding live read: err=%v stderr=%q", err, stderr)
	}
	cachePath, err := config.TasksCachePath()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := tasks.LoadCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	setAge := func(age time.Duration) {
		snap.FetchedAt = time.Now().Add(-age)
		if err := tasks.SaveCache(cachePath, snap); err != nil {
			t.Fatal(err)
		}
	}

	liveClient := newTasksClient
	t.Cleanup(func() { newTasksClient = liveClient })
	newTasksClient = func(context.Context, exec.Runner, string, tasks.Token) (*tasks.Client, error) {
		return nil, fmt.Errorf("%w: dial tcp 192.0.2.1:443: connect: no route to host", tasks.ErrUnreachable)
	}

	for _, format := range [][]string{{}, {"--json"}} {
		for _, verb := range [][]string{{"ls"}, {"ready"}, {"show", "1"}} {
			args := append(append([]string(nil), verb...), format...)
			name := strings.Join(args, " ")

			t.Run("refuse "+name, func(t *testing.T) {
				setAge(73 * time.Hour)
				stdout, stderr, err := runTasksCmd(t, deps, args...)
				if err == nil {
					t.Fatal("a 73h cache was served; want a refusal")
				}
				if got := ExitCode(err); got != exitTasksStale {
					t.Errorf("exit = %d, want %d", got, exitTasksStale)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
				for _, want := range []string{"STALE DATA", "no route to host", "73h", "refusing data older than 72h; pass --allow-stale to use it"} {
					if !strings.Contains(stderr+err.Error(), want) {
						t.Errorf("stderr+error %q lacks %q", stderr+err.Error(), want)
					}
				}
			})

			t.Run("allow-stale "+name, func(t *testing.T) {
				setAge(73 * time.Hour)
				stdout, stderr, err := runTasksCmd(t, deps, append(append([]string(nil), args...), "--allow-stale")...)
				if err != nil {
					t.Fatalf("--allow-stale: %v", err)
				}
				if !strings.HasPrefix(stderr, "forgectl: STALE DATA: ") || strings.Contains(stderr, "refusing") {
					t.Errorf("stderr %q: want the notice and no refusal", stderr)
				}
				if stdout == "" {
					t.Error("stdout empty, want the cached data")
				}
				if len(format) > 0 {
					var parsed any
					if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
						t.Errorf("stdout is not JSON: %v", err)
					}
				}
			})

			t.Run("under-ceiling "+name, func(t *testing.T) {
				setAge(71 * time.Hour)
				stdout, stderr, err := runTasksCmd(t, deps, args...)
				if err != nil {
					t.Fatalf("a 71h cache was refused: %v", err)
				}
				if !strings.HasPrefix(stderr, "forgectl: STALE DATA: ") || strings.Contains(stderr, "refusing") {
					t.Errorf("stderr %q: want the notice and no refusal", stderr)
				}
				if stdout == "" {
					t.Error("stdout empty, want the cached data")
				}
			})
		}
	}
}
