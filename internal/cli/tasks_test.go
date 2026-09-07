// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tasks"
	"github.com/cameronsjo/forgectl/internal/theme"
)

const tasksTestFakeToken = "tk_" + "cafebabecafebabecafebabecafebabecafebabe"

// withFakeTasksBackend points every tasks command at an httptest server for
// the duration of the test, via the newTasksClient seam, and returns a
// FakeRunner whose "security find-generic-password" branch answers with
// tasksTestFakeToken. Restores the seam on cleanup.
func withFakeTasksBackend(t *testing.T, handler http.Handler) (*httptest.Server, *exec.FakeRunner) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	orig := newTasksClient
	newTasksClient = func(_ context.Context, _ exec.Runner, _ string, token tasks.Token) (*tasks.Client, error) {
		return tasks.NewClientForTesting(srv.URL, token), nil
	}
	t.Cleanup(func() { newTasksClient = orig })

	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "security" {
				return tasksTestFakeToken, nil
			}
			return "", nil
		},
	}
	return srv, runner
}

func fakeVikunjaHandler(t *testing.T) http.Handler {
	t.Helper()
	tasksBody := []tasks.Task{
		{ID: 1, Title: "write the plan", Done: false, Position: 1},
		{ID: 2, Title: "ship it", Done: false, Position: 2, RelatedTasks: map[string][]tasks.Task{
			"blocked": {{ID: 1, Title: "write the plan", Done: false}},
		}},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" {
			_ = json.NewEncoder(w).Encode([]struct{}{})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_ = json.NewEncoder(w).Encode(tasksBody)
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode([]tasks.Project{{ID: 1, Title: "Inbox"}})
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_ = json.NewEncoder(w).Encode([]tasks.Label{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func runTasksCmd(t *testing.T, deps module.Deps, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newTasksCmd(deps)
	cmd.SetArgs(args)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	err = cmd.ExecuteContext(context.Background())
	return outBuf.String(), errBuf.String(), err
}

// captureProcessStd swaps the PROCESS's os.Stdout and os.Stderr for the
// duration of fn and returns whatever was written to them.
//
// This exists because runTasksCmd captures only cobra's OWN writers, and a
// leak does not have to go through them: a stray fmt.Println, a debug
// fmt.Fprintln(os.Stderr, …), or a panic trace all bypass the cobra buffers
// entirely. Measured 2026-09-07 — a deliberate `fmt.Fprintln(os.Stderr,
// token.Header())` injected into loadTasksSnapshot left the leak test GREEN,
// so the load-bearing assertion had a hole exactly the width of the most
// likely accident. The cobra-writer capture stays; this is added coverage,
// not a replacement.
func captureProcessStd(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	// Drain concurrently: a pipe's buffer is finite, so reading only after fn
	// returns would deadlock on output larger than it.
	outCh, errCh := make(chan string, 1), make(chan string, 1)
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, outR); outCh <- b.String() }()
	go func() { var b bytes.Buffer; _, _ = io.Copy(&b, errR); errCh <- b.String() }()

	func() {
		defer func() {
			os.Stdout, os.Stderr = origOut, origErr
			_ = outW.Close()
			_ = errW.Close()
		}()
		fn()
	}()
	return <-outCh, <-errCh
}

// TestTasksCommands_TokenNeverLeaks is the load-bearing test: a recognizable
// fake token is seeded at the keychain seam, every tasks command is run, and
// every output surface — cobra's stdout and stderr, the PROCESS's os.Stdout
// and os.Stderr, the returned error's string, and the on-disk cache file —
// is grepped for the literal. None may contain it.
func TestTasksCommands_TokenNeverLeaks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, runner := withFakeTasksBackend(t, fakeVikunjaHandler(t))
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	var surfaces []string
	procOut, procErr := captureProcessStd(t, func() {
		for _, args := range [][]string{{"ls"}, {"ls", "--json"}, {"show", "1"}, {"show", "1", "--json"}, {"ready"}, {"ready", "--json"}} {
			stdout, stderr, err := runTasksCmd(t, deps, args...)
			surfaces = append(surfaces, stdout, stderr)
			if err != nil {
				surfaces = append(surfaces, err.Error())
			}
		}
	})
	surfaces = append(surfaces, procOut, procErr)

	cachePath, pathErr := config.TasksCachePath()
	if pathErr != nil {
		t.Fatalf("config.TasksCachePath: %v", pathErr)
	}
	cacheBytes, readErr := os.ReadFile(cachePath) //nolint:gosec // fixed test-owned path
	if readErr != nil {
		t.Fatalf("expected a cache file at %s after a successful fetch: %v", cachePath, readErr)
	}
	surfaces = append(surfaces, string(cacheBytes))

	for i, s := range surfaces {
		if strings.Contains(s, tasksTestFakeToken) {
			t.Fatalf("surface %d contains the token literal:\n%s", i, s)
		}
	}
}

func TestTasksLs_Unauthorized_DoesNotFallBackToCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed a cache file so a wrongful fallback would be observable.
	cachePath, err := config.TasksCachePath()
	if err != nil {
		t.Fatalf("config.TasksCachePath: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(cachePath), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	stale := tasks.Snapshot{Tasks: []tasks.Task{{ID: 99, Title: "stale cached task"}}}
	if saveErr := tasks.SaveCache(cachePath, stale); saveErr != nil {
		t.Fatal(saveErr)
	}

	_, runner := withFakeTasksBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token invalid"}`))
	}))
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	stdout, _, err := runTasksCmd(t, deps, "ls")
	if err == nil {
		t.Fatal("ls with a rejected token = nil error, want a failure")
	}
	if ExitCode(err) != exitTasksUnauthorized {
		t.Fatalf("ExitCode = %d, want %d (unauthorized)", ExitCode(err), exitTasksUnauthorized)
	}
	if strings.Contains(stdout, "stale cached task") {
		t.Fatal("an unauthorized token must never fall back to cached data")
	}
}

func TestTasksLs_Unreachable_FallsBackToCacheWithAgeStated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cachePath, err := config.TasksCachePath()
	if err != nil {
		t.Fatalf("config.TasksCachePath: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(cachePath), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	stale := tasks.Snapshot{Tasks: []tasks.Task{{ID: 7, Title: "cached task", Position: 1}}}
	if saveErr := tasks.SaveCache(cachePath, stale); saveErr != nil {
		t.Fatal(saveErr)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // connection refused for every request

	orig := newTasksClient
	newTasksClient = func(_ context.Context, _ exec.Runner, _ string, token tasks.Token) (*tasks.Client, error) {
		return tasks.NewClientForTesting(addr, token), nil
	}
	t.Cleanup(func() { newTasksClient = orig })

	runner := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "security" {
			return tasksTestFakeToken, nil
		}
		return "", nil
	}}
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	stdout, stderr, err := runTasksCmd(t, deps, "ls")
	if err != nil {
		t.Fatalf("ls on a network failure with a cache present = %v, want nil (served from cache)", err)
	}
	if !strings.Contains(stdout, "cached task") {
		t.Fatalf("stdout = %q, want the cached task to appear", stdout)
	}
	if !strings.Contains(stderr, "old") {
		t.Fatalf("stderr = %q, want the cache-fallback notice stating the cache's age", stderr)
	}
}

func TestTasksReady_ExcludesActiveBlocker_EndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, runner := withFakeTasksBackend(t, fakeVikunjaHandler(t))
	deps := module.Deps{Runner: runner, Theme: theme.Default()}

	stdout, _, err := runTasksCmd(t, deps, "ready")
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if strings.Contains(stdout, "ship it") {
		t.Fatalf("ready output contains a task with an active blocker: %q", stdout)
	}
	if !strings.Contains(stdout, "write the plan") {
		t.Fatalf("ready output missing the unblocked task: %q", stdout)
	}
}

// TestTasksLs_HostRefused_DoesNotFallBackToCacheAndExitsFour is the pin for
// a fail-quiet the SECOND security pass found in the FIRST pass's fix.
//
// The host-pin refusal reaches Client.get as the dialer's error. Wrapping it
// into ErrUnreachable there (with %v, which destroys the sentinel) sent it
// down the cache-fallback path — which returns a nil error, so the command
// printed "serving cached data" and exited 0. A refusal to send the
// credential presented as a successful run: strictly worse than losing the
// exit code, and invisible.
func TestTasksLs_HostRefused_DoesNotFallBackToCacheAndExitsFour(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed a cache so a wrongful fallback is observable rather than inferred.
	cachePath, err := config.TasksCachePath()
	if err != nil {
		t.Fatalf("config.TasksCachePath: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(cachePath), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	stale := tasks.Snapshot{Tasks: []tasks.Task{{ID: 99, Title: "stale cached task"}}}
	if saveErr := tasks.SaveCache(cachePath, stale); saveErr != nil {
		t.Fatal(saveErr)
	}

	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "security" {
				return tasksTestFakeToken, nil
			}
			return "", nil
		},
	}
	orig := newTasksClient
	newTasksClient = func(_ context.Context, _ exec.Runner, _ string, _ tasks.Token) (*tasks.Client, error) {
		return nil, fmt.Errorf("%w: test refusal", tasks.ErrHostRefused)
	}
	t.Cleanup(func() { newTasksClient = orig })

	deps := module.Deps{Runner: runner, Theme: theme.Default()}
	stdout, _, err := runTasksCmd(t, deps, "ls")
	if err == nil {
		t.Fatal("ls with a refused host = nil error, want a failure — a pin refusal must never exit 0")
	}
	if strings.Contains(stdout, "stale cached task") {
		t.Fatal("a host-pin refusal must never fall back to cached data")
	}
	if got := ExitCode(err); got != exitTasksHostRefused {
		t.Fatalf("ExitCode = %d, want %d (host refused)", got, exitTasksHostRefused)
	}
}
